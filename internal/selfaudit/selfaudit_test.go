package selfaudit

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/orchard-suite/platformkit/v3/annals"

	"github.com/orchard-suite/annals/internal/people"
	"github.com/orchard-suite/annals/internal/store"
	"github.com/orchard-suite/annals/internal/store/storetest"
)

// fakeStore records what it was asked to put. block, when set, holds every
// Put until it is closed; fail makes every Put fail.
type fakeStore struct {
	mu    sync.Mutex
	put   []annals.Event
	block chan struct{}
	fail  error
}

func (f *fakeStore) Put(_ context.Context, evs []annals.Event) ([]store.Result, error) {
	if f.block != nil {
		<-f.block
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.put = append(f.put, evs...)
	if f.fail != nil {
		return nil, f.fail
	}
	out := make([]store.Result, len(evs))
	for i, ev := range evs {
		out[i] = store.Result{Event: ev}
	}
	return out, nil
}

func (f *fakeStore) Masker() people.Masker { return people.NewMasker([]byte("test key")) }

func (f *fakeStore) events() []annals.Event {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]annals.Event(nil), f.put...)
}

type counted struct{ verb, outcome string }

type fakeCounter struct {
	mu  sync.Mutex
	got []counted
}

func (c *fakeCounter) SelfRecord(_ context.Context, verb, outcome string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.got = append(c.got, counted{verb, outcome})
}

func (c *fakeCounter) all() []counted {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]counted(nil), c.got...)
}

func newAuditor(st Store) (*Auditor, *fakeCounter, *bytes.Buffer) {
	var logs bytes.Buffer
	c := &fakeCounter{}
	return New(st, c, slog.New(slog.NewJSONHandler(&logs, nil))), c, &logs
}

var tok = store.Token{ID: "01JBX7Y8Z9ABCDEFGHJKMNPQRS", Name: "beacon prod", Source: "beacon", Reads: "reader"}

// A store that never answers does not hold up the action being recorded.
func TestAWriteNeverBlocks(t *testing.T) {
	st := &fakeStore{block: make(chan struct{})}
	a, c, _ := newAuditor(st)
	done := make(chan struct{})
	go func() {
		a.TokenIssued(context.Background(), "admin@example.com", tok)
		a.PeopleViewed(context.Background(), a.PersonViewer("hr@example.com"), people.PII, DoorBoard)
		a.Pruned(context.Background(), "ion", 3, time.Now())
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("recording waited on the store")
	}
	close(st.block)
	a.Wait()
	if len(st.events()) != 3 || len(c.all()) != 3 {
		t.Errorf("put %d, counted %v", len(st.events()), c.all())
	}
}

// A store that fails is logged and counted, and the caller never hears of it.
func TestAFailedWriteIsLoggedAndCounted(t *testing.T) {
	st := &fakeStore{fail: errors.New("clickhouse is down")}
	a, c, logs := newAuditor(st)
	a.TokenRevoked(context.Background(), "admin@example.com", tok)
	a.Wait()
	if got := c.all(); len(got) != 1 || got[0] != (counted{VerbTokenRevoke, OutcomeFailed}) {
		t.Errorf("counted %v", got)
	}
	var line map[string]any
	if err := json.Unmarshal(bytes.TrimSpace(logs.Bytes()), &line); err != nil {
		t.Fatalf("log is not one JSON line: %q", logs.String())
	}
	if line["msg"] != "self record not stored" || line["verb"] != VerbTokenRevoke || line["error"] != "clickhouse is down" {
		t.Errorf("log: %v", line)
	}

	// A people.view that was not stored is tried again on the next view.
	v := a.PersonViewer("hr@example.com")
	a.PeopleViewed(context.Background(), v, people.PII, DoorBoard)
	a.Wait()
	a.PeopleViewed(context.Background(), v, people.PII, DoorBoard)
	a.Wait()
	if n := len(st.events()); n != 3 {
		t.Errorf("put %d times, want the view tried twice", n)
	}
}

// A record the validator refuses never reaches the store.
func TestARefusedRecordIsCounted(t *testing.T) {
	st := &fakeStore{}
	a, c, logs := newAuditor(st)
	bad := tok
	bad.Name = "for owner@example.com"
	a.TokenIssued(context.Background(), "admin@example.com", bad)
	a.Wait()
	if len(st.events()) != 0 {
		t.Error("a refused record was stored")
	}
	if got := c.all(); len(got) != 1 || got[0] != (counted{VerbTokenIssue, OutcomeRefused}) {
		t.Errorf("counted %v", got)
	}
	if !strings.Contains(logs.String(), `"reason":"email_in_text"`) {
		t.Errorf("log: %s", logs.String())
	}
}

// One people.view per viewer, level, and UTC day, with a key inside the
// contract's limit that names a person only by ref.
func TestPeopleViewOncePerDay(t *testing.T) {
	st := &fakeStore{}
	a, _, _ := newAuditor(st)
	day := time.Date(2026, 11, 7, 23, 59, 0, 0, time.UTC)
	a.now = func() time.Time { return day }
	long := strings.Repeat("a", annals.MaxShort-len("@example.com")) + "@example.com"
	v := a.PersonViewer(long)
	for range 3 {
		a.PeopleViewed(context.Background(), v, people.PII, DoorBoard)
	}
	a.PeopleViewed(context.Background(), v, people.Admin, DoorBoard)
	a.PeopleViewed(context.Background(), v, people.Reader, DoorBoard)
	a.PeopleViewed(context.Background(), TokenViewer(tok), people.PII, DoorAPI)
	day = day.Add(2 * time.Minute)
	a.PeopleViewed(context.Background(), v, people.PII, DoorBoard)
	a.Wait()
	evs := st.events()
	if len(evs) != 4 {
		t.Fatalf("put %d records, want pii, admin, the token, and the next day's pii", len(evs))
	}
	ref := st.Masker().Ref(long)
	keys := map[string]bool{}
	for _, ev := range evs {
		keys[ev.EventKey] = true
		if len(ev.EventKey) > annals.MaxShort || strings.Contains(ev.EventKey, "@") {
			t.Errorf("key %q", ev.EventKey)
		}
	}
	for _, want := range []string{
		"people.view:" + ref + ":pii:2026-11-07",
		"people.view:" + ref + ":admin:2026-11-07",
		"people.view:" + tok.ID + ":pii:2026-11-07",
		"people.view:" + ref + ":pii:2026-11-08",
	} {
		if !keys[want] {
			t.Errorf("no record keyed %q in %v", want, keys)
		}
	}
}

func TestPrunedRecordsOnlyWhatWent(t *testing.T) {
	st := &fakeStore{}
	a, _, _ := newAuditor(st)
	cutoff := time.Date(2025, 11, 7, 12, 0, 0, 0, time.UTC)
	a.Pruned(context.Background(), "ion", 0, cutoff)
	a.Pruned(context.Background(), "ion", 42, cutoff)
	a.Wait()
	evs := st.events()
	if len(evs) != 1 {
		t.Fatalf("put %d records", len(evs))
	}
	ev := evs[0]
	if ev.Actor != (annals.Actor{Kind: "system", ID: "annals"}) || ev.Action.Verb != VerbRetentionPrune ||
		ev.Action.Target != (annals.Target{System: "annals", Kind: "source", ID: "ion"}) ||
		ev.Change == nil || ev.Change.Summary != "deleted 42 records older than 2025-11-07T12:00:00Z" {
		t.Errorf("record: %+v %+v", ev, ev.Change)
	}
}

// Every kind lands in a real store under source annals, and a reader's view
// of it hides the person.
func TestRecordsLandInTheStore(t *testing.T) {
	st := storetest.Open(t)
	a, c, _ := newAuditor(st)
	ctx := context.Background()
	a.TokenIssued(ctx, "admin@example.com", tok)
	a.TokenRevoked(ctx, "admin@example.com", tok)
	a.PeopleViewed(ctx, a.PersonViewer("hr@example.com"), people.PII, DoorBoard)
	a.Pruned(ctx, "ion", 2, time.Now().Add(-30*24*time.Hour))
	a.Wait()
	page, err := st.Query(ctx, store.Filter{Source: Source})
	if err != nil {
		t.Fatal(err)
	}
	verbs := map[string]bool{}
	for _, ev := range page.Events {
		verbs[ev.Action.Verb] = true
		masked := st.Masker().Mask(ev)
		if raw, _ := json.Marshal(masked); strings.Contains(string(raw), "@example.com") {
			t.Errorf("a reader sees a person in %s", raw)
		}
	}
	for _, v := range []string{VerbTokenIssue, VerbTokenRevoke, VerbPeopleView, VerbRetentionPrune} {
		if !verbs[v] {
			t.Errorf("no %s record", v)
		}
	}
	for _, got := range c.all() {
		if got.outcome != OutcomeStored {
			t.Errorf("counted %v", got)
		}
	}
}
