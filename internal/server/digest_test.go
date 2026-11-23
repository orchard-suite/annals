package server

import (
	"strings"
	"testing"
	"time"

	"github.com/orchard-suite/platformkit/v3/annals"

	"github.com/orchard-suite/annals/internal/store"
)

func TestWhyAPatternIsWorthALook(t *testing.T) {
	sys := annals.Event{Actor: annals.Actor{Kind: "system", ID: "beacon-agent"}}
	person := annals.Event{Actor: annals.Actor{Kind: "person", ID: "p"}}
	for _, c := range []struct {
		name   string
		p      store.Pattern
		usual  int
		before bool
		want   reason
	}{
		{"a failure, however routine", store.Pattern{Latest: sys, Count: 500, Failed: 1}, 500, true, whyFailed},
		{"a refusal", store.Pattern{Latest: sys, Count: 1, Refused: 1}, 0, true, whyRefused},
		{"three times its usual", store.Pattern{Latest: sys, Count: 60}, 20, true, whySpike},
		{"busier, but not three times", store.Pattern{Latest: sys, Count: 50}, 20, true, whyNone},
		{"three times a tiny usual is no spike", store.Pattern{Latest: sys, Count: 9}, 3, true, whyNone},
		{"new since the window before", store.Pattern{Latest: sys, Count: 40}, 0, true, whyNew},
		{"nothing before to compare with", store.Pattern{Latest: sys, Count: 40}, 0, false, whyNone},
		{"a person", store.Pattern{Latest: person, Count: 40}, 40, true, whyPerson},
		{"rare", store.Pattern{Latest: sys, Count: 4}, 4, true, whyRare},
		{"routine", store.Pattern{Latest: sys, Count: 588}, 600, true, whyNone},
	} {
		if got := why(c.p, c.usual, c.before); got != c.want {
			t.Errorf("%s: %v, want %v", c.name, got.Word(), c.want.Word())
		}
	}
}

func TestAllTimeIsAboutThirtySteps(t *testing.T) {
	now := time.Date(2026, 11, 8, 2, 0, 0, 0, time.UTC)
	for _, c := range []struct {
		oldest   time.Time
		maxSteps int
		step     time.Duration
	}{
		{now.Add(-5 * time.Hour), 45, time.Hour},
		{now.Add(-38 * 24 * time.Hour), 45, 24 * time.Hour},
		{now.Add(-400 * 24 * time.Hour), 45, 9 * 24 * time.Hour},
	} {
		w := stretch(allTime, c.oldest, now)
		if w.Step != c.step || int(w.Span/w.Step) > c.maxSteps || w.Span < now.Sub(c.oldest) {
			t.Errorf("from %s: step %s, span %s", c.oldest, w.Step, w.Span)
		}
	}
}

func TestVerbsAndTargetsReadAsWords(t *testing.T) {
	if got := plainVerb("vault.reveal_secret"); got != "reveal secret" {
		t.Error(got)
	}
	for verb, want := range map[string]string{
		"vault.reveal_secret|secret":     "revealed secret",
		"rule.changed|blueprint":         "changed rule on",
		"blueprint.changed|blueprint":    "changed",
		"icon.changed|model":             "changed icon on",
		"entity.deleted|container_image": "deleted",
		"notice.accept|notice":           "accepted",
		"notice.drop|notice":             "dropped",
		"vault.triage|message":           "triaged",
		"vault.sync_app|argo":            "synced app",
		"incident.acknowledge|incident":  "acknowledged",
		"memory.remember|memory":         "remembered",
		"job.run|cronjob":                "ran",
		"tool.bash|command":              "ran bash",
	} {
		v, kind, _ := strings.Cut(verb, "|")
		if got := verbWords(annals.Action{Verb: v, Target: annals.Target{Kind: kind}}); got != want {
			t.Errorf("%s: %q, want %q", v, got, want)
		}
	}
	if got := verbFor(annals.Action{Verb: "vault.reveal_secret", Target: annals.Target{Kind: "secret"}}, 3) + " " + targetsSaid(annals.Action{Target: annals.Target{Kind: "secret"}}, 3); got != "revealed 3 secrets" {
		t.Error(got)
	}
	if got := targetSaid(annals.Action{Verb: "icon.changed", Target: annals.Target{Kind: "model", ID: "icon.changed"}}); got != "model" {
		t.Error(got)
	}
	if got := targetName(annals.Target{Kind: "container_image", ID: "x"}); got != "container image x" {
		t.Error(got)
	}
	if got := plural("entity"); got != "entities" {
		t.Error(got)
	}
	if got := thousands(1234567); got != "1,234,567" {
		t.Error(got)
	}
	if got := string(spark([]int{0, 2, 4}, 0)); strings.Count(got, "<rect") != 2 {
		t.Error(got)
	}
}

func TestDaysFollowTheBoardsZone(t *testing.T) {
	chicago, err := time.LoadLocation("America/Chicago")
	if err != nil {
		t.Fatal(err)
	}
	was := zone
	zone = chicago
	t.Cleanup(func() { zone = was })
	// 03:00 UTC on the 8th is 21:00 on the 7th in Chicago.
	late := annals.Event{At: time.Date(2025, 11, 8, 3, 0, 0, 0, time.UTC)}
	early := annals.Event{At: time.Date(2025, 11, 7, 20, 0, 0, 0, time.UTC)}
	got := days([]annals.Event{late, early})
	if len(got) != 1 || !strings.HasPrefix(got[0].Label, "Friday, November 7") {
		t.Errorf("days: %+v", got)
	}
	if got := hm(late.At); got != "21:00" {
		t.Errorf("clock: %s", got)
	}
}

func TestRunsFoldOnePatternPerDay(t *testing.T) {
	base := time.Date(2026, 11, 7, 14, 0, 0, 0, time.UTC)
	ev := func(min int, actor, verb, target, outcome string) annals.Event {
		return annals.Event{At: base.Add(time.Duration(min) * time.Minute), Actor: annals.Actor{ID: actor},
			Action: annals.Action{Verb: verb, Outcome: outcome, Target: annals.Target{System: "s", Kind: "secret", ID: target}}}
	}
	// Newest first: two patterns interleave on one day, and the failure stands apart.
	evs := []annals.Event{
		ev(50, "beacon-agent", "reveal", "a", "ok"), ev(40, "beacon-agent", "triage", "m", "ok"), ev(30, "beacon-agent", "reveal", "b", "ok"),
		ev(20, "beacon-agent", "triage", "n", "ok"), ev(10, "beacon-agent", "reveal", "a", "failed"),
		ev(-10, "beacon-agent", "reveal", "a", "ok"), ev(-24*60, "beacon-agent", "reveal", "a", "ok"),
	}
	runs := runsOf(evs, true)
	if len(runs) != 4 || runs[0].Count() != 3 || runs[0].Targets != 2 || runs[0].Spread() != 2 || runs[1].Count() != 2 || runs[2].Count() != 1 || runs[3].Count() != 1 {
		t.Errorf("runs: %+v", runs)
	}
	if n := len(runsOf(evs, false)); n != 7 {
		t.Errorf("unfolded: %d runs", n)
	}
}

func TestRareLinesByManyActorsFoldIntoOne(t *testing.T) {
	rare := func(actor string, at int) fold {
		return fold{Why: whyRare, Count: 2, Targets: 1, Actors: 1, Steps: []int{1, 1},
			Latest: annals.Event{Source: "orrery", At: time.Unix(int64(at), 0), Actor: annals.Actor{ID: actor},
				Action: annals.Action{Verb: "icon.changed", Target: annals.Target{System: "orrery", Kind: "model"}}}}
	}
	other := rare("pat", 9)
	other.Latest.Action.Verb = "model.imported"
	look := []fold{rare("a", 3), rare("b", 5), other, rare("c", 4)}
	got := foldRare(look, nil, "24h")
	if len(got) != 2 || got[0].Actors != 3 || got[0].Count != 6 || got[0].Latest.Actor.ID != "b" || got[0].Steps[0] != 3 {
		t.Errorf("folded: %+v", got)
	}
	if !strings.Contains(got[0].Records, "verb=icon.changed") || strings.Contains(got[0].Records, "actor=") {
		t.Errorf("records link: %s", got[0].Records)
	}
	if two := foldRare(look[:2], nil, "24h"); len(two) != 2 {
		t.Errorf("two actors stay apart: %d lines", len(two))
	}
}

func TestStreamsFoldOneActionByManyActors(t *testing.T) {
	at := time.Date(2026, 11, 7, 15, 0, 0, 0, time.UTC)
	ev := func(actor string, min int) annals.Event {
		return annals.Event{At: at.Add(time.Duration(min) * time.Minute), Source: "orrery", Actor: annals.Actor{ID: actor},
			Action: annals.Action{Verb: "icon.changed", Outcome: "ok", Target: annals.Target{System: "orrery", Kind: "model", ID: actor}}}
	}
	runs := foldActors(runsOf([]annals.Event{ev("a", 9), ev("b", 8), ev("c", 7), ev("a", 6)}, true))
	if len(runs) != 1 || runs[0].Actors != 3 || runs[0].Count() != 4 || !runs[0].First().At.Equal(at.Add(9*time.Minute)) {
		t.Errorf("runs: %+v", runs)
	}
	if two := foldActors(runsOf([]annals.Event{ev("a", 9), ev("b", 8)}, true)); len(two) != 2 {
		t.Errorf("two actors stay apart: %d runs", len(two))
	}
}

func TestAnEstatesAreaWordsLeaveTheSentence(t *testing.T) {
	a := annals.Action{Verb: "lab.thing_changed", Target: annals.Target{Kind: "secret"}}
	if got := verbWords(a); got != "thing changed lab on" {
		t.Fatalf("before the area is known: %q", got)
	}
	addAreaWords([]string{" lab ", ""})
	defer delete(genericHeads, "lab")
	if got := verbWords(a); got != "thing changed" {
		t.Errorf("with the area known: %q", got)
	}
}
