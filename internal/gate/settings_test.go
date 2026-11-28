package gate

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/orchard-suite/platformkit/v3/change"
)

func envOf(m map[string]string) func(string) string {
	return func(k string) string { return m[k] }
}

// The estate's gate pack names its own kinds, so a change of one counts in
// its family, never as unregistered.
func TestSettingsReadThePack(t *testing.T) {
	s, err := ReadSettings(envOf(map[string]string{"GATE_PACK": "testdata/acme-gate.yaml", "GATE_MAX_BLIND": "10"}))
	if err != nil {
		t.Fatal(err)
	}
	if s.MaxBlind != 10 {
		t.Errorf("max blind: %d", s.MaxBlind)
	}
	fams, _ := score(t, s, ch{key: "a", at: tue, kinds: []string{"signs_in"}, route: "people", ended: "ok"})
	if r := row(t, fams, thisWeek, change.FamilyExposure); r.Changes != 1 {
		t.Errorf("the pack's kind counts in its family: %+v", r)
	}
	for _, r := range fams {
		if r.Family == Unregistered {
			t.Errorf("nothing is unregistered: %+v", r)
		}
	}
}

func TestSettingsWithoutAPack(t *testing.T) {
	s, err := ReadSettings(envOf(nil))
	if err != nil {
		t.Fatal(err)
	}
	if s.MaxBlind != 20 || len(s.Kinds.All()) != len(change.Base().All()) {
		t.Errorf("the defaults are the base kinds and 20: %+v", s)
	}
}

func TestSettingsRefuse(t *testing.T) {
	bad := filepath.Join(t.TempDir(), "bad.yaml")
	if err := os.WriteFile(bad, []byte("kinds:\n  - {name: sends, family: inert, means: x}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	for name, env := range map[string]map[string]string{
		"missing pack":  {"GATE_PACK": "testdata/none.yaml"},
		"bad kind":      {"GATE_PACK": bad},
		"bad max blind": {"GATE_MAX_BLIND": "101"},
	} {
		if _, err := ReadSettings(envOf(env)); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
}
