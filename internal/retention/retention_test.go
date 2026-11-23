package retention

import (
	"context"
	"testing"
	"time"

	"github.com/orchard-suite/annals/internal/store"
	"github.com/orchard-suite/annals/internal/store/storetest"
	"github.com/orchard-suite/platformkit/v3/annals"
)

func TestParse(t *testing.T) {
	p, err := Parse("365d", "beacon=90d, ion=36h,audit=0")
	if err != nil {
		t.Fatal(err)
	}
	if p.Default != 365*24*time.Hour || p.Sources["beacon"] != 90*24*time.Hour || p.Sources["ion"] != 36*time.Hour || p.Sources["audit"] != 0 {
		t.Errorf("%+v", p)
	}
	for _, bad := range []string{"soon", "-3d", "3 days"} {
		if _, err := Parse(bad, ""); err == nil {
			t.Errorf("%q parsed", bad)
		}
	}
	if _, err := Parse("", "ion"); err == nil {
		t.Error("a source without a duration parsed")
	}
	got := p.Describe()
	if len(got) != 4 || got[0] != "default 365 days" || got[1] != "audit forever" {
		t.Errorf("%v", got)
	}
}

func TestApplyPrunesPerSource(t *testing.T) {
	st := storetest.Open(t)
	now := time.Now()
	put := func(source string, age time.Duration) {
		t.Helper()
		ev := annals.Event{At: now.Add(-age), Source: source, Actor: annals.Actor{Kind: "system", ID: "s"},
			Action: annals.Action{Verb: "x.y", Target: annals.Target{System: "s", Kind: "k", ID: "i"}, Outcome: "ok"}}
		if _, err := st.Put(context.Background(), []annals.Event{ev}); err != nil {
			t.Fatal(err)
		}
	}
	put("ion", 40*24*time.Hour)    // past ion's 30 days: goes
	put("ion", 10*24*time.Hour)    // stays
	put("audit", 900*24*time.Hour) // forever: stays
	put("ops", 400*24*time.Hour)   // past the default: goes
	put("ops", 100*24*time.Hour)   // stays
	p, _ := Parse("365d", "ion=30d,audit=0")
	rec := &recorder{}
	n, err := Apply(context.Background(), st, p, now, nil, rec)
	if err != nil || n != 2 {
		t.Fatalf("pruned %d (%v), want 2", n, err)
	}
	left, _ := st.Count(context.Background(), store.Filter{})
	if left != 3 {
		t.Errorf("%d left, want 3", left)
	}
	want := map[string]pruned{
		"ion": {1, now.Add(-30 * 24 * time.Hour)},
		"ops": {1, now.Add(-365 * 24 * time.Hour)},
	}
	if len(rec.got) != len(want) {
		t.Fatalf("recorded %+v, want %+v", rec.got, want)
	}
	for source, w := range want {
		if g := rec.got[source]; g.n != w.n || !g.cutoff.Equal(w.cutoff) {
			t.Errorf("%s: recorded %+v, want %+v", source, g, w)
		}
	}
	// A pass that deletes nothing records nothing.
	rec.got = nil
	if n, err := Apply(context.Background(), st, p, now, nil, rec); err != nil || n != 0 || len(rec.got) != 0 {
		t.Errorf("second pass: %d %v %+v", n, err, rec.got)
	}
}

type pruned struct {
	n      int64
	cutoff time.Time
}

type recorder struct{ got map[string]pruned }

func (r *recorder) Pruned(_ context.Context, source string, n int64, cutoff time.Time) {
	if r.got == nil {
		r.got = map[string]pruned{}
	}
	r.got[source] = pruned{n, cutoff}
}
