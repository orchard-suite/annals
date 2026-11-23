// Package contract holds Annals' published event contract to its Go
// form. The contract is published here in three forms: docs/event.md in
// words, schema/event.json as JSON Schema, and the platform kit's annals
// package in Go. These tests check the schema and the Go validator accept
// every example in schema/examples and refuse the same shapes for the same
// reasons.
package contract

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/santhosh-tekuri/jsonschema/v6"

	"github.com/orchard-suite/platformkit/v3/annals"
)

func loadSchema(t *testing.T) *jsonschema.Schema {
	t.Helper()
	c := jsonschema.NewCompiler()
	s, err := c.Compile("../../schema/event.json")
	if err != nil {
		t.Fatalf("compile schema: %v", err)
	}
	return s
}

func examples(t *testing.T) map[string][]byte {
	t.Helper()
	files, err := filepath.Glob("../../schema/examples/*.json")
	if err != nil || len(files) == 0 {
		t.Fatalf("no examples: %v", err)
	}
	out := map[string][]byte{}
	for _, f := range files {
		b, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		out[filepath.Base(f)] = b
	}
	return out
}

// Every example passes the JSON Schema and the Go validator alike.
func TestExamplesMatchSchemaAndValidator(t *testing.T) {
	schema := loadSchema(t)
	for name, b := range examples(t) {
		var doc any
		if err := json.Unmarshal(b, &doc); err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if err := schema.Validate(doc); err != nil {
			t.Errorf("%s: schema: %v", name, err)
		}
		var ev annals.Event
		if err := json.Unmarshal(b, &ev); err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if err := ev.Validate(ev.Source); err != nil {
			t.Errorf("%s: validator: %v", name, err)
		}
	}
}

// The schema and the validator refuse the same shapes, for the same reasons.
func TestRefusals(t *testing.T) {
	schema := loadSchema(t)
	base := examples(t)["beacon.json"]
	cases := []struct {
		name   string
		mutate func(m map[string]any)
		reason string
	}{
		{"unknown actor kind", func(m map[string]any) { m["actor"].(map[string]any)["kind"] = "robot" }, "bad_actor_kind"},
		{"no actor id", func(m map[string]any) { delete(m["actor"].(map[string]any), "id") }, "missing_actor_id"},
		{"bad outcome", func(m map[string]any) { m["action"].(map[string]any)["outcome"] = "maybe" }, "bad_outcome"},
		{"verb not a slug", func(m map[string]any) { m["action"].(map[string]any)["verb"] = "Did A Thing" }, "bad_slug"},
		{"bad runtime", func(m map[string]any) { m["agent"].(map[string]any)["runtime"] = "gpt" }, "bad_runtime"},
		{"label with newline", func(m map[string]any) { m["labels"] = map[string]any{"ok": "a\nb"} }, "bad_label"},
		{"too many labels", func(m map[string]any) {
			l := map[string]any{}
			for i := 0; i < 17; i++ {
				l["k"+strings.Repeat("x", i)] = "v"
			}
			m["labels"] = l
		}, "too_many_labels"},
		{"title too long", func(m map[string]any) {
			m["action"].(map[string]any)["target"].(map[string]any)["title"] = strings.Repeat("t", 513)
		}, "too_long"},
		{"email in a title", func(m map[string]any) {
			m["action"].(map[string]any)["target"].(map[string]any)["title"] = "for someone@example.com"
		}, "email_in_text"},
		{"email in a label", func(m map[string]any) { m["labels"] = map[string]any{"by": "someone@example.com"} }, "email_in_text"},
		{"actor email not an email", func(m map[string]any) { m["actor"].(map[string]any)["email"] = "someone" }, "bad_email"},
		{"party kind unknown", func(m map[string]any) {
			m["parties"] = []any{map[string]any{"role": "recipient", "kind": "robot", "id": "x"}}
		}, "bad_party_kind"},
		{"person party with no id", func(m map[string]any) {
			m["parties"] = []any{map[string]any{"role": "recipient", "kind": "person"}}
		}, "missing_parties_id"},
		{"item with no id", func(m map[string]any) {
			m["items"] = []any{map[string]any{"system": "sharepoint", "kind": "file"}}
		}, "missing_items_id"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var m map[string]any
			if err := json.Unmarshal(base, &m); err != nil {
				t.Fatal(err)
			}
			tc.mutate(m)
			if err := schema.Validate(m); err == nil {
				t.Errorf("schema accepted it")
			}
			b, _ := json.Marshal(m)
			var ev annals.Event
			if err := json.Unmarshal(b, &ev); err != nil {
				t.Fatal(err)
			}
			err := ev.Validate("beacon")
			if annals.Reason(err) != tc.reason {
				t.Errorf("validator reason %q (%v), want %q", annals.Reason(err), err, tc.reason)
			}
		})
	}
}
