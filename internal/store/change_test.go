package store_test

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/orchard-suite/platformkit/v3/annals"

	. "github.com/orchard-suite/annals/internal/store"
)

// observed is a change.observed record from source with one sighting per
// verdict, each of a stops effect on a workload, in place.
func observed(source, key string, at time.Time, verdicts ...string) annals.Event {
	ev := annals.Event{At: at, Source: source, EventKey: key, Actor: annals.Actor{Kind: "system", ID: "dispatch-watch"},
		Action: annals.Action{Verb: "change.observed", Target: annals.Target{System: "orrery", Kind: "change", ID: "run:" + key}, Outcome: "ok"},
		Cause:  "run:" + key, CauseSource: "orrery"}
	for i, v := range verdicts {
		ev.Effects = append(ev.Effects, annals.Effect{Kind: "stops", Subject: annals.Subject{Blueprint: "workload", Key: fmt.Sprintf("acme-%d", i)}, Path: "in-place", Verdict: v})
	}
	return ev
}

func TestProofCountsWatchedChangesByKey(t *testing.T) {
	s := open(t)
	ctx := context.Background()
	key := ProofKey{Kind: "stops", Blueprint: "workload", Path: "in-place"}
	none, err := s.Proof(ctx, key)
	if err != nil {
		t.Fatal(err)
	}
	if none != (Proof{ProofKey: key}) {
		t.Fatalf("a key never watched counts nothing: %+v", none)
	}

	t0 := time.Date(2026, 11, 26, 15, 0, 0, 0, time.UTC)
	stated := example(t, "change-stated")
	evs := []annals.Event{
		observed("dispatch", "w1", t0, "clean", "clean"),
		observed("dispatch", "w2", t0.Add(time.Hour), "clean", "blind"),
		observed("dispatch", "w3", t0.Add(2*time.Hour), "blind", "harm", "clean"),
		observed("dispatch", "w4", t0.Add(3*time.Hour), "clean"),
		observed("dispatch", "w1", t0.Add(4*time.Hour), "harm"), // a repeat of w1, stored once
		observed("tally", "t1", t0.Add(5*time.Hour), "clean"),
		stated, // predicted effects carry no verdict and prove nothing
	}
	notWatch := observed("dispatch", "x1", t0, "harm")
	notWatch.Action.Verb = "change.ended"
	evs = append(evs, notWatch)
	other := observed("dispatch", "o1", t0, "harm")
	other.Effects[0].Path = "swap"
	evs = append(evs, other)
	if _, err := s.Put(ctx, evs); err != nil {
		t.Fatal(err)
	}

	got, err := s.Proof(ctx, key)
	if err != nil {
		t.Fatal(err)
	}
	want := Proof{ProofKey: key, Watched: 5, Clean: 3, Harm: 1, Blind: 1, LastSeen: t0.Add(5 * time.Hour)}
	if got != want {
		t.Errorf("proof\n got %+v\nwant %+v", got, want)
	}

	key.Source = "dispatch"
	got, err = s.Proof(ctx, key)
	if err != nil {
		t.Fatal(err)
	}
	want = Proof{ProofKey: key, Watched: 4, Clean: 2, Harm: 1, Blind: 1, LastSeen: t0.Add(3 * time.Hour)}
	if got != want {
		t.Errorf("proof from one sender\n got %+v\nwant %+v", got, want)
	}

	swap, err := s.Proof(ctx, ProofKey{Kind: "stops", Blueprint: "workload", Path: "swap"})
	if err != nil {
		t.Fatal(err)
	}
	if swap.Watched != 1 || swap.Harm != 1 {
		t.Errorf("each path is its own key: %+v", swap)
	}
}

func TestCauseAcrossSenders(t *testing.T) {
	s := open(t)
	ctx := context.Background()
	stated := example(t, "change-stated")
	routed := example(t, "change-routed")
	seen := example(t, "change-observed")
	// The same key from dispatch is another event, never the cause.
	decoy := mk("dispatch", "dispatch-watch", "change.note", "orrery", "x", "ok", time.Now())
	decoy.EventKey = "n1"
	decoy.Cause = stated.EventKey
	if _, err := s.Put(ctx, []annals.Event{stated, routed, seen, decoy}); err != nil {
		t.Fatal(err)
	}
	page, err := s.Query(ctx, Filter{Cause: stated.EventKey, CauseSource: "orrery"})
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]bool{}
	for _, ev := range page.Events {
		got[ev.Source+"/"+ev.EventKey] = true
	}
	if len(got) != 2 || !got["orrery/"+routed.EventKey] || !got["dispatch/"+seen.EventKey] {
		t.Errorf("what the change.stated event led to, from every sender: %v", got)
	}
	page, err = s.Query(ctx, Filter{Cause: stated.EventKey, CauseSource: "dispatch"})
	if err != nil {
		t.Fatal(err)
	}
	if len(page.Events) != 1 || page.Events[0].EventKey != "n1" {
		t.Errorf("a cause with no cause_source is from its own sender: %v", page.Events)
	}
	stored, err := s.ByKey(ctx, seen.Source, seen.EventKey)
	if err != nil {
		t.Fatal(err)
	}
	if stored.CauseSource != "orrery" || len(stored.Effects) != 2 || stored.Effects[1].Verdict != "blind" {
		t.Errorf("a record keeps its cause_source and effects: %+v", stored)
	}
}
