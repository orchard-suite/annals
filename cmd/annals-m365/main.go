// Command annals-m365 collects Microsoft 365 audit records into Annals:
// sharing in SharePoint and OneDrive, and Copilot prompts with their tool
// calls. See internal/m365.
package main

import (
	"context"
	"errors"
	"log/slog"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/orchard-suite/platformkit/v3/annals"
	"github.com/orchard-suite/platformkit/v3/telemetry"

	"github.com/orchard-suite/annals/internal/graph"
	"github.com/orchard-suite/annals/internal/m365"
)

// version is set at build time from VERSION.
var version = "dev"

func main() {
	log := telemetry.NewLogger(os.Stdout, m365.Service, version, telemetry.ParseLevel(os.Getenv("LOG_LEVEL")))
	if err := run(log); err != nil {
		log.Error("collector stopped", "error", err)
		os.Exit(1)
	}
}

func run(log *slog.Logger) error {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	need := func(k string) (string, error) {
		if v := os.Getenv(k); v != "" {
			return v, nil
		}
		return "", errors.New(k + " is required")
	}
	annalsURL, err := need("ANNALS_URL")
	if err != nil {
		return err
	}
	annalsToken, err := need("ANNALS_TOKEN")
	if err != nil {
		return err
	}
	tenant, err := need("AZURE_TENANT_ID")
	if err != nil {
		return err
	}
	client, err := need("AZURE_CLIENT_ID")
	if err != nil {
		return err
	}
	tokenFile, err := need("AZURE_FEDERATED_TOKEN_FILE")
	if err != nil {
		return err
	}
	interval, err := duration("INTERVAL", 30*time.Minute)
	if err != nil {
		return err
	}
	lookback, err := duration("LOOKBACK", 48*time.Hour)
	if err != nil {
		return err
	}
	lag, err := duration("LAG", 15*time.Minute)
	if err != nil {
		return err
	}
	recent, err := duration("RECENT", 3*time.Hour)
	if err != nil {
		return err
	}
	sweep, err := duration("SWEEP", 24*time.Hour)
	if err != nil {
		return err
	}

	shutdown, err := telemetry.Setup(ctx, m365.Service, version)
	if err != nil {
		return err
	}
	httpClient := telemetry.Client(2 * time.Minute)
	tokens := &graph.FederatedToken{
		LoginURL: env("AZURE_AUTHORITY_HOST", "https://login.microsoftonline.com"), TenantID: tenant, ClientID: client,
		AssertionFile: tokenFile, Scope: graph.Scope, HTTP: httpClient,
	}
	c := &m365.Collector{
		Graph:    m365.NewGraph(graph.New(env("GRAPH_URL", "https://graph.microsoft.com/v1.0"), tokens, httpClient)),
		Post:     annals.New(annalsURL, annalsToken, env("ANNALS_SOURCE", "m365")),
		Log:      log,
		Lookback: lookback, Recent: recent, Sweep: sweep, Lag: lag, Poll: 30 * time.Second,
	}
	if err := c.Metrics(); err != nil {
		return err
	}
	log.Info("collector started", "interval", interval.String(), "lookback", lookback.String(), "recent", recent.String(), "sweep", sweep.String(), "record_types", m365.RecordTypes)
	c.Run(ctx, interval)
	done, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	return shutdown(done)
}

func env(k, fallback string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return fallback
}

func duration(k string, fallback time.Duration) (time.Duration, error) {
	v := os.Getenv(k)
	if v == "" {
		return fallback, nil
	}
	d, err := time.ParseDuration(v)
	if err != nil || d <= 0 {
		return 0, errors.New(k + " must be a duration such as 30m")
	}
	return d, nil
}
