package server

import (
	"context"
	"net/url"
	"slices"
	"time"

	"github.com/orchard-suite/platformkit/v3/annals"

	"github.com/orchard-suite/annals/internal/store"
)

// run is records of one actor doing one thing to one kind of target with
// one outcome, within one day, newest first. A stream shows a run as one
// line, so a machine that did the same thing every few minutes all day
// takes one line, even when its records interleave with another's.
type run struct {
	Events  []annals.Event
	Targets int
	// Hours counts the run's records in each hour of its day.
	Hours []int
	// Here is set when the stream is one target's, so the line need not
	// name it.
	Here bool
	// Actors is how many actors the run folds together, when more than one.
	Actors int
}

// Spread is how many hours of its day the run touched.
func (r run) Spread() int {
	n := 0
	for _, h := range r.Hours {
		if h > 0 {
			n++
		}
	}
	return n
}

// PerHour is the run's records per hour it was busy, rounded.
func (r run) PerHour() int { return (len(r.Events) + r.Spread()/2) / max(1, r.Spread()) }

// First is the run's newest record.
func (r run) First() annals.Event { return r.Events[0] }

// Last is the run's oldest record.
func (r run) Last() annals.Event { return r.Events[len(r.Events)-1] }

// runShown is how many records an opened run lists before linking to the
// rest.
const runShown = 12

// Shown is the records an opened run lists.
func (r run) Shown() []annals.Event { return r.Events[:min(len(r.Events), runShown)] }

// Rest is the records page holding every record of the run, when it holds
// more than it lists.
func (r run) Rest() string {
	if len(r.Events) <= runShown {
		return ""
	}
	first, last := r.First(), r.Last()
	q := url.Values{
		"verb": {first.Action.Verb}, "system": {first.Action.Target.System},
		"kind": {first.Action.Target.Kind}, "outcome": {first.Action.Outcome}, "every": {"1"},
		"since": {last.At.UTC().Format(time.RFC3339Nano)}, "until": {first.At.Add(time.Millisecond).UTC().Format(time.RFC3339Nano)},
	}
	if r.Actors <= 1 {
		q.Set("actor", first.Actor.ID)
	}
	return "/records?" + q.Encode()
}

// Count is how many records the run holds.
func (r run) Count() int { return len(r.Events) }

// runKey is what makes records one run: the pattern and the day.
func runKey(ev annals.Event) string {
	return ev.At.In(zone).Format("20060102") + "|" + ev.Actor.ID + "|" + ev.Action.Verb + "|" + ev.Action.Target.System + "|" +
		ev.Action.Target.Kind + "|" + ev.Action.Outcome
}

// runsOf folds records, newest first, into runs, ordered by their newest
// record. With fold off, every record is a run of one.
func runsOf(evs []annals.Event, fold bool) []run {
	var out []run
	at := map[string]int{}
	seen := map[string]map[string]bool{}
	for _, ev := range evs {
		k := runKey(ev)
		if i, ok := at[k]; fold && ok {
			out[i].Events = append(out[i].Events, ev)
			out[i].Hours[ev.At.In(zone).Hour()]++
			if !seen[k][ev.Action.Target.ID] {
				seen[k][ev.Action.Target.ID] = true
				out[i].Targets++
			}
			continue
		}
		at[k] = len(out)
		seen[k] = map[string]bool{ev.Action.Target.ID: true}
		r := run{Events: []annals.Event{ev}, Targets: 1, Hours: make([]int, 24)}
		r.Hours[ev.At.In(zone).Hour()]++
		out = append(out, r)
	}
	return out
}

// foldActors folds, within one day, the small runs of one action by many
// actors (collectors each changing one icon) into one run, as Today does.
func foldActors(runs []run) []run {
	key := func(r run) string {
		e := r.First()
		return e.At.In(zone).Format("20060102") + "|" + e.Source + "|" + e.Action.Verb + "|" + e.Action.Target.System + "|" + e.Action.Target.Kind + "|" + e.Action.Outcome
	}
	n := map[string]int{}
	for _, r := range runs {
		if r.Count() < foldAt {
			n[key(r)]++
		}
	}
	var out []run
	at := map[string]int{}
	for _, r := range runs {
		k := key(r)
		if r.Count() >= foldAt || n[k] < manyAt {
			out = append(out, r)
			continue
		}
		i, ok := at[k]
		if !ok {
			at[k] = len(out)
			r.Actors = 1
			r.Hours = slices.Clone(r.Hours)
			out = append(out, r)
			continue
		}
		g := &out[i]
		g.Actors++
		g.Targets += r.Targets
		g.Events = append(g.Events, r.Events...)
		for h, c := range r.Hours {
			g.Hours[h] += c
		}
	}
	for i := range out {
		if out[i].Actors > 1 {
			slices.SortStableFunc(out[i].Events, func(a, b annals.Event) int { return b.At.Compare(a.At) })
		}
	}
	return out
}

// streamRuns is how many runs a page of a stream shows, and streamReads how
// many records it reads to find them.
const (
	streamRuns  = 60
	streamReads = 1000
)

// stream reads one page of records matching f, as the caller may see them,
// folded into runs, and the address of the next page.
func (s *Server) stream(ctx context.Context, v view, f store.Filter, u *url.URL, fold bool) ([]dayGroup, string, error) {
	f.Limit = streamReads
	if !fold {
		f.Limit = store.DefaultLimit
	}
	page, err := s.store.Query(ctx, v.filter(f))
	if err != nil {
		return nil, "", err
	}
	runs := runsOf(v.events(page.Events), fold)
	if fold {
		runs = foldActors(runs)
	}
	next := page.Next
	if len(runs) > streamRuns {
		// Cut at a whole day, so every record newer than the oldest one
		// shown is shown, and the next page starts right after it.
		hour := runs[streamRuns-1].First().At.In(zone).Format("20060102")
		kept := runs[:0]
		var oldest annals.Event
		for _, r := range runs {
			if r.First().At.In(zone).Format("20060102") < hour {
				continue
			}
			kept = append(kept, r)
			if oldest.ID == "" || r.Last().At.Before(oldest.At) {
				oldest = r.Last()
			}
		}
		runs = kept
		next = store.CursorAfter(oldest)
	}
	if f.TargetID != "" {
		for i := range runs {
			runs[i].Here = true
		}
	}
	groups := byDay(runs)
	if f.Actor != "" {
		for i := range groups {
			groups[i].Self = true
		}
	}
	return groups, nextURL(u, next), nil
}

// firstOf is the newest record in groups, if there is one.
func firstOf(groups []dayGroup) (annals.Event, bool) {
	if len(groups) == 0 {
		return annals.Event{}, false
	}
	return groups[0].Runs[0].First(), true
}

// nearby is what happened around one record: how many like it, and the
// actor's records just before and after.
type nearby struct {
	Like     int
	Outcomes []store.Bucket
	// Seq is the outcome of each record like it, oldest first, with this
	// one marked.
	Seq    []likeStep
	LikeAt string
	Before []annals.Event
	After  []annals.Event
}

// likeStep is one record like the one on the page.
type likeStep struct {
	Outcome string
	Here    bool
	At      time.Time
}

// aroundSpan is how far either side of a record its neighbors are looked
// for.
const aroundSpan = 12 * time.Hour

// around reads the neighbors of ev, which the caller may already see.
func (s *Server) around(ctx context.Context, v view, ev annals.Event) (nearby, error) {
	from, to := ev.At.Add(-aroundSpan), ev.At.Add(aroundSpan)
	like := store.Filter{Actor: ev.Actor.ID, Verb: ev.Action.Verb, System: ev.Action.Target.System, TargetKind: ev.Action.Target.Kind, Since: from, Until: to}
	n, err := s.store.Count(ctx, v.filter(like))
	if err != nil {
		return nearby{}, err
	}
	outcomes, err := s.store.Breakdown(ctx, v.filter(like), "outcome", 3)
	if err != nil {
		return nearby{}, err
	}
	q := url.Values{
		"actor": {ev.Actor.ID}, "verb": {ev.Action.Verb}, "system": {ev.Action.Target.System}, "kind": {ev.Action.Target.Kind},
		"since": {from.UTC().Format(time.RFC3339)}, "until": {to.UTC().Format(time.RFC3339)},
	}
	likes, err := s.store.Query(ctx, v.filter(store.Filter{Actor: like.Actor, Verb: like.Verb, System: like.System, TargetKind: like.TargetKind, Since: from, Until: to, Limit: store.MaxLimit}))
	if err != nil {
		return nearby{}, err
	}
	var seq []likeStep
	for i := len(likes.Events) - 1; i >= 0; i-- {
		seq = append(seq, likeStep{Outcome: likes.Events[i].Action.Outcome, Here: likes.Events[i].ID == ev.ID, At: likes.Events[i].At})
	}
	near := nearby{Like: n, Outcomes: outcomes, Seq: seq, LikeAt: "/records?" + q.Encode()}
	before, err := s.store.Query(ctx, v.filter(store.Filter{Actor: ev.Actor.ID, Since: from, Until: ev.At, Limit: 3}))
	if err != nil {
		return nearby{}, err
	}
	after, err := s.store.Query(ctx, v.filter(store.Filter{Actor: ev.Actor.ID, Since: ev.At.Add(time.Millisecond), Until: to, Limit: store.MaxLimit}))
	if err != nil {
		return nearby{}, err
	}
	near.Before = v.events(before.Events)
	near.After = v.events(after.Events[max(0, len(after.Events)-3):])
	return near, nil
}
