package store_test

import (
	"context"
	"encoding/json"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/orchard-suite/platformkit/v3/annals"

	. "github.com/orchard-suite/annals/internal/store"
	"github.com/orchard-suite/annals/internal/store/storetest"
)

func open(t *testing.T) *Store { return storetest.Open(t) }

// put stores one event the way the server does.
func put(t *testing.T, s *Store, ev annals.Event) (annals.Event, bool, error) {
	t.Helper()
	res, err := s.Put(context.Background(), []annals.Event{ev})
	if err != nil {
		return annals.Event{}, false, err
	}
	return res[0].Event, res[0].Duplicate, nil
}

func example(t *testing.T, name string) annals.Event {
	t.Helper()
	b, err := os.ReadFile("../../schema/examples/" + name + ".json")
	if err != nil {
		t.Fatal(err)
	}
	var ev annals.Event
	if err := json.Unmarshal(b, &ev); err != nil {
		t.Fatal(err)
	}
	return ev
}

func mk(source, actor, verb, system, target, outcome string, at time.Time) annals.Event {
	ev := annals.Event{At: at, Source: source, Actor: annals.Actor{Kind: "agent", ID: actor},
		Action: annals.Action{Verb: verb, Target: annals.Target{System: system, Kind: "thing", ID: target}, Outcome: outcome}}
	if outcome != "ok" {
		ev.Action.Reason = "because"
	}
	return ev
}

func TestPutAssignsIDAndIsIdempotentByKey(t *testing.T) {
	s := open(t)
	ctx := context.Background()
	ev := example(t, "beacon")
	first, dup, err := put(t, s, ev)
	if err != nil || dup || first.ID == "" || first.ReceivedAt.IsZero() {
		t.Fatalf("first put: %v dup=%v id=%q", err, dup, first.ID)
	}
	again, dup, err := put(t, s, ev)
	if err != nil || !dup || again.ID != first.ID {
		t.Fatalf("second put: %v dup=%v id=%q want %q", err, dup, again.ID, first.ID)
	}
	other := ev
	other.Source = "another"
	third, dup, err := put(t, s, other)
	if err != nil || dup || third.ID == first.ID {
		t.Fatalf("same key from another source is a new event: %v dup=%v", err, dup)
	}
	noKey := ev
	noKey.EventKey = ""
	a, _, _ := put(t, s, noKey)
	b, _, _ := put(t, s, noKey)
	if a.ID == b.ID {
		t.Error("without a key every post is a new event")
	}
	n, _ := s.Count(ctx, Filter{})
	if n != 4 {
		t.Errorf("count %d, want 4", n)
	}
	got, err := s.Get(ctx, first.ID)
	if err != nil || got.Actor.Name != "Beacon Agent" || got.Labels["module"] != "tasks" {
		t.Errorf("get: %v %+v", err, got)
	}
}

func TestFiltersAndPaging(t *testing.T) {
	s := open(t)
	ctx := context.Background()
	now := time.Now().UTC().Truncate(time.Second)
	rows := []annals.Event{
		mk("beacon", "beacon-agent", "record.update", "beacon", "r1", "ok", now.Add(-1*time.Hour)),
		mk("beacon", "beacon-agent", "record.update", "beacon", "r2", "failed", now.Add(-2*time.Hour)),
		mk("ion", "sess-a", "tool.bash", "workstation", "/x", "refused", now.Add(-3*time.Hour)),
		mk("ion", "sess-a", "tool.write", "workstation", "/y", "ok", now.Add(-30*time.Hour)),
		mk("graft", "graft-agent", "change.land", "github", "o/r", "ok", now.Add(-5*time.Hour)),
	}
	rows[4].Authority = &annals.Authority{Approval: &annals.Approval{Required: true, By: "user-1"}}
	for _, r := range rows {
		if _, _, err := put(t, s, r); err != nil {
			t.Fatal(err)
		}
	}
	count := func(f Filter) int {
		t.Helper()
		p, err := s.Query(ctx, f)
		if err != nil {
			t.Fatal(err)
		}
		return len(p.Events)
	}
	if n := count(Filter{}); n != 5 {
		t.Errorf("all: %d", n)
	}
	if n := count(Filter{Source: "ion"}); n != 2 {
		t.Errorf("source: %d", n)
	}
	if n := count(Filter{Actor: "beacon-agent"}); n != 2 {
		t.Errorf("actor: %d", n)
	}
	if n := count(Filter{System: "workstation"}); n != 2 {
		t.Errorf("system: %d", n)
	}
	if n := count(Filter{Outcome: "ok"}); n != 3 {
		t.Errorf("outcome: %d", n)
	}
	if n := count(Filter{Verb: "tool.bash"}); n != 1 {
		t.Errorf("verb: %d", n)
	}
	if n := count(Filter{Since: now.Add(-24 * time.Hour)}); n != 4 {
		t.Errorf("since: %d", n)
	}
	if n := count(Filter{Until: now.Add(-24 * time.Hour)}); n != 1 {
		t.Errorf("until: %d", n)
	}
	if n := count(Filter{ApprovalRequired: true}); n != 1 {
		t.Errorf("approval: %d", n)
	}
	if n := count(Filter{System: "workstation", TargetID: "/x"}); n != 1 {
		t.Errorf("target: %d", n)
	}
	if n := count(Filter{Source: "ion", Outcome: "ok", Since: now.Add(-24 * time.Hour)}); n != 0 {
		t.Errorf("combined: %d", n)
	}

	p1, err := s.Query(ctx, Filter{Limit: 2})
	if err != nil || len(p1.Events) != 2 || p1.Next == "" {
		t.Fatalf("page 1: %v %d next=%q", err, len(p1.Events), p1.Next)
	}
	if p1.Events[0].At.Before(p1.Events[1].At) {
		t.Error("newest first")
	}
	p2, err := s.Query(ctx, Filter{Limit: 2, Cursor: p1.Next})
	if err != nil || len(p2.Events) != 2 || p2.Next == "" {
		t.Fatalf("page 2: %v %d", err, len(p2.Events))
	}
	p3, err := s.Query(ctx, Filter{Limit: 2, Cursor: p2.Next})
	if err != nil || len(p3.Events) != 1 || p3.Next != "" {
		t.Fatalf("page 3: %v %d next=%q", err, len(p3.Events), p3.Next)
	}
	seen := map[string]bool{}
	for _, p := range []Page{p1, p2, p3} {
		for _, e := range p.Events {
			if seen[e.ID] {
				t.Errorf("event %s on two pages", e.ID)
			}
			seen[e.ID] = true
		}
	}

	bs, err := s.Breakdown(ctx, Filter{}, "source", 10)
	if err != nil || len(bs) != 3 || bs[0].Count != 2 {
		t.Errorf("breakdown: %v %+v", err, bs)
	}
	if _, err := s.Breakdown(ctx, Filter{}, "body", 10); err == nil {
		t.Error("grouping by an unknown column must fail")
	}
	st, err := s.Stats(ctx)
	if err != nil || st.Events != 5 || st.Actors != 3 || st.DBBytes == 0 {
		t.Errorf("stats: %v %+v", err, st)
	}
}

func TestPruneByAge(t *testing.T) {
	s := open(t)
	ctx := context.Background()
	now := time.Now()
	for _, r := range []annals.Event{
		mk("ion", "a", "tool.bash", "ws", "/1", "ok", now.Add(-40*24*time.Hour)),
		mk("ion", "a", "tool.bash", "ws", "/2", "ok", now.Add(-10*24*time.Hour)),
		mk("beacon", "j", "r.u", "beacon", "r", "ok", now.Add(-400*24*time.Hour)),
		mk("beacon", "j", "r.u", "beacon", "r", "ok", now.Add(-100*24*time.Hour)),
		mk("ops", "p", "w.r", "k8s", "d", "ok", now.Add(-400*24*time.Hour)),
	} {
		if _, _, err := put(t, s, r); err != nil {
			t.Fatal(err)
		}
	}
	n, err := s.Prune(ctx, "ion", now.Add(-30*24*time.Hour))
	if err != nil || n != 1 {
		t.Fatalf("prune ion: %v %d", err, n)
	}
	bySource, err := s.PruneExcept(ctx, []string{"ion"}, now.Add(-365*24*time.Hour))
	if err != nil || len(bySource) != 2 || bySource["beacon"] != 1 || bySource["ops"] != 1 {
		t.Fatalf("prune default: %v %v", err, bySource)
	}
	left, _ := s.Count(ctx, Filter{})
	if left != 2 {
		t.Errorf("%d left, want 2", left)
	}
}

func TestTokens(t *testing.T) {
	s := open(t)
	ctx := context.Background()
	secret, tok, err := s.Mint(ctx, "beacon prod", "beacon", "", "admin@example.com")
	if err != nil || secret == "" || tok.ID == "" {
		t.Fatal(err)
	}
	got, ok := s.Verify(ctx, secret)
	if !ok || got.Source != "beacon" || got.LastUsed.IsZero() || got.Reads != "reader" {
		t.Fatalf("verify: %v %+v", ok, got)
	}
	if _, ok := s.Verify(ctx, secret+"x"); ok {
		t.Error("a wrong secret verified")
	}
	if _, ok := s.Verify(ctx, "nope"); ok {
		t.Error("a secret without the prefix verified")
	}
	if revoked, err := s.Revoke(ctx, tok.ID); err != nil || revoked.ID != tok.ID || revoked.Source != "beacon" || revoked.RevokedAt.IsZero() {
		t.Fatalf("revoke: %v %+v", err, revoked)
	}
	if _, ok := s.Verify(ctx, secret); ok {
		t.Error("a revoked token verified")
	}
	if _, err := s.Revoke(ctx, tok.ID); err != ErrNotFound {
		t.Errorf("revoking twice: %v", err)
	}
	list, _ := s.Tokens(ctx)
	if len(list) != 1 || list[0].RevokedAt.IsZero() {
		t.Errorf("list: %+v", list)
	}
	if _, _, err := s.Mint(ctx, "x", "beacon", "admin", "admin@example.com"); err == nil {
		t.Error("a token never administers")
	}
	pii, tok2, err := s.Mint(ctx, "auditor", "audit", "pii", "admin@example.com")
	if err != nil {
		t.Fatal(err)
	}
	if got, ok := s.Verify(ctx, pii); !ok || got.Level().String() != "pii" || got.ID != tok2.ID {
		t.Errorf("a pii token reads as pii: %+v", got)
	}
}

// A sender that retries a batch while the first copy is still being written
// gets one record per key, never two.
func TestConcurrentPutsStoreOnce(t *testing.T) {
	s := open(t)
	ctx := context.Background()
	ev := example(t, "beacon")
	var wg sync.WaitGroup
	ids := make([]string, 8)
	for i := range ids {
		wg.Add(1)
		go func() {
			defer wg.Done()
			res, err := s.Put(ctx, []annals.Event{ev, ev})
			if err != nil {
				t.Error(err)
				return
			}
			if res[0].Event.ID != res[1].Event.ID || !res[1].Duplicate {
				t.Errorf("the same key twice in one call: %+v", res)
			}
			ids[i] = res[0].Event.ID
		}()
	}
	wg.Wait()
	for _, id := range ids {
		if id != ids[0] {
			t.Fatalf("one key gave two ids: %v", ids)
		}
	}
	if n, _ := s.Count(ctx, Filter{}); n != 1 {
		t.Errorf("%d records, want 1", n)
	}
}

// Parties, causes, and person targets are found by id or email for those
// who see people, and only by ref for those who do not.
func TestPeopleFiltersAndCauses(t *testing.T) {
	s := open(t)
	ctx := context.Background()
	m := s.Masker()
	share := example(t, "sharepoint-share")
	prompt := example(t, "copilot-prompt")
	tool := example(t, "copilot-tool")
	for _, ev := range []annals.Event{share, prompt, tool} {
		if _, _, err := put(t, s, ev); err != nil {
			t.Fatal(err)
		}
	}
	guest := share.Parties[0]
	count := func(f Filter) int {
		t.Helper()
		n, err := s.Count(ctx, f)
		if err != nil {
			t.Fatal(err)
		}
		return n
	}
	if n := count(Filter{Party: guest.Email}); n != 1 {
		t.Errorf("by email: %d", n)
	}
	if n := count(Filter{Party: strings.ToUpper(guest.Email)}); n != 1 {
		t.Errorf("email is matched without case: %d", n)
	}
	if n := count(Filter{Party: guest.Email, Masked: true}); n != 0 {
		t.Errorf("a reader cannot find a person by email: %d", n)
	}
	if n := count(Filter{Party: m.Ref(guest.ID), Masked: true}); n != 1 {
		t.Errorf("a reader finds a person by ref: %d", n)
	}
	if n := count(Filter{Actor: share.Actor.ID, Masked: true}); n != 0 {
		t.Errorf("a reader cannot find a person actor by id: %d", n)
	}
	if n := count(Filter{Actor: m.Ref(share.Actor.ID), Masked: true}); n != 1 {
		t.Errorf("a reader finds a person actor by ref: %d", n)
	}
	if n := count(Filter{Actor: tool.Actor.ID, Masked: true}); n != 1 {
		t.Errorf("an agent is found by its id: %d", n)
	}
	caused, err := s.Query(ctx, Filter{Source: prompt.Source, Cause: prompt.EventKey})
	if err != nil || len(caused.Events) != 1 || caused.Events[0].EventKey != tool.EventKey {
		t.Errorf("what the prompt caused: %v %+v", err, caused.Events)
	}
	cause, err := s.ByKey(ctx, tool.Source, tool.Cause)
	if err != nil || cause.EventKey != prompt.EventKey {
		t.Errorf("the tool call's cause: %v %+v", err, cause)
	}
	bs, err := s.Breakdown(ctx, Filter{Masked: true}, "actor_id", 10)
	if err != nil {
		t.Fatal(err)
	}
	for _, b := range bs {
		if b.Key == share.Actor.ID {
			t.Errorf("a reader's breakdown names a person: %+v", bs)
		}
	}
}
