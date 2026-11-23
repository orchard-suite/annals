package store_test

import (
	"context"
	"testing"
	"time"

	"github.com/orchard-suite/platformkit/v3/annals"

	. "github.com/orchard-suite/annals/internal/store"
)

func TestPatternsFoldRepeatsByActorVerbAndKind(t *testing.T) {
	s := open(t)
	ctx := context.Background()
	now := time.Now().UTC().Truncate(time.Hour)
	since := now.Add(-4 * time.Hour)
	var rows []annals.Event
	for i := range 5 {
		rows = append(rows, mk("beacon", "beacon-agent", "secret.reveal", "beacon", []string{"a", "b"}[i%2], "ok", since.Add(time.Duration(i)*time.Minute)))
	}
	rows = append(rows,
		mk("beacon", "beacon-agent", "secret.reveal", "beacon", "a", "failed", now.Add(-30*time.Minute)),
		mk("ion", "sess-a", "tool.bash", "workstation", "/x", "refused", now.Add(-90*time.Minute)),
		mk("ion", "sess-a", "tool.bash", "workstation", "/x", "ok", now.Add(-5*time.Hour)),
	)
	for _, r := range rows {
		if _, _, err := put(t, s, r); err != nil {
			t.Fatal(err)
		}
	}
	ps, err := s.Patterns(ctx, Filter{Since: since}, now, time.Hour, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(ps) != 2 {
		t.Fatalf("patterns: %d, want 2 (the record before the window is left out)", len(ps))
	}
	p := ps[0]
	if p.Latest.Actor.ID != "beacon-agent" || p.Count != 6 || p.Failed != 1 || p.Targets != 2 {
		t.Errorf("first pattern: %+v", p)
	}
	if p.Latest.Action.Outcome != "failed" {
		t.Errorf("latest is the newest record, got %s", p.Latest.Action.Outcome)
	}
	if len(p.Steps) != 5 || p.Steps[0] != 5 || p.Steps[3] != 1 {
		t.Errorf("steps: %v", p.Steps)
	}
	if ps[1].Refused != 1 || ps[1].Count != 1 {
		t.Errorf("second pattern: %+v", ps[1])
	}
	if ps[1].LatestBad == nil || ps[1].LatestBad.Action.Outcome != "refused" || ps[1].Latest.ID != ps[1].LatestBad.ID {
		t.Errorf("the newest refused record comes back whole: %+v", ps[1].LatestBad)
	}
	counts, err := s.PatternCounts(ctx, Filter{Since: since}, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(counts) != 2 || counts[p.Key] != 6 || counts[ps[1].Key] != 1 {
		t.Errorf("pattern counts match the patterns: %v", counts)
	}
}

func TestPulseCountsEachStepByOutcome(t *testing.T) {
	s := open(t)
	ctx := context.Background()
	now := time.Now().UTC().Truncate(time.Hour)
	since := now.Add(-2 * time.Hour)
	for _, r := range []annals.Event{
		mk("ion", "a", "tool.bash", "workstation", "/x", "ok", since.Add(time.Minute)),
		mk("ion", "a", "tool.bash", "workstation", "/x", "failed", since.Add(2*time.Minute)),
		mk("ion", "a", "tool.bash", "workstation", "/x", "refused", now.Add(time.Minute)),
	} {
		if _, _, err := put(t, s, r); err != nil {
			t.Fatal(err)
		}
	}
	steps, err := s.Pulse(ctx, Filter{Since: since}, now, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	want := []Step{{Total: 2, Failed: 1}, {}, {Total: 1, Refused: 1}}
	if len(steps) != 3 || steps[0] != want[0] || steps[1] != want[1] || steps[2] != want[2] {
		t.Errorf("pulse: %+v", steps)
	}
}
