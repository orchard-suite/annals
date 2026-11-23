// Package selfaudit records what Annals itself does as ordinary records
// under the source "annals": tokens issued and revoked, people shown to a
// caller who may see them, ids an admin tied to a person or untied, and
// records deleted by retention.
//
// Each record passes the same validator every posted event passes and goes
// straight into the store. Writing one never holds up or fails the action it
// records: the write runs on its own, and a failure is logged and counted,
// then dropped.
package selfaudit

import (
	"context"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/orchard-suite/platformkit/v3/annals"

	"github.com/orchard-suite/annals/internal/people"
	"github.com/orchard-suite/annals/internal/store"
)

// Source is the source Annals' own records carry.
const Source = "annals"

// The verbs Annals records about itself.
const (
	VerbTokenIssue     = "token.issue"
	VerbTokenRevoke    = "token.revoke"
	VerbPeopleView     = "people.view"
	VerbRetentionPrune = "retention.prune"
	VerbPeopleLink     = "people.link"
	VerbPeopleUnlink   = "people.unlink"
)

// The doors a person or a token can see people through, as the people.view
// record's door label names them.
const (
	DoorBoard = "board"
	DoorAPI   = "api"
	DoorMCP   = "mcp"
)

// Outcomes of one write, as the self-record metric counts them.
const (
	OutcomeStored    = "stored"
	OutcomeDuplicate = "duplicate"
	OutcomeRefused   = "refused"
	OutcomeFailed    = "failed"
)

// writeTimeout bounds one write, so a stuck store cannot pile up writers.
const writeTimeout = 10 * time.Second

// Store is what the auditor writes to.
type Store interface {
	Put(ctx context.Context, evs []annals.Event) ([]store.Result, error)
	Masker() people.Masker
}

// Counter counts each write by verb and outcome.
type Counter interface {
	SelfRecord(ctx context.Context, verb, outcome string)
}

// Auditor writes Annals' own records.
type Auditor struct {
	store   Store
	counter Counter
	log     *slog.Logger
	now     func() time.Time
	wg      sync.WaitGroup

	// viewed holds today's people.view keys already written, so a busy
	// caller costs one store lookup a day, not one per page.
	mu     sync.Mutex
	day    string
	viewed map[string]bool
}

func New(st Store, counter Counter, log *slog.Logger) *Auditor {
	return &Auditor{store: st, counter: counter, log: log, now: time.Now, viewed: map[string]bool{}}
}

// Wait returns once every write started so far has finished.
func (a *Auditor) Wait() { a.wg.Wait() }

// TokenIssued records that by, the admin's email, issued tok.
func (a *Auditor) TokenIssued(ctx context.Context, by string, tok store.Token) {
	a.token(ctx, VerbTokenIssue, by, tok)
}

// TokenRevoked records that by, the admin's email, revoked tok.
func (a *Auditor) TokenRevoked(ctx context.Context, by string, tok store.Token) {
	a.token(ctx, VerbTokenRevoke, by, tok)
}

func (a *Auditor) token(ctx context.Context, verb, by string, tok store.Token) {
	a.write(ctx, annals.Event{
		EventKey: verb + ":" + tok.ID,
		Actor:    person(by),
		Action: annals.Action{
			Verb:    verb,
			Target:  annals.Target{System: Source, Kind: "token", ID: tok.ID, Title: tok.Name},
			Outcome: "ok",
		},
		Labels: map[string]string{"source": tok.Source, "reads": tok.Reads},
	}, nil)
}

// Linked records that by, the admin's email, tied the id to the person to.
func (a *Auditor) Linked(ctx context.Context, by, id, to string) {
	a.link(ctx, VerbPeopleLink, by, id, to)
}

// Unlinked records that by, the admin's email, untied the id from the person from.
func (a *Auditor) Unlinked(ctx context.Context, by, id, from string) {
	a.link(ctx, VerbPeopleUnlink, by, id, from)
}

// link names the person as the target and the id as a party, both person
// fields, so a reader sees only refs.
func (a *Auditor) link(ctx context.Context, verb, by, id, to string) {
	a.write(ctx, annals.Event{
		Actor: person(by),
		Action: annals.Action{
			Verb:    verb,
			Target:  annals.Target{System: Source, Kind: annals.PersonKind, ID: to},
			Outcome: "ok",
		},
		Parties: []annals.Party{{Role: "alias", Kind: annals.PersonKind, ID: id}},
	}, nil)
}

// Viewer is who was shown people: a person signed in at the front door, or
// the token an API or MCP caller holds.
type Viewer struct {
	actor     annals.Actor
	authority *annals.Authority
	// key names the viewer in the record's event_key, which every reader
	// sees, so a person is named there only by their ref.
	key string
}

// PersonViewer is a person the front door signed in, by email.
func (a *Auditor) PersonViewer(email string) Viewer {
	return Viewer{actor: person(email), key: a.store.Masker().Filed(email)}
}

// TokenViewer is the caller holding tok.
func TokenViewer(tok store.Token) Viewer {
	return Viewer{
		actor:     annals.Actor{Kind: "system", ID: tok.ID, Name: tok.Name},
		authority: &annals.Authority{TokenID: tok.ID},
		key:       tok.ID,
	}
}

// PeopleViewed records that v was shown people at level through door. It
// writes at most one record per viewer, level, and UTC day; the event_key
// makes the store keep one even across restarts.
func (a *Auditor) PeopleViewed(ctx context.Context, v Viewer, level people.Level, door string) {
	if !level.SeesPeople() {
		return
	}
	now := a.now().UTC()
	day := now.Format(time.DateOnly)
	key := VerbPeopleView + ":" + v.key + ":" + level.String() + ":" + day
	a.mu.Lock()
	if a.day != day {
		a.day, a.viewed = day, map[string]bool{}
	}
	seen := a.viewed[key]
	a.viewed[key] = true
	a.mu.Unlock()
	if seen {
		return
	}
	a.write(ctx, annals.Event{
		At:        now,
		EventKey:  key,
		Actor:     v.actor,
		Authority: v.authority,
		Action: annals.Action{
			Verb:    VerbPeopleView,
			Target:  annals.Target{System: Source, Kind: "view", ID: "people", Title: "People in records"},
			Outcome: "ok",
		},
		Labels: map[string]string{"level": level.String(), "door": door},
	}, func() {
		// A key that was not stored is tried again on the next view.
		a.mu.Lock()
		if a.day == day {
			delete(a.viewed, key)
		}
		a.mu.Unlock()
	})
}

// Pruned records that retention deleted n records of source older than
// cutoff. A pass that deleted nothing records nothing.
func (a *Auditor) Pruned(ctx context.Context, source string, n int64, cutoff time.Time) {
	if n <= 0 {
		return
	}
	a.write(ctx, annals.Event{
		Actor: annals.Actor{Kind: "system", ID: Source},
		Action: annals.Action{
			Verb:    VerbRetentionPrune,
			Target:  annals.Target{System: Source, Kind: "source", ID: source},
			Outcome: "ok",
		},
		Change: &annals.Change{Summary: fmt.Sprintf("deleted %d records older than %s", n, cutoff.UTC().Format(time.RFC3339))},
	}, nil)
}

func person(email string) annals.Actor {
	return annals.Actor{Kind: annals.PersonKind, ID: email, Email: email}
}

// write validates ev and stores it on its own goroutine, so the caller goes
// on at once. lost, when set, runs when the record was not stored.
func (a *Auditor) write(ctx context.Context, ev annals.Event, lost func()) {
	verb := ev.Action.Verb
	ev.Source = Source
	if ev.At.IsZero() {
		ev.At = a.now().UTC()
	}
	if err := ev.Validate(Source); err != nil {
		a.counter.SelfRecord(ctx, verb, OutcomeRefused)
		a.log.ErrorContext(ctx, "self record refused", "verb", verb, "reason", annals.Reason(err), "detail", err.Error())
		if lost != nil {
			lost()
		}
		return
	}
	// The write outlives the request it records, but keeps its trace.
	ctx = context.WithoutCancel(ctx)
	a.wg.Add(1)
	go func() {
		defer a.wg.Done()
		wctx, cancel := context.WithTimeout(ctx, writeTimeout)
		defer cancel()
		res, err := a.store.Put(wctx, []annals.Event{ev})
		switch {
		case err != nil:
			a.counter.SelfRecord(ctx, verb, OutcomeFailed)
			a.log.ErrorContext(ctx, "self record not stored", "verb", verb, "error", err)
			if lost != nil {
				lost()
			}
		case len(res) == 1 && res[0].Duplicate:
			a.counter.SelfRecord(ctx, verb, OutcomeDuplicate)
			a.log.DebugContext(ctx, "self record already stored", "verb", verb, "event", res[0].Event.ID)
		default:
			a.counter.SelfRecord(ctx, verb, OutcomeStored)
			var id string
			if len(res) == 1 {
				id = res[0].Event.ID
			}
			a.log.InfoContext(ctx, "self record stored", "verb", verb, "event", id)
		}
	}()
}
