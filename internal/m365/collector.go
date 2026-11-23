package m365

import (
	"context"
	"log/slog"
	"time"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/metric"

	"github.com/orchard-suite/platformkit/v3/annals"
)

// Service names the collector in telemetry.
const Service = "annals-m365"

// Poster posts events to Annals.
type Poster interface {
	PostBatch(ctx context.Context, evs []annals.Event) ([]annals.Receipt, error)
}

// Collector reads the audit log on a schedule and posts what it finds.
type Collector struct {
	Graph *Graph
	Post  Poster
	Log   *slog.Logger
	// Lookback is how far back a sweep reads. Records reach the audit log
	// up to a day late, so it is longer than a day; Annals stores a
	// record's events once however often a pass reads it.
	Lookback time.Duration
	// Recent is how far back the passes between sweeps read: past how late
	// nearly every record arrives, and far less than Lookback, so most
	// passes do not post the same records again. Zero makes every pass a
	// sweep.
	Recent time.Duration
	// Sweep is how often a pass reads the whole Lookback. The first pass
	// always does, to cover the time the collector was down.
	Sweep time.Duration
	// Poll is how often a pass checks a running query.
	Poll time.Duration
	// Lag leaves the newest minutes out of a window, where the audit log is
	// still filling.
	Lag time.Duration

	swept time.Time

	records  metric.Int64Counter
	receipts metric.Int64Counter
	passes   metric.Int64Counter
	duration metric.Float64Histogram
}

// Metrics registers the collector's instruments. Their attributes are the
// record type, the receipt status, and the pass outcome, never a person.
func (c *Collector) Metrics() error {
	m := otel.Meter(Service)
	var err error
	if c.records, err = m.Int64Counter("annals_m365.records", metric.WithDescription("Audit records read, by record type and whether they were mapped.")); err != nil {
		return err
	}
	if c.receipts, err = m.Int64Counter("annals_m365.events", metric.WithDescription("Events posted to Annals, by receipt status.")); err != nil {
		return err
	}
	if c.passes, err = m.Int64Counter("annals_m365.passes", metric.WithDescription("Collection passes, by outcome and kind (sweep or recent).")); err != nil {
		return err
	}
	c.duration, err = m.Float64Histogram("annals_m365.pass.duration", metric.WithUnit("s"), metric.WithDescription("How long a pass took, by outcome and kind."))
	return err
}

// window is what a pass at now reads, [from, to), and whether it is a
// sweep of the whole Lookback.
func (c *Collector) window(now time.Time) (from, to time.Time, sweep bool) {
	sweep = c.Recent <= 0 || c.Recent >= c.Lookback || c.swept.IsZero() || now.Sub(c.swept) >= c.Sweep
	reach := c.Recent
	if sweep {
		reach = c.Lookback
	}
	return now.Add(-reach), now.Add(-c.Lag), sweep
}

// Pass reads its window, a sweep of the whole Lookback or the Recent
// stretch, and posts every record's events.
func (c *Collector) Pass(ctx context.Context, now time.Time) error {
	start := time.Now()
	from, to, sweep := c.window(now)
	kind := "recent"
	if sweep {
		kind = "sweep"
	}
	counts := map[string]int{}
	read, skipped := 0, map[string]int{}
	err := c.Graph.Records(ctx, from, to, c.Poll, func(page []Record) error {
		var evs []annals.Event
		for _, r := range page {
			mapped, ok := Map(r)
			read++
			if !ok {
				skipped[r.RecordType]++
			}
			c.count(ctx, c.records, 1, attribute.String("type", r.RecordType), attribute.Bool("mapped", ok))
			evs = append(evs, mapped...)
		}
		if len(evs) == 0 {
			return nil
		}
		receipts, err := c.Post.PostBatch(ctx, evs)
		if err != nil {
			return err
		}
		for _, rc := range receipts {
			counts[rc.Status]++
			c.count(ctx, c.receipts, 1, attribute.String("status", rc.Status))
			if rc.Status == "refused" || rc.Status == "failed" {
				c.Log.WarnContext(ctx, "event not stored", "status", rc.Status, "reason", rc.Reason, "event_key", rc.EventKey)
			}
		}
		return nil
	})
	outcome := "ok"
	if err != nil {
		outcome = "failed"
	}
	c.count(ctx, c.passes, 1, attribute.String("outcome", outcome), attribute.String("kind", kind))
	if c.duration != nil {
		c.duration.Record(ctx, time.Since(start).Seconds(), metric.WithAttributes(attribute.String("outcome", outcome), attribute.String("kind", kind)))
	}
	if err != nil {
		c.Log.ErrorContext(ctx, "pass failed", "task", "collect", "kind", kind, "from", from.UTC(), "to", to.UTC(), "error", err)
		return err
	}
	if sweep {
		c.swept = now
	}
	c.Log.InfoContext(ctx, "pass done", "task", "collect", "kind", kind, "from", from.UTC(), "to", to.UTC(),
		"records", read, "skipped", skipped, "stored", counts["stored"], "duplicate", counts["duplicate"], "refused", counts["refused"], "failed", counts["failed"], "seconds", int(time.Since(start).Seconds()))
	return nil
}

func (c *Collector) count(ctx context.Context, ctr metric.Int64Counter, n int64, attrs ...attribute.KeyValue) {
	if ctr != nil {
		ctr.Add(ctx, n, metric.WithAttributes(attrs...))
	}
}

// Run passes every interval until ctx ends.
func (c *Collector) Run(ctx context.Context, interval time.Duration) {
	tracer := otel.Tracer(Service)
	pass := func() {
		spanCtx, span := tracer.Start(ctx, "collect")
		defer span.End()
		_ = c.Pass(spanCtx, time.Now())
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
