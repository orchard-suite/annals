// Package gate scores the change gate by what changes did. It reads the
// change events Annals keeps and answers, for each week and effect family,
// how many asks were needless, how many changes were missed, how many the
// watch could not see, and how long people made changes wait, each with
// its coverage; and, for each draft gate pack, how many asks it would have
// sent to no one and how many of those did harm.
//
// A person's yes is never a label. The labels are what the watch saw: a
// sight's verdict, and an incident joined to the change.
package gate

import (
	"math"
	"slices"
	"sort"
	"strings"
	"time"

	"github.com/orchard-suite/platformkit/v3/annals"
	"github.com/orchard-suite/platformkit/v3/change"
)

// The change events a score reads.
const (
	verbStated   = "change.stated"
	verbRouted   = "change.routed"
	verbDecided  = "change.decided"
	verbEnded    = "change.ended"
	verbObserved = "change.observed"
)

// Verbs is every verb a score reads.
var Verbs = []string{verbStated, verbRouted, verbDecided, verbEnded, verbObserved}

// Unregistered is the family of an effect kind the estate's kinds do not
// name, so such an effect is counted where it can be seen, never dropped.
const Unregistered = "unregistered"

// draftPrefix starts a routed label that holds a draft pack's route.
const draftPrefix = "draft."

// MaxWeeks is the most weeks one read covers.
const MaxWeeks = 52

// Settings are the estate's part of a score.
type Settings struct {
	// Kinds names each effect kind's family: the suite's base kinds and the
	// estate's own.
	Kinds *change.Kinds
	// MaxBlind is the share of the changes that ran, in percent, that a
	// family's watch may fail to see and still be scored.
	MaxBlind int
}

// What a family's score says of itself.
const (
	ScoreScored     = "scored"
	ScoreTooBlind   = "too blind"
	ScoreNothingRan = "nothing ran"
)

// Family is one effect family's numbers for one week.
type Family struct {
	Week    time.Time
	Through time.Time
	Family  string
	Score   string
	// Changes the judge routed with an effect of the family; Asks of them
	// went to people, and Ran of them were applied.
	Changes, Asks, Ran int
	// Needless asks went to people, ran, and the watch saw them clean.
	// Misses ran and did harm: routed to no one, or joined to an incident.
	// Both are nil when the family is not scored.
	Needless, Misses *int
	// Unknowns ran and the watch could not see the family: blind, or never
	// watched.
	Unknowns int
	// Coverage is the percent of the changes that ran that the watch saw;
	// nil when none ran.
	Coverage *float64
	// WaitMedian is the median minutes from an ask to its last answer, and
	// Waited how many asks that is; WaitHours their sum.
	WaitMedian *float64
	WaitHours  float64
	Waited     int
}

// Draft is one draft pack's replay of one week, from the route each
// draft gave beside the live one.
type Draft struct {
	Week    time.Time
	Through time.Time
	Pack    string
	// Judged is the changes the draft routed, and Asks of them went to
	// people live.
	Judged, Asks int
	// ToNoOne is the asks the draft would have sent to no one. Harmed of
	// them did harm, and Unseen ran with no clear watch.
	ToNoOne, Harmed, Unseen int
	// Coverage is the percent of the asks it would have sent to no one,
	// and that ran, that the watch saw; nil when none ran.
	Coverage *float64
}

// outcome is what the watch saw, worst first.
type outcome int

const (
	clean outcome = iota
	blind
	harm
)

// changeOf is one change, joined from its events.
type changeOf struct {
	stated   time.Time
	effects  []annals.Effect
	route    string
	routed   time.Time
	drafts   map[string]string
	decided  time.Time
	ran      bool
	observed bool
	sights   []annals.Effect
	incident bool
}

// key names a change by the stated event's sender and key, as every later
// event names it in its cause.
type key struct{ source, key string }

// join reads every change in the events, keyed by its stated event.
func join(evs []annals.Event) map[key]*changeOf {
	out := map[key]*changeOf{}
	at := func(k key) *changeOf {
		c, ok := out[k]
		if !ok {
			c = &changeOf{}
			out[k] = c
		}
		return c
	}
	for _, ev := range evs {
		switch ev.Action.Verb {
		case verbStated:
			c := at(key{ev.Source, ev.EventKey})
			c.stated = ev.At
			c.effects = ev.Effects
		case verbRouted:
			c := at(key{ev.CauseFrom(), ev.Cause})
			c.route = ev.Labels["route"]
			c.routed = ev.At
			for k, v := range ev.Labels {
				if pack, ok := strings.CutPrefix(k, draftPrefix); ok && pack != "" {
					if c.drafts == nil {
						c.drafts = map[string]string{}
					}
					c.drafts[pack] = v
				}
			}
		case verbDecided:
			c := at(key{ev.CauseFrom(), ev.Cause})
			if ev.At.After(c.decided) {
				c.decided = ev.At
			}
		case verbEnded:
			c := at(key{ev.CauseFrom(), ev.Cause})
			if ev.Action.Outcome != "refused" {
				c.ran = true
			}
		case verbObserved:
			c := at(key{ev.CauseFrom(), ev.Cause})
			c.ran, c.observed = true, true
			c.sights = append(c.sights, ev.Effects...)
			for _, it := range ev.Items {
				if it.Kind == "incident" {
					c.incident = true
				}
			}
		}
	}
	return out
}

// families is the families a change's stated effects fall in.
func (s Settings) families(c *changeOf) []string {
	var out []string
	for _, ef := range c.effects {
		f := s.family(ef.Kind)
		if !slices.Contains(out, f) {
			out = append(out, f)
		}
	}
	return out
}

func (s Settings) family(kind string) string {
	if s.Kinds != nil {
		if k, ok := s.Kinds.Get(kind); ok {
			return k.Family
		}
	}
	return Unregistered
}

// seen is what the watch saw of a change's sights that pass keep: harm
// when it saw harm or an incident was joined, blind when it saw none of
// them or any of them blind, and clean only when it looked at each one.
func (c *changeOf) seen(keep func(annals.Effect) bool) outcome {
	if c.incident {
		return harm
	}
	worst, looked := clean, false
	for _, sg := range c.sights {
		if !keep(sg) {
			continue
		}
		looked = true
		switch sg.Verdict {
		case "harm":
			worst = harm
		case "blind":
			worst = max(worst, blind)
		case "clean":
		default:
			worst = max(worst, blind)
		}
	}
	if !looked {
		return max(worst, blind)
	}
	return worst
}

// WeekOf is the Monday, at midnight UTC, that starts t's week.
func WeekOf(t time.Time) time.Time {
	t = t.UTC()
	day := time.Date(t.Year(), t.Month(), t.Day(), 0, 0, 0, 0, time.UTC)
	back := (int(day.Weekday()) + 6) % 7
	return day.AddDate(0, 0, -back)
}

// Since is where a read of weeks weeks up to now starts: the Monday that
// starts the oldest of them.
func Since(now time.Time, weeks int) time.Time {
	return WeekOf(now).AddDate(0, 0, -7*(weeks-1))
}

// Score scores the changes stated from the Monday that starts since's
// week until now, one row a week and family, and one a week and draft
// pack. The families the estate names always have a row, so a quiet week
// reads as nothing, never as missing.
func Score(evs []annals.Event, since, now time.Time, s Settings) ([]Family, []Draft) {
	start := WeekOf(since)
	var weeks []time.Time
	for w := start; w.Before(now); w = w.AddDate(0, 0, 7) {
		weeks = append(weeks, w)
	}
	through := func(w time.Time) time.Time {
		end := w.AddDate(0, 0, 7)
		if end.After(now) {
			return now.UTC()
		}
		return end
	}

	type famKey struct {
		week   time.Time
		family string
	}
	type draftKey struct {
		week time.Time
		pack string
	}
	fams := map[famKey]*Family{}
	waits := map[famKey][]float64{}
	drafts := map[draftKey]*Draft{}
	draftSeen := map[draftKey]int{}
	draftRan := map[draftKey]int{}

	names := []string{change.FamilyDisruption, change.FamilyExposure}
	for _, w := range weeks {
		for _, f := range names {
			fams[famKey{w, f}] = &Family{Week: w, Through: through(w), Family: f}
		}
	}

	for _, c := range join(evs) {
		if c.stated.IsZero() || c.route == "" || c.stated.Before(start) || !c.stated.Before(now) {
			continue
		}
		w := WeekOf(c.stated)
		asked := c.route == change.RoutePeople
		for _, f := range s.families(c) {
			k := famKey{w, f}
			row := fams[k]
			if row == nil {
				row = &Family{Week: w, Through: through(w), Family: f}
				fams[k] = row
			}
			row.Changes++
			if asked {
				row.Asks++
				if !c.decided.IsZero() {
					from := c.routed
					if from.IsZero() {
						from = c.stated
					}
					waits[k] = append(waits[k], c.decided.Sub(from).Minutes())
				}
			}
			if !c.ran {
				continue
			}
			row.Ran++
			family := f
			switch c.seen(func(e annals.Effect) bool { return s.family(e.Kind) == family }) {
			case clean:
				if asked {
					row.Needless = inc(row.Needless)
				}
			case harm:
				if !asked || c.incident {
					row.Misses = inc(row.Misses)
				}
			case blind:
				row.Unknowns++
			}
		}
		for pack, route := range c.drafts {
			k := draftKey{w, pack}
			d := drafts[k]
			if d == nil {
				d = &Draft{Week: w, Through: through(w), Pack: pack}
				drafts[k] = d
			}
			d.Judged++
			if !asked {
				continue
			}
			d.Asks++
			if route != change.RouteNoOne {
				continue
			}
			d.ToNoOne++
			if !c.ran {
				continue
			}
			draftRan[k]++
			switch c.seen(func(annals.Effect) bool { return true }) {
			case harm:
				d.Harmed++
				draftSeen[k]++
			case clean:
				draftSeen[k]++
			case blind:
				d.Unseen++
			}
		}
	}

	var outF []Family
	for k, row := range fams {
		ws := waits[k]
		row.Waited = len(ws)
		if len(ws) > 0 {
			sort.Float64s(ws)
			m := median(ws)
			row.WaitMedian = &m
			total := 0.0
			for _, v := range ws {
				total += v
			}
			row.WaitHours = round(total/60, 1)
		}
		switch {
		case row.Ran == 0:
			row.Score = ScoreNothingRan
			row.Needless, row.Misses = nil, nil
		case row.Unknowns*100 > s.MaxBlind*row.Ran:
			row.Score = ScoreTooBlind
			row.Needless, row.Misses = nil, nil
		default:
			row.Score = ScoreScored
			row.Needless, row.Misses = orZero(row.Needless), orZero(row.Misses)
		}
		if row.Ran > 0 {
			c := round(100*float64(row.Ran-row.Unknowns)/float64(row.Ran), 1)
			row.Coverage = &c
		}
		outF = append(outF, *row)
	}
	slices.SortFunc(outF, func(a, b Family) int {
		if c := b.Week.Compare(a.Week); c != 0 {
			return c
		}
		return strings.Compare(a.Family, b.Family)
	})

	var outD []Draft
	for k, d := range drafts {
		if n := draftRan[k]; n > 0 {
			c := round(100*float64(draftSeen[k])/float64(n), 1)
			d.Coverage = &c
		}
		outD = append(outD, *d)
	}
	slices.SortFunc(outD, func(a, b Draft) int {
		if c := b.Week.Compare(a.Week); c != 0 {
			return c
		}
		return strings.Compare(a.Pack, b.Pack)
	})
	return outF, outD
}

func inc(p *int) *int {
	n := 1
	if p != nil {
		n = *p + 1
	}
	return &n
}

func orZero(p *int) *int {
	if p == nil {
		n := 0
		return &n
	}
	return p
}

// median of sorted values, to a tenth.
func median(sorted []float64) float64 {
	n := len(sorted)
	m := sorted[n/2]
	if n%2 == 0 {
		m = (sorted[n/2-1] + sorted[n/2]) / 2
	}
	return round(m, 1)
}

func round(v float64, places int) float64 {
	p := math.Pow(10, float64(places))
	return math.Round(v*p) / p
}
