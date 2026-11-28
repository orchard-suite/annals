package gate

import (
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/orchard-suite/platformkit/v3/annals"
	"github.com/orchard-suite/platformkit/v3/change"
)

// A Thursday, so its week starts on Monday 2026-11-23.
var now = time.Date(2026, 11, 26, 18, 0, 0, 0, time.UTC)

type ch struct {
	key       string
	at        time.Time
	kinds     []string
	route     string
	drafts    map[string]string
	decideIn  time.Duration
	ended     string
	sights    map[string]string // kind to verdict
	incidents int
}

func (c ch) events() []annals.Event {
	target := annals.Target{System: "orrery", Kind: "change", ID: c.key}
	var effects []annals.Effect
	for _, k := range c.kinds {
		effects = append(effects, annals.Effect{Kind: k, Subject: annals.Subject{Blueprint: "workload", Key: "acme-api"}, Path: "in-place"})
	}
	evs := []annals.Event{{
		At: c.at, Source: "orrery", EventKey: c.key, Effects: effects,
		Action: annals.Action{Verb: verbStated, Target: target, Outcome: "ok"},
	}}
	of := func(verb, suffix string, at time.Time, labels map[string]string) annals.Event {
		return annals.Event{At: at, Source: "orrery", EventKey: c.key + ":" + suffix, Cause: c.key,
			Action: annals.Action{Verb: verb, Target: target, Outcome: "ok"}, Labels: labels}
	}
	if c.route != "" {
		labels := map[string]string{"route": c.route}
		for p, r := range c.drafts {
			labels["draft."+p] = r
		}
		evs = append(evs, of(verbRouted, "routed", c.at.Add(time.Second), labels))
	}
	if c.decideIn > 0 {
		evs = append(evs, of(verbDecided, "decided", c.at.Add(time.Second+c.decideIn), map[string]string{"answer": "yes"}))
	}
	if c.ended != "" {
		e := of(verbEnded, "ended", c.at.Add(time.Hour), nil)
		e.Action.Outcome = c.ended
		evs = append(evs, e)
	}
	if c.sights != nil || c.incidents > 0 {
		o := annals.Event{At: c.at.Add(2 * time.Hour), Source: "dispatch", EventKey: "watch:" + c.key, Cause: c.key, CauseSource: "orrery",
			Action: annals.Action{Verb: verbObserved, Target: target, Outcome: "ok"}}
		for k, v := range c.sights {
			o.Effects = append(o.Effects, annals.Effect{Kind: k, Subject: annals.Subject{Blueprint: "workload", Key: "acme-api"}, Path: "in-place", Verdict: v})
		}
		for i := range c.incidents {
			o.Items = append(o.Items, annals.Item{System: "dispatch", Kind: "incident", ID: fmt.Sprint(i + 1)})
		}
		evs = append(evs, o)
	}
	return evs
}

func score(t *testing.T, s Settings, changes ...ch) ([]Family, []Draft) {
	t.Helper()
	var evs []annals.Event
	for _, c := range changes {
		evs = append(evs, c.events()...)
	}
	if s.Kinds == nil {
		s.Kinds = change.Base()
	}
	return Score(evs, Since(now, 2), now, s)
}

func row(t *testing.T, rows []Family, week time.Time, family string) Family {
	t.Helper()
	for _, r := range rows {
		if r.Week.Equal(week) && r.Family == family {
			return r
		}
	}
	t.Fatalf("no row for %s %s in %+v", week, family, rows)
	return Family{}
}

func val(p *int) string {
	if p == nil {
		return "nil"
	}
	return fmt.Sprint(*p)
}

var (
	thisWeek = time.Date(2026, 11, 23, 0, 0, 0, 0, time.UTC)
	lastWeek = time.Date(2026, 11, 16, 0, 0, 0, 0, time.UTC)
	tue      = time.Date(2026, 11, 24, 10, 0, 0, 0, time.UTC)
)

func TestWeekOf(t *testing.T) {
	for in, want := range map[string]string{
		"2026-11-23T00:00:00Z": "2026-11-23T00:00:00Z",
		"2026-11-29T23:59:59Z": "2026-11-23T00:00:00Z",
		"2026-11-30T00:00:00Z": "2026-11-30T00:00:00Z",
		"2026-11-22T12:00:00Z": "2026-11-16T00:00:00Z",
	} {
		at, _ := time.Parse(time.RFC3339, in)
		if got := WeekOf(at).Format(time.RFC3339); got != want {
			t.Errorf("WeekOf(%s) = %s, want %s", in, got, want)
		}
	}
	if got := Since(now, 2); !got.Equal(lastWeek) {
		t.Errorf("Since 2 weeks = %s", got)
	}
}

// Each outcome lands in its own number, and the approval is never one.
func TestOutcomesAreTheLabels(t *testing.T) {
	fams, _ := score(t, Settings{MaxBlind: 50},
		ch{key: "a", at: tue, kinds: []string{"stops"}, route: "people", decideIn: 10 * time.Minute, ended: "ok", sights: map[string]string{"stops": "clean"}},
		ch{key: "b", at: tue, kinds: []string{"stops"}, route: "people", decideIn: 30 * time.Minute, ended: "ok", sights: map[string]string{"stops": "harm"}},
		ch{key: "c", at: tue, kinds: []string{"stops"}, route: "no_one", ended: "ok", sights: map[string]string{"stops": "harm"}},
		ch{key: "d", at: tue, kinds: []string{"stops"}, route: "people", decideIn: 20 * time.Minute, ended: "ok", sights: map[string]string{"stops": "clean"}, incidents: 1},
		ch{key: "e", at: tue, kinds: []string{"stops"}, route: "no_one", ended: "ok", sights: map[string]string{"stops": "blind"}},
		ch{key: "f", at: tue, kinds: []string{"stops"}, route: "no_one", ended: "ok", sights: map[string]string{"stops": "clean"}},
	)
	r := row(t, fams, thisWeek, change.FamilyDisruption)
	if r.Score != ScoreScored || r.Changes != 6 || r.Asks != 3 || r.Ran != 6 {
		t.Fatalf("counts: %+v", r)
	}
	// a is needless; b did harm but people were asked, so it is no miss;
	// c went to no one and did harm; d was joined to an incident.
	if val(r.Needless) != "1" || val(r.Misses) != "2" || r.Unknowns != 1 {
		t.Errorf("needless %s, misses %s, unknowns %d", val(r.Needless), val(r.Misses), r.Unknowns)
	}
	if r.Coverage == nil || *r.Coverage != 83.3 {
		t.Errorf("coverage %v", r.Coverage)
	}
	if r.WaitMedian == nil || *r.WaitMedian != 20 || r.WaitHours != 1 || r.Waited != 3 {
		t.Errorf("wait: median %v, hours %v, waited %d", r.WaitMedian, r.WaitHours, r.Waited)
	}
}

// A family the watch could not see is not scored: blind is never clean.
func TestTooBlindIsNotScored(t *testing.T) {
	fams, _ := score(t, Settings{MaxBlind: 20},
		ch{key: "a", at: tue, kinds: []string{"grants"}, route: "people", decideIn: time.Minute, ended: "ok", sights: map[string]string{}},
		ch{key: "b", at: tue, kinds: []string{"grants", "stops"}, route: "people", decideIn: time.Minute, ended: "ok", sights: map[string]string{"stops": "clean"}},
	)
	exp := row(t, fams, thisWeek, change.FamilyExposure)
	if exp.Score != ScoreTooBlind || exp.Needless != nil || exp.Misses != nil || exp.Unknowns != 2 {
		t.Errorf("exposure: %+v", exp)
	}
	if exp.Coverage == nil || *exp.Coverage != 0 {
		t.Errorf("exposure coverage %v", exp.Coverage)
	}
	dis := row(t, fams, thisWeek, change.FamilyDisruption)
	if dis.Score != ScoreScored || val(dis.Needless) != "1" {
		t.Errorf("disruption: %+v needless %s", dis, val(dis.Needless))
	}
}

// A change that ran with no watch at all is an unknown, and one that never
// ran is in no outcome.
func TestUnwatchedAndUnran(t *testing.T) {
	fams, _ := score(t, Settings{MaxBlind: 100},
		ch{key: "a", at: tue, kinds: []string{"stops"}, route: "no_one", ended: "ok"},
		ch{key: "b", at: tue, kinds: []string{"stops"}, route: "people", ended: "refused"},
		ch{key: "c", at: tue, kinds: []string{"stops"}, route: "people"},
	)
	r := row(t, fams, thisWeek, change.FamilyDisruption)
	if r.Changes != 3 || r.Ran != 1 || r.Unknowns != 1 || val(r.Misses) != "0" || r.Waited != 0 || r.WaitMedian != nil {
		t.Errorf("%+v", r)
	}
}

// Every week has a row for each family, and a quiet one says nothing ran.
func TestQuietWeeks(t *testing.T) {
	fams, drafts := score(t, Settings{MaxBlind: 20})
	if len(fams) != 4 || len(drafts) != 0 {
		t.Fatalf("rows: %+v %+v", fams, drafts)
	}
	r := row(t, fams, lastWeek, change.FamilyExposure)
	if r.Score != ScoreNothingRan || r.Coverage != nil || r.Needless != nil {
		t.Errorf("%+v", r)
	}
	if !r.Through.Equal(thisWeek) || !row(t, fams, thisWeek, change.FamilyExposure).Through.Equal(now) {
		t.Errorf("through: last week %s", r.Through)
	}
	if fams[0].Week != thisWeek {
		t.Errorf("newest week first: %s", fams[0].Week)
	}
}

// An unrouted change, one stated before the range, and a kind nobody
// registered: the first two are left out, the last counted on its own.
func TestWhatCounts(t *testing.T) {
	old := time.Date(2026, 11, 1, 10, 0, 0, 0, time.UTC)
	fams, _ := score(t, Settings{MaxBlind: 20},
		ch{key: "a", at: tue, kinds: []string{"stops"}, ended: "ok"},
		ch{key: "b", at: old, kinds: []string{"stops"}, route: "people", ended: "ok"},
		ch{key: "c", at: tue, kinds: []string{"sends"}, route: "people", ended: "ok"},
	)
	if r := row(t, fams, thisWeek, change.FamilyDisruption); r.Changes != 0 {
		t.Errorf("disruption: %+v", r)
	}
	if r := row(t, fams, thisWeek, Unregistered); r.Changes != 1 {
		t.Errorf("unregistered: %+v", r)
	}
	kinds, err := change.LoadKinds(strings.NewReader(`{"kinds":[{"name":"sends","family":"exposure","means":"x"}]}`))
	if err != nil {
		t.Fatal(err)
	}
	fams, _ = score(t, Settings{Kinds: kinds, MaxBlind: 20}, ch{key: "c", at: tue, kinds: []string{"sends"}, route: "people", ended: "ok"})
	if r := row(t, fams, thisWeek, change.FamilyExposure); r.Changes != 1 {
		t.Errorf("an estate kind counts in its family: %+v", r)
	}
}

// A draft pack's replay counts the asks it would have sent to no one, and
// how many of those did harm.
func TestDrafts(t *testing.T) {
	next := map[string]string{"acme-next": "no_one"}
	_, drafts := score(t, Settings{MaxBlind: 20},
		ch{key: "a", at: tue, kinds: []string{"stops"}, route: "people", drafts: next, ended: "ok", sights: map[string]string{"stops": "clean"}},
		ch{key: "b", at: tue, kinds: []string{"stops"}, route: "people", drafts: next, ended: "ok", sights: map[string]string{"stops": "harm"}},
		ch{key: "c", at: tue, kinds: []string{"stops"}, route: "people", drafts: next, ended: "ok", sights: map[string]string{"stops": "blind"}},
		ch{key: "d", at: tue, kinds: []string{"stops"}, route: "people", drafts: map[string]string{"acme-next": "people"}, ended: "ok"},
		ch{key: "e", at: tue, kinds: []string{"stops"}, route: "no_one", drafts: next, ended: "ok"},
		ch{key: "f", at: tue, kinds: []string{"stops"}, route: "people", drafts: next},
	)
	if len(drafts) != 1 {
		t.Fatalf("%+v", drafts)
	}
	d := drafts[0]
	if d.Pack != "acme-next" || d.Judged != 6 || d.Asks != 5 || d.ToNoOne != 4 || d.Harmed != 1 || d.Unseen != 1 {
		t.Errorf("%+v", d)
	}
	if d.Coverage == nil || *d.Coverage != 66.7 {
		t.Errorf("coverage %v", d.Coverage)
	}
}

func TestTables(t *testing.T) {
	fams, drafts := score(t, Settings{MaxBlind: 20},
		ch{key: "a", at: tue, kinds: []string{"stops"}, route: "people", drafts: map[string]string{"acme-next": "no_one"}, ended: "ok", sights: map[string]string{"stops": "clean"}},
	)
	ft := FamilyTable(fams)
	if len(ft.Rows) != 4 || len(ft.Rows[0]) != len(ft.Columns) {
		t.Fatalf("%+v", ft)
	}
	dt := DraftTable(drafts)
	if len(dt.Rows) != 1 || len(dt.Rows[0]) != len(dt.Columns) || dt.Rows[0][2] != "acme-next" {
		t.Fatalf("%+v", dt)
	}
	if empty := DraftTable(nil); empty.Rows == nil {
		t.Error("an empty table has rows, not null")
	}
}
