// Package metrics is Annals' own instruments. Their names are a
// contract with the dashboards and alerts an estate builds on them.
package metrics

import (
	"context"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/metric"
)

// Service names Annals in telemetry.
const Service = "annals"

// Gauges is what the observable gauges read at each export.
type Gauges struct {
	Events  int
	Tokens  int
	DBBytes int64
	// People is how many people the directory holds.
	People int
}

type Metrics struct {
	ingested metric.Int64Counter
	refused  metric.Int64Counter
	pruned   metric.Int64Counter
	audit    metric.Int64Counter
	dirPass  metric.Int64Counter
}

// New registers Annals' instruments. gauges is called once per
// export, never per request.
func New(gauges func() Gauges) (*Metrics, error) {
	m := otel.Meter(Service)
	ingested, err := m.Int64Counter("annals.events.ingested", metric.WithDescription("Events stored, by source and outcome."))
	if err != nil {
		return nil, err
	}
	refused, err := m.Int64Counter("annals.events.refused", metric.WithDescription("Events refused, by reason."))
	if err != nil {
		return nil, err
	}
	pruned, err := m.Int64Counter("annals.events.pruned", metric.WithDescription("Events deleted by retention, by source."))
	if err != nil {
		return nil, err
	}
	audit, err := m.Int64Counter("annals.self.records", metric.WithDescription("Annals' own records of what it did, by verb and outcome."))
	if err != nil {
		return nil, err
	}
	dirPass, err := m.Int64Counter("annals.directory.passes", metric.WithDescription("Passes that read the tenant's users into the people directory, by outcome."))
	if err != nil {
		return nil, err
	}
	peopleGauge, err := m.Int64ObservableGauge("annals.directory.people", metric.WithDescription("People the directory holds."))
	if err != nil {
		return nil, err
	}
	events, err := m.Int64ObservableGauge("annals.events", metric.WithDescription("Events kept."))
	if err != nil {
		return nil, err
	}
	tokens, err := m.Int64ObservableGauge("annals.tokens", metric.WithDescription("Live sender tokens."))
	if err != nil {
		return nil, err
	}
	dbSize, err := m.Int64ObservableGauge("annals.db.size", metric.WithUnit("By"), metric.WithDescription("Bytes Annals' ClickHouse database holds on disk."))
	if err != nil {
		return nil, err
	}
	_, err = m.RegisterCallback(func(_ context.Context, o metric.Observer) error {
		g := gauges()
		o.ObserveInt64(events, int64(g.Events))
		o.ObserveInt64(tokens, int64(g.Tokens))
		o.ObserveInt64(dbSize, g.DBBytes)
		o.ObserveInt64(peopleGauge, int64(g.People))
		return nil
	}, events, tokens, dbSize, peopleGauge)
	if err != nil {
		return nil, err
	}
	return &Metrics{ingested: ingested, refused: refused, pruned: pruned, audit: audit, dirPass: dirPass}, nil
}

func (m *Metrics) Ingested(ctx context.Context, source, outcome string) {
	m.ingested.Add(ctx, 1, metric.WithAttributes(attribute.String("source", source), attribute.String("outcome", outcome)))
}

func (m *Metrics) Refused(ctx context.Context, reason string) {
	m.refused.Add(ctx, 1, metric.WithAttributes(attribute.String("reason", reason)))
}

func (m *Metrics) Pruned(ctx context.Context, source string, n int64) {
	m.pruned.Add(ctx, n, metric.WithAttributes(attribute.String("source", source)))
}

// SelfRecord counts one of Annals' own records: stored, duplicate,
// refused by the validator, or failed in the store.
func (m *Metrics) SelfRecord(ctx context.Context, verb, outcome string) {
	m.audit.Add(ctx, 1, metric.WithAttributes(attribute.String("verb", verb), attribute.String("outcome", outcome)))
}

// DirectoryPass counts one pass of the people directory's sync.
func (m *Metrics) DirectoryPass(ctx context.Context, outcome string) {
	m.dirPass.Add(ctx, 1, metric.WithAttributes(attribute.String("outcome", outcome)))
}
