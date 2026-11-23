package annalsmcp

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/orchard-suite/annals/internal/people"
	"github.com/orchard-suite/annals/internal/store"
	"github.com/orchard-suite/annals/internal/store/storetest"
	"github.com/orchard-suite/platformkit/v3/annals"
)

func seeded(t *testing.T) *store.Store {
	t.Helper()
	st := storetest.Open(t)
	files, _ := filepath.Glob("../../schema/examples/*.json")
	for _, f := range files {
		b, _ := os.ReadFile(f)
		var ev annals.Event
		if err := json.Unmarshal(b, &ev); err != nil {
			t.Fatal(err)
		}
		if err := ev.Validate(ev.Source); err != nil {
			t.Fatal(err)
		}
		if _, err := st.Put(context.Background(), []annals.Event{ev}); err != nil {
			t.Fatal(err)
		}
	}
	return st
}

// serve puts the same bearer gate the API port uses in front of /mcp and
// connects a client that reads at level.
func serve(t *testing.T, st *store.Store, level people.Level) *mcp.ClientSession {
	t.Helper()
	gate := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer an_test" {
			http.Error(w, "no", http.StatusUnauthorized)
			return
		}
		Handler(st, "test").ServeHTTP(w, r.WithContext(WithLevel(r.Context(), level)))
	})
	srv := httptest.NewServer(gate)
	t.Cleanup(srv.Close)
	ctx := context.Background()
	client := mcp.NewClient(&mcp.Implementation{Name: "test", Version: "0"}, nil)
	if _, err := client.Connect(ctx, &mcp.StreamableClientTransport{Endpoint: srv.URL}, nil); err == nil {
		t.Fatal("connected without a token")
	}
	hc := &http.Client{Transport: bearer{"an_test"}}
	sess, err := client.Connect(ctx, &mcp.StreamableClientTransport{Endpoint: srv.URL, HTTPClient: hc}, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { sess.Close() })
	return sess
}

func TestToolsOverStreamableHTTP(t *testing.T) {
	st := seeded(t)
	sess := serve(t, st, people.PII)
	ctx := context.Background()

	tools, err := sess.ListTools(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	names := map[string]bool{}
	for _, tl := range tools.Tools {
		names[tl.Name] = true
	}
	for _, n := range []string{"annals_search", "annals_actor_summary", "annals_target_history"} {
		if !names[n] {
			t.Errorf("missing tool %s", n)
		}
	}

	call := func(name string, args map[string]any) (string, map[string]any) {
		t.Helper()
		res, err := sess.CallTool(ctx, &mcp.CallToolParams{Name: name, Arguments: args})
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if res.IsError {
			t.Fatalf("%s: tool error %v", name, res.Content)
		}
		text := res.Content[0].(*mcp.TextContent).Text
		var structured map[string]any
		b, _ := json.Marshal(res.StructuredContent)
		json.Unmarshal(b, &structured)
		return text, structured
	}
	text, out := call("annals_search", map[string]any{"outcome": "refused"})
	if out["total"].(float64) != 1 || !strings.Contains(text, "tool.bash") {
		t.Errorf("search: %v\n%s", out["total"], text)
	}
	text, out = call("annals_search", map[string]any{"source": "beacon", "since": "2026-10-01T00:00:00Z"})
	if out["total"].(float64) != 1 || !strings.Contains(text, "Task 42") {
		t.Errorf("search by source: %s", text)
	}
	_, out = call("annals_search", map[string]any{"limit": 2})
	if len(out["events"].([]any)) != 2 || out["next"] == "" {
		t.Errorf("paging: %v", out)
	}
	text, out = call("annals_actor_summary", map[string]any{"actor": "beacon-agent", "since": "2026-10-01T00:00:00Z"})
	if out["total"].(float64) != 1 || !strings.Contains(text, "app.record.update") {
		t.Errorf("actor summary: %s", text)
	}
	text, out = call("annals_target_history", map[string]any{"system": "github", "kind": "repository", "id": "example-org/example-repo"})
	if out["total"].(float64) != 1 || !strings.Contains(text, "approved by user-0001") {
		t.Errorf("target history: %s", text)
	}
	res, err := sess.CallTool(ctx, &mcp.CallToolParams{Name: "annals_search", Arguments: map[string]any{"outcome": "maybe"}})
	if err != nil || !res.IsError {
		t.Errorf("a bad outcome is a tool error: %v %v", err, res)
	}
	text, out = call("annals_search", map[string]any{"party": "auditor@example.org"})
	if out["total"].(float64) != 1 || !strings.Contains(text, "recipient auditor@example.org") {
		t.Errorf("search by party: %s", text)
	}
	text, out = call("annals_search", map[string]any{"source": "m365", "cause": "copilot:thread-91ac:msg-01"})
	if out["total"].(float64) != 1 || !strings.Contains(text, "comet.search_projects") || !strings.Contains(text, "touched Spring launch") {
		t.Errorf("what a prompt led to: %s", text)
	}
}

// A reader token sees what happened, but no one's name or email, and cannot
// find a person by their email.
func TestReaderTokenSeesNoPeople(t *testing.T) {
	st := seeded(t)
	sess := serve(t, st, people.Reader)
	ctx := context.Background()
	res, err := sess.CallTool(ctx, &mcp.CallToolParams{Name: "annals_search", Arguments: map[string]any{"source": "m365"}})
	if err != nil || res.IsError {
		t.Fatalf("search: %v %v", err, res)
	}
	text := res.Content[0].(*mcp.TextContent).Text
	b, _ := json.Marshal(res.StructuredContent)
	for _, leak := range []string{"auditor@example.org", "owner@example.com", "writer@example.com", "Outside Auditor", "Site Owner", "user-0002"} {
		if strings.Contains(text, leak) || strings.Contains(string(b), leak) {
			t.Errorf("a reader saw %q", leak)
		}
	}
	if !strings.Contains(text, "Finance Close") || !strings.Contains(text, "comet.search_projects") {
		t.Errorf("a reader still sees what happened: %s", text)
	}
	res, _ = sess.CallTool(ctx, &mcp.CallToolParams{Name: "annals_search", Arguments: map[string]any{"party": "auditor@example.org"}})
	var out map[string]any
	b, _ = json.Marshal(res.StructuredContent)
	json.Unmarshal(b, &out)
	if out["total"].(float64) != 0 {
		t.Errorf("a reader found a person by email: %v", out["total"])
	}
	ref := st.Masker().Ref("guest-0042")
	res, _ = sess.CallTool(ctx, &mcp.CallToolParams{Name: "annals_search", Arguments: map[string]any{"party": ref}})
	b, _ = json.Marshal(res.StructuredContent)
	json.Unmarshal(b, &out)
	if out["total"].(float64) != 1 {
		t.Errorf("a reader finds a person by ref: %v", out["total"])
	}
}

type bearer struct{ token string }

func (b bearer) RoundTrip(r *http.Request) (*http.Response, error) {
	r = r.Clone(r.Context())
	r.Header.Set("Authorization", "Bearer "+b.token)
	return http.DefaultTransport.RoundTrip(r)
}
