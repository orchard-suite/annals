// Package retention is the one exception to append-only: events older than
// a source's retention are deleted, on a schedule, by age.
package retention

import (
	"context"
	"fmt"
	"log/slog"
	"sort"
	"strconv"
	"strings"
	"time"

	"go.opentelemetry.io/otel"

	"github.com/orchard-suite/annals/internal/metrics"
	"github.com/orchard-suite/annals/internal/store"
)

// Policy is how long each source's events are kept. Zero means forever.
type Policy struct {
	Default time.Duration
	Sources map[string]time.Duration
}

// Parse reads "365d" and "beacon=90d,ion=30d". A duration is a whole
// number of days (d), hours (h), or a Go duration.
func Parse(def, sources string) (Policy, error) {
	p := Policy{Sources: map[string]time.Duration{}}
	var err error
	if def != "" {
		if p.Default, err = parseDuration(def); err != nil {
			return p, fmt.Errorf("RETENTION_DEFAULT: %w", err)
		}
	}
	for _, part := range strings.Split(sources, ",") {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}
		name, v, ok := strings.Cut(part, "=")
		if !ok {
			return p, fmt.Errorf("RETENTION_SOURCES: %q is not source=duration", part)
		}
		d, err := parseDuration(strings.TrimSpace(v))
		if err != nil {
			return p, fmt.Errorf("RETENTION_SOURCES %s: %w", name, err)
		}
		p.Sources[strings.TrimSpace(name)] = d
	}
	return p, nil
}

func parseDuration(s string) (time.Duration, error) {
	if n, ok := strings.CutSuffix(s, "d"); ok {
		days, err := strconv.Atoi(n)
		if err != nil || days < 0 {
			return 0, fmt.Errorf("%q is not a number of days", s)
		}
		return time.Duration(days) * 24 * time.Hour, nil
	}
	d, err := time.ParseDuration(s)
	if err != nil || d < 0 {
		return 0, fmt.Errorf("%q is not a duration", s)
	}
	return d, nil
}

// Describe lists the policy for the board and the logs.
func (p Policy) Describe() []string {
	var out []string
	if p.Default > 0 {
		out = append(out, "default "+days(p.Default))
	} else {
		out = append(out, "default forever")
	}
	names := make([]string, 0, len(p.Sources))
	for n := range p.Sources {
		names = append(names, n)
	}
	sort.Strings(names)
	for _, n := range names {
		if p.Sources[n] == 0 {
			out = append(out, n+" forever")
		} else {
			out = append(out, n+" "+days(p.Sources[n]))
		}
	}
	return out
}

func days(d time.Duration) string {
	if d%(24*time.Hour) == 0 {
		return strconv.Itoa(int(d/(24*time.Hour))) + " days"
	}
	return d.String()
}

// Recorder keeps a record of each source a pass deleted from.
type Recorder interface {
	Pruned(ctx context.Context, source string, n int64, cutoff time.Time)
}

// Apply prunes once under p, as of now, and returns how many events went.
// rec, when set, records each source that lost records.
func Apply(ctx context.Context, s *store.Store, p Policy, now time.Time, m *metrics.Metrics, rec Recorder) (int64, error) {
	var total int64
	pruned := func(source, bucket string, n int64, cutoff time.Time) {
		if n == 0 {
			return
		}
		if m != nil {
			m.Pruned(ctx, bucket, n)
		}
		if rec != nil {
			rec.Pruned(ctx, source, n, cutoff)
		}
		total += n
	}
	keep := make([]string, 0, len(p.Sources))
	for source, d := range p.Sources {
		keep = append(keep, source)
		if d == 0 {
			continue
		}
		cutoff := now.Add(-d)
		n, err := s.Prune(ctx, source, cutoff)
		if err != nil {
			return total, err
		}
		pruned(source, source, n, cutoff)
	}
	if p.Default > 0 {
		cutoff := now.Add(-p.Default)
		bySource, err := s.PruneExcept(ctx, keep, cutoff)
		if err != nil {
			return total, err
		}
		for source, n := range bySource {
			pruned(source, "default", n, cutoff)
		}
	}
	return total, nil
}

// Run applies p every interval until ctx ends. Each pass is its own root span.
func Run(ctx context.Context, s *store.Store, p Policy, interval time.Duration, m *metrics.Metrics, rec Recorder, log *slog.Logger) {
	tracer := otel.Tracer(metrics.Service)
	pass := func() {
		spanCtx, span := tracer.Start(ctx, "retention")
		defer span.End()
		n, err := Apply(spanCtx, s, p, time.Now(), m, rec)
		if err != nil {
			log.ErrorContext(spanCtx, "retention failed", "task", "retention", "error", err)
			return
		}
		log.InfoContext(spanCtx, "retention ran", "task", "retention", "pruned", n)
	}
	pass()
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			pass()
		}
	}
}
