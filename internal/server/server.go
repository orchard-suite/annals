// Package server is Annals server. It answers on two ports, one per kind
// of caller, so no route can be reached with the wrong kind of identity:
//
//   - API: products posting events, agents and tools asking questions. Trusts
//     only a bearer token Annals issued. /healthz is open.
//   - Session: the board and token administration. Trusts the front door's
//     X-Forwarded-Email and X-Forwarded-Groups headers, so only Traefik may
//     reach it.
//
// Every read passes through the caller's level (people.Level): a reader
// sees each person as a ref, a PII reader and an admin see people.
package server

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/orchard-suite/platformkit/v3/annals"
	"github.com/orchard-suite/platformkit/v3/door"
	"github.com/orchard-suite/platformkit/v3/faro"

	"github.com/orchard-suite/annals/internal/metrics"
	"github.com/orchard-suite/annals/internal/people"
	"github.com/orchard-suite/annals/internal/retention"
	"github.com/orchard-suite/annals/internal/selfaudit"
	"github.com/orchard-suite/annals/internal/store"
)

type Config struct {
	// PublicURL is the board, such as https://annals.example.com.
	PublicURL string
	// ReaderRole, PIIRole, and AdminRole are the bare front-door roles, such
	// as Admin. A reader opens the board with people as refs, a PII reader
	// also sees people, and an admin sees everything and administers tokens.
	// Each role implies the ones before it.
	ReaderRole string
	PIIRole    string
	AdminRole  string
	// FaroURL and FaroKey relay browser telemetry. Without them the server
	// accepts batches and drops them.
	FaroURL string
	FaroKey string
	// Retention is shown on the board.
	Retention retention.Policy
	// AreaWords are verb prefixes that name an area of a product, such as
	// "vault" in "vault.reveal_secret": a sentence on the board leaves them
	// out, as it does "entity", "record", and "item".
	AreaWords []string
	Version   string
}

type Server struct {
	cfg     Config
	store   *store.Store
	metrics *metrics.Metrics
	audit   *selfaudit.Auditor
	log     *slog.Logger
	// publicOrigin is what a browser sends as Origin from the board.
	publicOrigin string
	// readers admits every role, admins only the admin role.
	readers *door.Gate
	admins  *door.Gate
	relay   *faro.Relay
}

// New builds the server. audit keeps Annals' own records of tokens
// issued and revoked and of people shown.
func New(cfg Config, st *store.Store, m *metrics.Metrics, audit *selfaudit.Auditor, log *slog.Logger) (*Server, error) {
	addAreaWords(cfg.AreaWords)
	u, err := url.Parse(cfg.PublicURL)
	if err != nil || u.Scheme == "" || u.Host == "" {
		return nil, errors.New("PUBLIC_URL must be an absolute URL")
	}
	cfg.PublicURL = strings.TrimRight(cfg.PublicURL, "/")
	if cfg.ReaderRole == "" {
		cfg.ReaderRole = "Reader"
	}
	if cfg.PIIRole == "" {
		cfg.PIIRole = "PiiReader"
	}
	if cfg.AdminRole == "" {
		cfg.AdminRole = "Admin"
	}
	readers, err := door.NewGate(log, cfg.ReaderRole, cfg.PIIRole, cfg.AdminRole)
	if err != nil {
		return nil, err
	}
	admins, err := door.NewGate(log, cfg.AdminRole)
	if err != nil {
		return nil, err
	}
	relay, err := faro.NewRelay(faro.Config{Service: metrics.Service, Version: cfg.Version, URL: cfg.FaroURL, Key: cfg.FaroKey}, log)
	if err != nil {
		return nil, err
	}
	return &Server{
		cfg:          cfg,
		store:        st,
		metrics:      m,
		audit:        audit,
		log:          log,
		publicOrigin: u.Scheme + "://" + u.Host,
		readers:      readers,
		admins:       admins,
		relay:        relay,
	}, nil
}

// Gauges feeds the observable gauges.
func (s *Server) Gauges() metrics.Gauges {
	st, err := s.store.Stats(context.Background())
	if err != nil {
		s.log.Error("stats failed", "error", err)
		return metrics.Gauges{}
	}
	return metrics.Gauges{Events: st.Events, Tokens: st.Tokens, DBBytes: st.DBBytes, People: s.store.Directory().People()}
}

// Ingest validates and stores events for the source a token allows and
// returns one receipt per event, in order. It is the one way a record
// enters, shared by the single and batch routes. Valid events are stored
// together even when others are refused.
func (s *Server) Ingest(ctx context.Context, evs []annals.Event, source string) []annals.Receipt {
	receipts := make([]annals.Receipt, len(evs))
	var valid []annals.Event
	var at []int
	for i, ev := range evs {
		receipts[i] = annals.Receipt{Index: i, EventKey: ev.EventKey}
		if err := ev.Validate(source); err != nil {
			s.metrics.Refused(ctx, annals.Reason(err))
			s.log.WarnContext(ctx, "event refused", "source", source, "reason", annals.Reason(err), "detail", err.Error(), "event_key", ev.EventKey)
			receipts[i].Status, receipts[i].Reason, receipts[i].Error = "refused", annals.Reason(err), err.Error()
			continue
		}
		valid = append(valid, ev)
		at = append(at, i)
	}
	if len(valid) == 0 {
		return receipts
	}
	results, err := s.store.Put(ctx, valid)
	if err != nil {
		s.log.ErrorContext(ctx, "events not stored", "source", source, "events", len(valid), "error", err)
		for _, i := range at {
			s.metrics.Refused(ctx, "store_error")
			receipts[i].Status, receipts[i].Reason, receipts[i].Error = "failed", "store_error", "the event could not be stored"
		}
		return receipts
	}
	for j, res := range results {
		rc := &receipts[at[j]]
		rc.ID = res.Event.ID
		if res.Duplicate {
			rc.Status, rc.Duplicate = "duplicate", true
			s.log.DebugContext(ctx, "event already stored", "source", source, "event", res.Event.ID, "event_key", rc.EventKey)
			continue
		}
		rc.Status = "stored"
		s.metrics.Ingested(ctx, res.Event.Source, res.Event.Action.Outcome)
		s.log.InfoContext(ctx, "event stored", "source", res.Event.Source, "event", res.Event.ID,
			"actor_kind", res.Event.Actor.Kind, "verb", res.Event.Action.Verb, "system", res.Event.Action.Target.System, "outcome", res.Event.Action.Outcome)
	}
	return receipts
}

// view is what one caller may see: their level and the filter mask it sets.
type view struct {
	level  people.Level
	masker people.Masker
}

func (s *Server) viewAt(level people.Level) view { return view{level: level, masker: s.store.Masker()} }

func (v view) event(ev annals.Event) annals.Event { return v.masker.For(v.level, ev) }

func (v view) events(evs []annals.Event) []annals.Event {
	if v.level.SeesPeople() {
		return evs
	}
	out := make([]annals.Event, len(evs))
	for i, ev := range evs {
		out[i] = v.masker.Mask(ev)
	}
	return out
}

func (v view) filter(f store.Filter) store.Filter {
	f.Masked = !v.level.SeesPeople()
	return f
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

// Problem is the API's error body.
type Problem struct {
	Error  string `json:"error"`
	Reason string `json:"reason,omitempty"`
}

func writeError(w http.ResponseWriter, status int, msg, reason string) {
	writeJSON(w, status, Problem{Error: msg, Reason: reason})
}

func (s *Server) logRequest(ctx context.Context, level slog.Level, msg string, r *http.Request, attrs ...any) {
	s.log.Log(ctx, level, msg, append([]any{"method", r.Method, "route", r.Pattern}, attrs...)...)
}

// parseFilter reads the query filters every list shares.
func parseFilter(q url.Values) (store.Filter, error) {
	var f store.Filter
	var err error
	if f.Since, err = parseTime(q.Get("since")); err != nil {
		return f, errors.New("since must be RFC 3339 or a span such as 24h or 7d")
	}
	if f.Until, err = parseTime(q.Get("until")); err != nil {
		return f, errors.New("until must be RFC 3339 or a span such as 24h or 7d")
	}
	f.Actor = q.Get("actor")
	f.Source = q.Get("source")
	f.System = q.Get("system")
	f.Outcome = q.Get("outcome")
	f.Verb = q.Get("verb")
	f.TargetKind = q.Get("kind")
	f.TargetID = q.Get("target")
	f.Party = q.Get("party")
	f.Cause = q.Get("cause")
	f.CauseSource = q.Get("cause_source")
	f.ApprovalRequired = q.Get("approval") == "required"
	f.Cursor = q.Get("cursor")
	if v := q.Get("limit"); v != "" {
		n, err := parseInt(v)
		if err != nil || n < 1 {
			return f, errors.New("limit must be a positive number")
		}
		f.Limit = n
	}
	return f, nil
}

// parseTime reads an RFC 3339 time or a span back from now ("24h", "7d").
func parseTime(v string) (time.Time, error) {
	if v == "" {
		return time.Time{}, nil
	}
	if t, err := time.Parse(time.RFC3339, v); err == nil {
		return t, nil
	}
	if n, ok := strings.CutSuffix(v, "d"); ok {
		d, err := parseInt(n)
		if err != nil {
			return time.Time{}, err
		}
		return time.Now().Add(-time.Duration(d) * 24 * time.Hour), nil
	}
	d, err := time.ParseDuration(v)
	if err != nil {
		return time.Time{}, err
	}
	return time.Now().Add(-d), nil
}

func parseInt(v string) (int, error) {
	n := 0
	if v == "" {
		return 0, errors.New("empty")
	}
	for _, c := range v {
		if c < '0' || c > '9' {
			return 0, errors.New("not a number")
		}
		n = n*10 + int(c-'0')
		if n > 1<<30 {
			return 0, errors.New("too large")
		}
	}
	return n, nil
}
