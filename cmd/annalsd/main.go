// Command annalsd is Annals server.
package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"syscall"
	"time"
	// The board shows times in TZ, so the image carries the zone database.
	_ "time/tzdata"

	"github.com/orchard-suite/platformkit/v3/change"
	"github.com/orchard-suite/platformkit/v3/telemetry"

	"github.com/orchard-suite/annals/internal/directory"
	"github.com/orchard-suite/annals/internal/gate"
	"github.com/orchard-suite/annals/internal/graph"
	"github.com/orchard-suite/annals/internal/metrics"
	"github.com/orchard-suite/annals/internal/retention"
	"github.com/orchard-suite/annals/internal/selfaudit"
	"github.com/orchard-suite/annals/internal/server"
	"github.com/orchard-suite/annals/internal/store"
)

// version is set at build time from VERSION.
var version = "dev"

func main() {
	log := telemetry.NewLogger(os.Stdout, metrics.Service, version, telemetry.ParseLevel(os.Getenv("LOG_LEVEL")))
	if err := run(log); err != nil {
		log.Error("annals stopped", "error", err)
		os.Exit(1)
	}
}

func run(log *slog.Logger) error {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	policy, err := retention.Parse(env("RETENTION_DEFAULT", "365d"), os.Getenv("RETENTION_SOURCES"))
	if err != nil {
		return err
	}
	gateSettings, err := gateSettings()
	if err != nil {
		return err
	}
	cfg := server.Config{
		PublicURL:  os.Getenv("PUBLIC_URL"),
		ReaderRole: env("READER_ROLE", "Reader"),
		PIIRole:    env("PII_ROLE", "PiiReader"),
		AdminRole:  env("ADMIN_ROLE", "Admin"),
		FaroURL:    os.Getenv("FARO_URL"),
		FaroKey:    os.Getenv("FARO_API_KEY"),
		Retention:  policy,
		AreaWords:  strings.Split(os.Getenv("AREA_WORDS"), ","),
		Gate:       gateSettings,
		Version:    version,
	}
	dsn := os.Getenv("CLICKHOUSE_URL")
	if dsn == "" {
		return errors.New("CLICKHOUSE_URL is required, such as clickhouse://annals@clickhouse:9000/annals")
	}

	shutdownTelemetry, err := telemetry.Setup(ctx, metrics.Service, version)
	if err != nil {
		return err
	}
	st, err := store.Open(ctx, dsn, os.Getenv("CLICKHOUSE_PASSWORD"))
	if err != nil {
		return err
	}
	defer st.Close()
	var srv *server.Server
	m, err := metrics.New(func() metrics.Gauges { return srv.Gauges() })
	if err != nil {
		return err
	}
	audit := selfaudit.New(st, m, log)
	defer audit.Wait()
	srv, err = server.New(cfg, st, m, audit, log)
	if err != nil {
		return err
	}
	go retention.Run(ctx, st, policy, time.Hour, m, audit, log)
	if err := syncDirectory(ctx, st, m, log); err != nil {
		return err
	}

	listeners := []struct {
		name, addr string
		handler    http.Handler
	}{
		{"session", env("SESSION_ADDR", ":8080"), srv.Session()},
		{"api", env("API_ADDR", ":8081"), srv.API()},
	}
	errs := make(chan error, len(listeners))
	servers := make([]*http.Server, 0, len(listeners))
	for _, l := range listeners {
		hs := &http.Server{
			Addr:              l.addr,
			Handler:           l.handler,
			ReadHeaderTimeout: 10 * time.Second,
			IdleTimeout:       2 * time.Minute,
		}
		servers = append(servers, hs)
		go func() {
			if err := hs.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
				errs <- err
			}
		}()
	}
	log.Info("annals started", "public_url", cfg.PublicURL, "retention", strings.Join(policy.Describe(), ", "))

	select {
	case <-ctx.Done():
	case err = <-errs:
	}
	log.Info("annals stopping")
	done, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	for _, hs := range servers {
		_ = hs.Shutdown(done)
	}
	return errors.Join(err, shutdownTelemetry(done))
}

// syncDirectory keeps the people directory in step with the tenant's Entra
// users when the pod signs in to Entra (AZURE_CLIENT_ID, AZURE_TENANT_ID,
// and AZURE_FEDERATED_TOKEN_FILE set, as workload identity sets them).
func syncDirectory(ctx context.Context, st *store.Store, m *metrics.Metrics, log *slog.Logger) error {
	client, tenant, tokenFile := os.Getenv("AZURE_CLIENT_ID"), os.Getenv("AZURE_TENANT_ID"), os.Getenv("AZURE_FEDERATED_TOKEN_FILE")
	if client == "" || tenant == "" || tokenFile == "" {
		log.Info("directory sync off", "reason", "AZURE_CLIENT_ID, AZURE_TENANT_ID, and AZURE_FEDERATED_TOKEN_FILE are not all set", "people", st.Directory().People())
		return nil
	}
	interval, err := time.ParseDuration(env("DIRECTORY_INTERVAL", "30m"))
	if err != nil || interval < time.Minute {
		return errors.New("DIRECTORY_INTERVAL must be a duration of a minute or more, such as 30m")
	}
	httpClient := telemetry.Client(time.Minute)
	tokens := &graph.FederatedToken{
		LoginURL: env("AZURE_AUTHORITY_HOST", "https://login.microsoftonline.com"), TenantID: tenant, ClientID: client,
		AssertionFile: tokenFile, Scope: graph.Scope, HTTP: httpClient,
	}
	sync := &directory.Sync{
		Graph:   graph.New(env("GRAPH_URL", "https://graph.microsoft.com/v1.0"), tokens, httpClient),
		Store:   st,
		Counter: m,
		Log:     log,
	}
	log.Info("directory sync on", "interval", interval.String(), "people", st.Directory().People())
	go sync.Run(ctx, interval)
	return nil
}

// gateSettings reads how the gate is scored: GATE_KINDS, a file of the
// estate's own effect kinds in the platform kit's shape, and GATE_MAX_BLIND,
// the percent of the changes that ran a family's watch may fail to see and
// still be scored.
func gateSettings() (gate.Settings, error) {
	s := gate.Settings{Kinds: change.Base(), MaxBlind: 20}
	if path := os.Getenv("GATE_KINDS"); path != "" {
		f, err := os.Open(path)
		if err != nil {
			return s, fmt.Errorf("GATE_KINDS: %w", err)
		}
		defer f.Close()
		if s.Kinds, err = change.LoadKinds(f); err != nil {
			return s, fmt.Errorf("GATE_KINDS: %w", err)
		}
	}
	if v := os.Getenv("GATE_MAX_BLIND"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 0 || n > 100 {
			return s, errors.New("GATE_MAX_BLIND must be a whole percent from 0 to 100")
		}
		s.MaxBlind = n
	}
	return s, nil
}

func env(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}
