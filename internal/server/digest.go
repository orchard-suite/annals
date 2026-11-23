package server

import (
	"context"
	"net/url"
	"slices"
	"strings"
	"time"

	"github.com/orchard-suite/platformkit/v3/annals"

	"github.com/orchard-suite/annals/internal/selfaudit"
	"github.com/orchard-suite/annals/internal/store"
)

// foldAt is how many times an actor must do one thing to one kind of target
// in a window before the board folds it into one routine line. Anything
// rarer, anything that failed or was refused, and anything a person did is
// listed as worth a look instead.
const foldAt = 5

// window is a span the digest covers and the step its pulse is cut into.
type window struct {
	Key, Label string
	Span, Step time.Duration
	// Tick labels every nth step on the pulse's axis.
	Tick int
}

var windows = []window{
	{"24h", "24 hours", 24 * time.Hour, time.Hour, 3},
	{"7d", "7 days", 7 * 24 * time.Hour, 6 * time.Hour, 4},
	{"30d", "30 days", 30 * 24 * time.Hour, 24 * time.Hour, 5},
}

// allTime is the window from the oldest record to now. Its span and step
// are set when the digest reads it.
var allTime = window{Key: "", Label: "all time"}

// windowFor is the window a since parameter names: absent means fallback,
// empty means all time.
func windowFor(q url.Values, fallback string) window {
	if !q.Has("since") {
		q = url.Values{"since": {fallback}}
	}
	key := q.Get("since")
	if key == "" {
		return allTime
	}
	for _, w := range windows {
		if w.Key == key {
			return w
		}
	}
	return windows[0]
}

// stretch sets an all-time window's span from its oldest record, in about
// thirty steps of whole hours or days.
func stretch(w window, oldest, now time.Time) window {
	span := max(now.Sub(oldest), time.Hour)
	step := time.Hour
	if span > 48*time.Hour {
		step = 24 * time.Hour
	}
	for span/step > 45 {
		step += 24 * time.Hour
	}
	if step < 24*time.Hour {
		for span/step > 45 {
			step += time.Hour
		}
	}
	w.Step = step
	w.Span = (span/step + 1) * step
	w.Tick = max(1, int(w.Span/step)/6)
	return w
}

// fold is one pattern as the board shows it: the newest record, masked for
// the caller, and how often the pattern happened.
type fold struct {
	Latest annals.Event
	// LatestBad is the newest record that failed or was refused, if any.
	LatestBad *annals.Event
	Count     int
	Bad       int
	Failed    int
	Refused   int
	Targets   int
	Steps     []int
	// Peak is the largest step of any pattern in its list, so every
	// sparkline in the list shares one scale.
	Peak int
	// Records lists every record of the pattern in the window.
	Records string
	// Why says why a pattern is worth a look; empty for routine.
	Why reason
	// Usual is how many times the pattern happened in the window before.
	Usual int
	// Actors is how many actors a rare line folds together; 1 for one.
	Actors int
	// Again is set when the line above in its group is the same actor's.
	Again bool
	// First is set on the oldest record of a target's page.
	First bool
}

// reason is why the board lists a pattern as worth a look, most urgent
// first.
type reason int

const (
	whyNone reason = iota
	whyFailed
	whyRefused
	whySpike
	whyNew
	whyPerson
	whyRare
)

// spikeAt is how many times its usual a pattern must run, and spikeMin how
// many records at least, before the board calls it a spike.
const (
	spikeAt  = 3
	spikeMin = 20
)

// Group is the heading for the patterns listed for this reason.
func (r reason) Group() string {
	return [...]string{"", "Failed or refused", "Failed or refused", "Busier than usual", "New in this window", "Done by a person", "Rare"}[r]
}

// Word is the reason as the board labels it.
func (r reason) Word() string {
	return [...]string{"", "failed", "refused", "spike", "new", "by a person", "rare"}[r]
}

// busy is one actor's share of a window.
type busy struct {
	Actor annals.Actor
	Count int
	Bad   int
}

// busyRow is one actor's row in a list scaled to the busiest.
type busyRow struct {
	B   busy
	Max int
}

// pulseStep is one bar of the pulse: routine, the rest, and the bad.
type pulseStep struct {
	Label, Tip, Href string
	// Minor labels hide where the chart is narrow.
	Minor                     bool
	Routine, Rest, Bad, Total int
}

// digest is a window of records seen as the board's front page sees them:
// how much happened, how much of it was routine, and what is worth a look.
type digest struct {
	Window window
	// Since is where the window starts, and Until where a window asked for
	// by its times ends.
	Since, Until time.Time
	Total        int
	Bad          int
	Failed       int
	Refused      int
	Routine      int
	Actors       int
	Max          int
	Pulse        []pulseStep
	// Alarm is the patterns that failed or were refused, newest first.
	Alarm  []fold
	Look   []fold
	Folded []fold
	// Busy is every actor in the window, busiest first.
	Busy []busy
	// Views and Viewers are the board's own notes that someone looked at
	// people, and how many did.
	Views, Viewers int
	// Scope is the filter's query, without the window, for links.
	Scope url.Values
}

// Within says the window as words: "in the last 7 days", or "since <date>"
// for all time.
func (d digest) Within() string {
	switch {
	case !d.Until.IsZero():
		from, to := d.Since.In(zone), d.Until.In(zone)
		if from.Format("2006-01-02") == to.Add(-time.Second).Format("2006-01-02") {
			return "on " + from.Format("Mon Jan 2") + ", " + from.Format("15:04") + " to " + to.Format("15:04")
		}
		return "from " + from.Format("Mon Jan 2 15:04") + " to " + to.Format("Mon Jan 2 15:04")
	case d.Window.Span == 0 && d.Window.Key != "":
		return "since " + d.Since.In(zone).Format("Mon Jan 2 15:04")
	case d.Window.Key == "":
		if d.Since.IsZero() {
			return "over all time"
		}
		return "since " + d.Since.In(zone).Format("January 2")
	}
	return "in the last " + d.Window.Label
}

// RoutinePct is the share of the window's records that were routine.
func (d digest) RoutinePct() int {
	if d.Total == 0 {
		return 0
	}
	return 100 * d.Routine / d.Total
}

// lookGroup is the patterns worth a look for one reason: the first few
// shown, the rest one click away.
type lookGroup struct {
	Name   string
	Shown  []fold
	Hidden []fold
}

// groupShown is how many patterns a group shows before folding the rest.
// Failures and refusals always show in full.
const groupShown = 3

// Groups is Look by reason, most urgent first.
func (d digest) Groups() []lookGroup {
	var out []lookGroup
	for _, f := range d.Look {
		name := f.Why.Group()
		if len(out) == 0 || out[len(out)-1].Name != name {
			out = append(out, lookGroup{Name: name})
		}
		g := &out[len(out)-1]
		if n := len(g.Shown) + len(g.Hidden); n > 0 {
			prev := g.Shown
			if len(g.Hidden) > 0 {
				prev = g.Hidden
			}
			f.Again = f.Actors == 1 && prev[len(prev)-1].Actors == 1 && prev[len(prev)-1].Latest.Actor.ID == f.Latest.Actor.ID
		}
		if len(g.Shown) < groupShown || f.Bad > 0 {
			g.Shown = append(g.Shown, f)
		} else {
			g.Hidden = append(g.Hidden, f)
		}
	}
	return out
}

// tally is one part of the verdict: how many patterns, said how.
type tally struct {
	Text, Class string
	Last        bool
}

// Tallies is what is worth a look, by reason, as parts of a sentence:
// "2 failed, 4 busier than usual, and 3 new".
func (d digest) Tallies() []tally {
	n := map[reason]int{}
	for _, f := range append(append(slices.Clone(d.Alarm), d.Look...), d.Folded...) {
		n[f.Why]++
	}
	var out []tally
	add := func(r reason, text, class string) {
		if n[r] > 0 {
			out = append(out, tally{Text: thousands(n[r]) + " " + text, Class: class})
		}
	}
	add(whyFailed, "failed", "red")
	add(whyRefused, "refused", "amber")
	add(whySpike, "busier than usual", "")
	add(whyNew, "new", "")
	add(whyPerson, "by a person", "")
	add(whyRare, "rare", "")
	if len(out) > 0 {
		out[len(out)-1].Last = true
	}
	return out
}

// LookCount is how many records the Look list stands for.
func (d digest) LookCount() int {
	n := 0
	for _, f := range d.Look {
		n += f.Count
	}
	return n
}

// digest reads the window ending now for f, as the caller may see it.
func (s *Server) digest(ctx context.Context, v view, f store.Filter, scope url.Values, w window) (digest, error) {
	now := time.Now()
	whole := w.Span == 0
	switch {
	case whole:
		oldest, err := s.store.Oldest(ctx, v.filter(f))
		if err != nil {
			return digest{}, err
		}
		if oldest.IsZero() {
			oldest = now
		}
		w = stretch(w, oldest, now)
		f.Since = oldest.Truncate(time.Hour)
		if w.Step >= 24*time.Hour {
			f.Since = dayStart(oldest)
		}
	case w.Step == time.Hour:
		f.Since = now.Add(-w.Span).Truncate(time.Hour).Add(time.Hour)
	default:
		// Steps of hours or days start at midnight in the board's zone.
		start := now.Add(-w.Span)
		f.Since = dayStart(start)
		for !f.Since.After(start) {
			f.Since = f.Since.Add(w.Step)
		}
	}
	f = v.filter(f)
	steps, err := s.store.Pulse(ctx, f, now, w.Step)
	if err != nil {
		return digest{}, err
	}
	pats, err := s.store.Patterns(ctx, f, now, w.Step, 500)
	if err != nil {
		return digest{}, err
	}
	// The window before, to tell the new and the spiking from the usual.
	usual := map[string]int{}
	if !whole {
		before := f
		before.Since, before.Until = f.Since.Add(-w.Span), f.Since
		if usual, err = s.store.PatternCounts(ctx, before, 2000); err != nil {
			return digest{}, err
		}
	}
	d := digest{Window: w, Since: f.Since, Scope: scope}
	routine := make([]int, len(steps))
	actors := map[string]*busy{}
	for _, p := range pats {
		fd := fold{
			Latest: v.event(p.Latest), Count: p.Count, Failed: p.Failed, Refused: p.Refused,
			Bad: p.Failed + p.Refused, Targets: p.Targets, Steps: p.Steps, Usual: usual[p.Key], Actors: 1,
		}
		if p.LatestBad != nil {
			bad := v.event(*p.LatestBad)
			fd.LatestBad = &bad
		}
		fd.Records = recordsURL(scope, w.Key, fd.Latest)
		fd.Why = why(p, fd.Usual, len(usual) > 0)
		if fd.Why == whyPerson && f.Actor != "" {
			// On a person's own page, being a person says nothing.
			fd.Why = whyNone
			if p.Count < foldAt {
				fd.Why = whyRare
			}
		}
		b := actors[fd.Latest.Actor.ID]
		if b == nil {
			b = &busy{Actor: fd.Latest.Actor}
			actors[fd.Latest.Actor.ID] = b
		}
		b.Count += p.Count
		b.Bad += fd.Bad
		if isPeopleView(p.Latest) {
			// The board's own notes that someone looked at people are one
			// line, however many looked.
			d.Views += p.Count
			d.Viewers++
			d.Routine += p.Count
			for i, n := range p.Steps {
				routine[i] += n
			}
			continue
		}
		if fd.Why == whyNone || fd.Why == whySpike {
			// A machine running hot is still machine work: the pulse
			// paints it faint, and the list says it is hot.
			for i, n := range p.Steps {
				routine[i] += n
			}
		}
		switch fd.Why {
		case whyNone, whySpike:
			// A spike is machine work running hot: it folds with the
			// routine, flagged.
			d.Folded = append(d.Folded, fd)
			d.Routine += p.Count
		case whyFailed, whyRefused:
			d.Alarm = append(d.Alarm, fd)
		default:
			d.Look = append(d.Look, fd)
		}
	}
	d.Look = foldRare(d.Look, scope, w.Key)
	// A line running hot leads the routine column.
	slices.SortStableFunc(d.Folded, func(a, b fold) int {
		if (a.Why == whySpike) != (b.Why == whySpike) {
			if a.Why == whySpike {
				return -1
			}
			return 1
		}
		return 0
	})
	slices.SortStableFunc(d.Alarm, func(a, b fold) int { return b.LatestBadAt().Compare(a.LatestBadAt()) })
	// The most urgent reason first, then the newest.
	slices.SortStableFunc(d.Look, func(a, b fold) int {
		if a.Why != b.Why {
			return int(a.Why) - int(b.Why)
		}
		return b.Latest.At.Compare(a.Latest.At)
	})
	peak := 0
	for _, f := range d.Folded {
		peak = max(peak, slices.Max(f.Steps))
	}
	for i := range d.Folded {
		d.Folded[i].Peak = peak
	}
	d.Actors = len(actors)
	for _, b := range actors {
		d.Busy = append(d.Busy, *b)
	}
	slices.SortFunc(d.Busy, func(a, b busy) int {
		if a.Count != b.Count {
			return b.Count - a.Count
		}
		return strings.Compare(a.Actor.ID, b.Actor.ID)
	})
	for i, st := range steps {
		bad := st.Failed + st.Refused
		ro := min(routine[i], st.Total-bad)
		at := f.Since.Add(time.Duration(i) * w.Step)
		ps := pulseStep{Routine: ro, Bad: bad, Rest: st.Total - bad - ro, Total: st.Total, Tip: stepTip(at, w.Step, st), Href: stepURL(scope, at, w.Step)}
		if i%w.Tick == 0 {
			ps.Label = stepLabel(at, w.Step)
			ps.Minor = (i/w.Tick)%2 == 1
		}
		if i == len(steps)-1 && !at.Add(w.Step).Before(now) {
			ps.Label, ps.Tip, ps.Minor = "now", ps.Tip+", so far", false
		}
		d.Pulse = append(d.Pulse, ps)
		d.Total += st.Total
		d.Failed += st.Failed
		d.Refused += st.Refused
		d.Max = max(d.Max, st.Total)
	}
	d.Bad = d.Failed + d.Refused
	return d, nil
}

// isPeopleView is the board's own note that someone looked at people.
func isPeopleView(ev annals.Event) bool {
	return ev.Source == selfaudit.Source && ev.Action.Verb == selfaudit.VerbPeopleView
}

// BadOnes is the patterns that failed or were refused.
func (d digest) BadOnes() []fold { return d.Alarm }

// LatestBadAt is when the pattern last failed or was refused.
func (f fold) LatestBadAt() time.Time {
	if f.LatestBad != nil {
		return f.LatestBad.At
	}
	return f.Latest.At
}

// manyAt is how many actors doing one rare thing make one line of them.
const manyAt = 3

// foldRare folds rare lines that are one action by many actors (many
// collectors each changing one icon) into one line, so rare means rare.
func foldRare(look []fold, scope url.Values, since string) []fold {
	key := func(f fold) string {
		a := f.Latest.Action
		return f.Latest.Source + "|" + a.Verb + "|" + a.Target.System + "|" + a.Target.Kind
	}
	n := map[string]int{}
	for _, f := range look {
		if f.Why == whyRare {
			n[key(f)]++
		}
	}
	var out []fold
	at := map[string]int{}
	for _, f := range look {
		k := key(f)
		if f.Why != whyRare || n[k] < manyAt {
			out = append(out, f)
			continue
		}
		i, ok := at[k]
		if !ok {
			at[k] = len(out)
			f.Actors = 1
			f.Steps = slices.Clone(f.Steps)
			q := url.Values{}
			for k, vs := range scope {
				q[k] = vs
			}
			a := f.Latest.Action
			q.Set("source", f.Latest.Source)
			q.Set("verb", a.Verb)
			q.Set("system", a.Target.System)
			q.Set("kind", a.Target.Kind)
			q.Set("since", since)
			f.Records = "/records?" + q.Encode()
			out = append(out, f)
			continue
		}
		g := &out[i]
		g.Actors++
		g.Count += f.Count
		g.Targets += f.Targets
		for j, c := range f.Steps {
			g.Steps[j] += c
		}
		if f.Latest.At.After(g.Latest.At) {
			g.Latest = f.Latest
		}
	}
	return out
}

// why is the reason a pattern is worth a look, or routine. A pattern is new
// only when the window before had records at all.
func why(p store.Pattern, usual int, before bool) reason {
	switch {
	case p.Failed > 0:
		return whyFailed
	case p.Refused > 0:
		return whyRefused
	case usual > 0 && p.Count >= spikeMin && p.Count >= spikeAt*usual:
		return whySpike
	case before && usual == 0 && p.Count >= foldAt:
		return whyNew
	// The board's own note that someone looked at people is a read, not
	// something a person did.
	case isPeopleView(p.Latest):
		return whyNone
	case p.Latest.Actor.Kind == annals.PersonKind:
		return whyPerson
	case p.Count < foldAt:
		return whyRare
	}
	return whyNone
}

func stepLabel(at time.Time, step time.Duration) string {
	at = at.In(zone)
	switch {
	case step >= 24*time.Hour:
		// Name the day by its middle, so a clock change does not move a
		// day's label to the day before.
		return at.Add(12 * time.Hour).Format("Jan 2")
	case step >= 6*time.Hour:
		return at.Format("Mon")
	}
	return at.Format("15:04")
}

func stepTip(at time.Time, step time.Duration, st store.Step) string {
	at = at.In(zone)
	when := at.Format("Mon Jan 2 15:04")
	if step >= 24*time.Hour {
		when = at.Format("Mon Jan 2")
	}
	tip := when + ": " + itoa(st.Total) + " records"
	if st.Failed > 0 {
		tip += ", " + itoa(st.Failed) + " failed"
	}
	if st.Refused > 0 {
		tip += ", " + itoa(st.Refused) + " refused"
	}
	return tip
}

// stepURL is the records page for one step of a pulse, within scope.
func stepURL(scope url.Values, at time.Time, step time.Duration) string {
	q := url.Values{}
	for k, vs := range scope {
		q[k] = vs
	}
	q.Set("since", at.UTC().Format(time.RFC3339))
	q.Set("until", at.Add(step).UTC().Format(time.RFC3339))
	return "/records?" + q.Encode()
}

// recordsURL is the records page filtered to one pattern within scope.
func recordsURL(scope url.Values, since string, ev annals.Event) string {
	q := url.Values{}
	for k, vs := range scope {
		q[k] = vs
	}
	q.Set("actor", ev.Actor.ID)
	q.Set("verb", ev.Action.Verb)
	q.Set("system", ev.Action.Target.System)
	q.Set("kind", ev.Action.Target.Kind)
	q.Set("since", since)
	return "/records?" + q.Encode()
}
