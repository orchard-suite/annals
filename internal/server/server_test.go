package server

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/orchard-suite/platformkit/v3/annals"

	"github.com/orchard-suite/annals/internal/metrics"
	"github.com/orchard-suite/annals/internal/retention"
	"github.com/orchard-suite/annals/internal/selfaudit"
	"github.com/orchard-suite/annals/internal/store"
	"github.com/orchard-suite/annals/internal/store/storetest"
)

type rig struct {
	t       *testing.T
	srv     *Server
	store   *store.Store
	audit   *selfaudit.Auditor
	api     *httptest.Server
	session *httptest.Server
}

func newRig(t *testing.T) *rig {
	t.Helper()
	return newRigWith(t, Config{})
}

// newRigWith starts both ports with cfg over the test's defaults.
func newRigWith(t *testing.T, cfg Config) *rig {
	t.Helper()
	st := storetest.Open(t)
	m, err := metrics.New(func() metrics.Gauges { return metrics.Gauges{} })
	if err != nil {
		t.Fatal(err)
	}
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	cfg.PublicURL, cfg.Retention, cfg.Version = "https://annals.example.com", retention.Policy{}, "test"
	audit := selfaudit.New(st, m, log)
	srv, err := New(cfg, st, m, audit, log)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(audit.Wait)
	r := &rig{t: t, srv: srv, store: st, audit: audit, api: httptest.NewServer(srv.API()), session: httptest.NewServer(srv.Session())}
	t.Cleanup(r.api.Close)
	t.Cleanup(r.session.Close)
	return r
}

func (r *rig) token(source string) string {
	r.t.Helper()
	return r.tokenReads(source, "")
}

func (r *rig) tokenReads(source, reads string) string {
	r.t.Helper()
	secret, _, err := r.store.Mint(context.Background(), source+" test", source, reads, "admin@example.com")
	if err != nil {
		r.t.Fatal(err)
	}
	return secret
}

// ingest stores events the way the API does and returns their ids.
func (r *rig) ingest(names ...string) []string {
	r.t.Helper()
	var ids []string
	for _, name := range names {
		ev := example(r.t, name)
		rc := r.srv.Ingest(context.Background(), []annals.Event{ev}, ev.Source)[0]
		if rc.Status != "stored" {
			r.t.Fatalf("%s: %+v", name, rc)
		}
		ids = append(ids, rc.ID)
	}
	return ids
}

func (r *rig) do(method, path, token string, body any) (*http.Response, []byte) {
	r.t.Helper()
	var buf io.Reader
	if body != nil {
		switch b := body.(type) {
		case string:
			buf = strings.NewReader(b)
		case []byte:
			buf = bytes.NewReader(b)
		default:
			j, _ := json.Marshal(b)
			buf = bytes.NewReader(j)
		}
	}
	req, _ := http.NewRequest(method, r.api.URL+path, buf)
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		r.t.Fatal(err)
	}
	defer resp.Body.Close()
	out, _ := io.ReadAll(resp.Body)
	return resp, out
}

func example(t *testing.T, name string) annals.Event {
	t.Helper()
	b, err := os.ReadFile("../../schema/examples/" + name + ".json")
	if err != nil {
		t.Fatal(err)
	}
	var ev annals.Event
	if err := json.Unmarshal(b, &ev); err != nil {
		t.Fatal(err)
	}
	return ev
}

func TestAPITokenBothBranches(t *testing.T) {
	r := newRig(t)
	ev := example(t, "ion")
	if resp, _ := r.do("POST", "/v1/events", "", ev); resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("no token: %d", resp.StatusCode)
	}
	if resp, _ := r.do("POST", "/v1/events", "an_notatoken", ev); resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("bad token: %d", resp.StatusCode)
	}
	if resp, _ := r.do("GET", "/v1/events", "", nil); resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("list without token: %d", resp.StatusCode)
	}
	if resp, _ := r.do("GET", "/healthz", "", nil); resp.StatusCode != http.StatusOK {
		t.Errorf("healthz must be open: %d", resp.StatusCode)
	}
	tok := r.token("ion")
	resp, body := r.do("POST", "/v1/events", tok, ev)
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("good token: %d %s", resp.StatusCode, body)
	}
	var rc annals.Receipt
	json.Unmarshal(body, &rc)
	if rc.Status != "stored" || rc.ID == "" || resp.Header.Get("Location") != "/v1/events/"+rc.ID {
		t.Errorf("receipt %+v", rc)
	}
	// The front door's headers mean nothing on the API port.
	req, _ := http.NewRequest("GET", r.api.URL+"/v1/events", nil)
	req.Header.Set("X-Forwarded-Email", "someone@example.com")
	req.Header.Set("X-Forwarded-Groups", "Admin")
	resp2, _ := http.DefaultClient.Do(req)
	if resp2.StatusCode != http.StatusUnauthorized {
		t.Errorf("headers admitted on the API port: %d", resp2.StatusCode)
	}
	// A revoked token stops at once.
	list, _ := r.store.Tokens(context.Background())
	_, _ = r.store.Revoke(context.Background(), list[0].ID)
	if resp, _ := r.do("GET", "/v1/events", tok, nil); resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("revoked token: %d", resp.StatusCode)
	}
}

func TestAPISourceIsTheTokens(t *testing.T) {
	r := newRig(t)
	tok := r.token("ion")
	ev := example(t, "beacon")
	resp, body := r.do("POST", "/v1/events", tok, ev)
	if resp.StatusCode != http.StatusUnprocessableEntity || !strings.Contains(string(body), "source_mismatch") {
		t.Errorf("another source: %d %s", resp.StatusCode, body)
	}
	ev.Source = ""
	resp, body = r.do("POST", "/v1/events", tok, ev)
	if resp.StatusCode != http.StatusCreated {
		t.Errorf("empty source takes the token's: %d %s", resp.StatusCode, body)
	}
	got, _ := r.store.Query(context.Background(), store.Filter{})
	if got.Events[0].Source != "ion" {
		t.Errorf("stored source %q", got.Events[0].Source)
	}
}

func TestAPIIdempotencyAndBatch(t *testing.T) {
	r := newRig(t)
	tok := r.token("graft")
	ev := example(t, "graft")
	resp, body := r.do("POST", "/v1/events", tok, ev)
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("%d %s", resp.StatusCode, body)
	}
	var first annals.Receipt
	json.Unmarshal(body, &first)
	resp, body = r.do("POST", "/v1/events", tok, ev)
	var again annals.Receipt
	json.Unmarshal(body, &again)
	if resp.StatusCode != http.StatusOK || !again.Duplicate || again.ID != first.ID {
		t.Errorf("repeat: %d %+v", resp.StatusCode, again)
	}
	bad := ev
	bad.EventKey = "job:01JBX7:bad"
	bad.Action.Outcome = "maybe"
	fresh := ev
	fresh.EventKey = "job:01JBX7:ready"
	resp, body = r.do("POST", "/v1/events", tok, []annals.Event{ev, bad, fresh})
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("batch: %d %s", resp.StatusCode, body)
	}
	var out struct{ Results []annals.Receipt }
	json.Unmarshal(body, &out)
	if len(out.Results) != 3 || out.Results[0].Status != "duplicate" || out.Results[1].Status != "refused" || out.Results[1].Reason != "bad_outcome" || out.Results[2].Status != "stored" {
		t.Errorf("receipts: %+v", out.Results)
	}
	n, _ := r.store.Count(context.Background(), store.Filter{})
	if n != 2 {
		t.Errorf("%d stored, want 2", n)
	}
	if resp, body := r.do("POST", "/v1/events", tok, `{"at":"2026-01-01T00:00:00Z","surprise":1}`); resp.StatusCode != http.StatusBadRequest || !strings.Contains(string(body), "bad_json") {
		t.Errorf("unknown field: %d %s", resp.StatusCode, body)
	}
}

func TestAPIListFiltersAndStats(t *testing.T) {
	r := newRig(t)
	r.ingest("beacon", "graft", "ion", "curl")
	tok := r.token("reader")
	count := func(q string) int {
		t.Helper()
		resp, body := r.do("GET", "/v1/events?"+q, tok, nil)
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("%s: %d %s", q, resp.StatusCode, body)
		}
		var p store.Page
		json.Unmarshal(body, &p)
		return len(p.Events)
	}
	for q, want := range map[string]int{
		"": 4, "source=ion": 1, "actor=beacon-agent": 1, "system=github": 1, "outcome=refused": 1, "verb=workload.restart": 1,
		"since=2026-10-31T14:10:00Z": 3, "until=2026-10-31T14:10:00Z": 1, "approval=required": 1, "outcome=ok&system=beacon": 1,
	} {
		if n := count(q); n != want {
			t.Errorf("%q: %d, want %d", q, n, want)
		}
	}
	if resp, _ := r.do("GET", "/v1/events?since=yesterday", tok, nil); resp.StatusCode != http.StatusBadRequest {
		t.Errorf("bad since: %d", resp.StatusCode)
	}
	resp, body := r.do("GET", "/v1/stats", tok, nil)
	var st store.Stats
	json.Unmarshal(body, &st)
	if resp.StatusCode != http.StatusOK || st.Events != 4 || len(st.Sources) != 4 || st.Tokens != 1 {
		t.Errorf("stats: %d %+v", resp.StatusCode, st)
	}
}

func (r *rig) page(path string, headers map[string]string) (*http.Response, string) {
	r.t.Helper()
	req, _ := http.NewRequest("GET", r.session.URL+path, nil)
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		r.t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return resp, string(b)
}

var reader = map[string]string{"X-Forwarded-Email": "reader@example.com", "X-Forwarded-Groups": "Reader"}
var admin = map[string]string{"X-Forwarded-Email": "admin@example.com", "X-Forwarded-Groups": "Admin,other"}

func TestSessionPortTrustsOnlyTheDoor(t *testing.T) {
	r := newRig(t)
	if resp, _ := r.page("/", nil); resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("no identity: %d", resp.StatusCode)
	}
	if resp, _ := r.page("/", map[string]string{"X-Forwarded-Email": "x@example.com", "X-Forwarded-Groups": "nothing"}); resp.StatusCode != http.StatusForbidden {
		t.Errorf("no role: %d", resp.StatusCode)
	}
	if resp, _ := r.page("/tokens", reader); resp.StatusCode != http.StatusForbidden {
		t.Errorf("reader on tokens: %d", resp.StatusCode)
	}
	req, _ := http.NewRequest("GET", r.session.URL+"/", nil)
	req.Header.Set("Authorization", "Bearer "+r.token("ion"))
	resp, _ := http.DefaultClient.Do(req)
	if resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("bearer token on the session port: %d", resp.StatusCode)
	}
	if resp, body := r.page("/", reader); resp.StatusCode != http.StatusOK || !strings.Contains(body, "worth a look") {
		t.Errorf("reader: %d", resp.StatusCode)
	}
	if resp, _ := r.page("/tokens", admin); resp.StatusCode != http.StatusOK {
		t.Errorf("admin on tokens: %d", resp.StatusCode)
	}
	if resp, _ := r.page("/", admin); resp.StatusCode != http.StatusOK {
		t.Errorf("admin implies reader: %d", resp.StatusCode)
	}
}

// The door sends bare roles, so a role with the app's prefix would refuse
// everyone; the server will not start with one.
func TestPrefixedRoleRefusedAtStart(t *testing.T) {
	st := storetest.Open(t)
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	if _, err := New(Config{PublicURL: "https://annals.example.com", ReaderRole: "annals.Reader"}, st, nil, nil, log); err == nil {
		t.Error("a prefixed role was accepted")
	}
}

func TestBoardPages(t *testing.T) {
	r := newRig(t)
	ids := r.ingest("beacon", "graft", "ion", "curl")
	for path, want := range map[string]string{
		"/?since=":                 "Task 42",
		"/?since=&outcome=refused": "permission rule denied",
		"/events/" + ids[1]:        "given by",
		"/actors/beacon-agent":     "Worth a look",
		"/targets/github/repository/" + url.PathEscape("example-org/example-repo"): "Who touched it",
		"/approvals?since=": "approved by",
	} {
		resp, body := r.page(path, reader)
		if resp.StatusCode != http.StatusOK || !strings.Contains(body, want) {
			t.Errorf("%s: %d, missing %q", path, resp.StatusCode, want)
		}
	}
	if resp, _ := r.page("/events/01JBX7Y8Z9ABCDEFGHJKMNPQRS", reader); resp.StatusCode != http.StatusNotFound {
		t.Errorf("missing event: %d", resp.StatusCode)
	}
}

func TestTokenAdministration(t *testing.T) {
	r := newRig(t)
	post := func(path string, form url.Values, origin string) (*http.Response, string) {
		req, _ := http.NewRequest("POST", r.session.URL+path, strings.NewReader(form.Encode()))
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		for k, v := range admin {
			req.Header.Set(k, v)
		}
		if origin != "" {
			req.Header.Set("Origin", origin)
		}
		c := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
		resp, err := c.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		b, _ := io.ReadAll(resp.Body)
		return resp, string(b)
	}
	form := url.Values{"name": {"beacon prod"}, "source": {"beacon"}}
	if resp, _ := post("/tokens", form, "https://evil.example.com"); resp.StatusCode != http.StatusForbidden {
		t.Errorf("foreign origin: %d", resp.StatusCode)
	}
	if resp, _ := post("/tokens", url.Values{"name": {"x"}, "source": {"Not A Slug"}}, "https://annals.example.com"); resp.StatusCode != http.StatusBadRequest {
		t.Errorf("bad source: %d", resp.StatusCode)
	}
	resp, body := post("/tokens", form, "https://annals.example.com")
	if resp.StatusCode != http.StatusOK || !strings.Contains(body, "an_") {
		t.Fatalf("mint: %d", resp.StatusCode)
	}
	i := strings.Index(body, "an_")
	secret := body[i : i+3+52]
	if resp, _ := r.do("GET", "/v1/whoami", secret, nil); resp.StatusCode != http.StatusOK {
		t.Errorf("minted token refused: %d", resp.StatusCode)
	}
	list, _ := r.store.Tokens(context.Background())
	if resp, _ := post("/tokens/"+list[0].ID+"/revoke", nil, "https://annals.example.com"); resp.StatusCode != http.StatusSeeOther {
		t.Errorf("revoke: %d", resp.StatusCode)
	}
	if resp, _ := r.do("GET", "/v1/whoami", secret, nil); resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("revoked token admitted: %d", resp.StatusCode)
	}
}

func TestTelemetryRelay(t *testing.T) {
	var got map[string]any
	var key string
	collector := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		key = req.Header.Get("x-api-key")
		json.NewDecoder(req.Body).Decode(&got)
		w.WriteHeader(http.StatusAccepted)
	}))
	defer collector.Close()
	batch := `{"meta":{"app":{"name":"spoofed"},"user":{"id":"spoofed"}},"logs":[]}`
	send := func(r *rig) int {
		req, _ := http.NewRequest("POST", r.session.URL+"/api/telemetry", strings.NewReader(batch))
		for k, v := range reader {
			req.Header.Set(k, v)
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		return resp.StatusCode
	}
	if code := send(newRig(t)); code != http.StatusAccepted || got != nil {
		t.Errorf("without a key the batch is accepted and dropped: %d %v", code, got)
	}
	if code := send(newRigWith(t, Config{FaroURL: collector.URL, FaroKey: "k1"})); code != http.StatusAccepted || key != "k1" {
		t.Fatalf("relay: %d key=%q", code, key)
	}
	meta := got["meta"].(map[string]any)
	if meta["app"].(map[string]any)["name"] != "annals-web" || meta["user"].(map[string]any)["email"] != "reader@example.com" {
		t.Errorf("meta not stamped: %v", meta)
	}
}

var pii = map[string]string{"X-Forwarded-Email": "hr@example.com", "X-Forwarded-Groups": "PiiReader"}

// A reader sees what happened with every person as a ref; a PII reader and
// an admin see who. Only an admin opens tokens.
func TestRolesOnTheBoard(t *testing.T) {
	r := newRig(t)
	ids := r.ingest("sharepoint-share", "copilot-prompt", "copilot-tool")
	people := []string{"auditor@example.org", "Outside Auditor", "owner@example.com", "Site Owner", "writer@example.com", "Plan Writer"}
	for _, path := range []string{"/?since=", "/events/" + ids[0], "/events/" + ids[1], "/events/" + ids[2]} {
		resp, body := r.page(path, reader)
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("%s: %d", path, resp.StatusCode)
		}
		for _, who := range people {
			if strings.Contains(body, who) {
				t.Errorf("a reader saw %q on %s", who, path)
			}
		}
		if !strings.Contains(body, "people hidden") {
			t.Errorf("%s does not say people are hidden", path)
		}
	}
	if _, body := r.page("/events/"+ids[0], reader); !strings.Contains(body, "Finance Close") || !strings.Contains(body, "outside") {
		t.Error("a reader still sees what was shared, and that it went outside")
	}
	for _, who := range []map[string]string{pii, admin} {
		_, body := r.page("/events/"+ids[0], who)
		if !strings.Contains(body, "auditor@example.org") || !strings.Contains(body, "Site Owner") {
			t.Errorf("%s does not see who shared with whom", who["X-Forwarded-Groups"])
		}
	}
	if resp, _ := r.page("/tokens", pii); resp.StatusCode != http.StatusForbidden {
		t.Errorf("a PII reader on tokens: %d", resp.StatusCode)
	}
	_, body := r.page("/?since=&party=auditor@example.org", reader)
	if strings.Contains(body, "Finance Close") {
		t.Error("a reader found a person by email")
	}
	_, body = r.page("/?since=&party=auditor@example.org", pii)
	if !strings.Contains(body, "Finance Close") {
		t.Error("a PII reader finds what was shared with a person")
	}
}

// A prompt shows the tool calls it led to, and a tool call the prompt that
// caused it.
func TestCauseLinksOnTheBoard(t *testing.T) {
	r := newRig(t)
	ids := r.ingest("copilot-prompt", "copilot-tool")
	_, body := r.page("/events/"+ids[0], pii)
	if !strings.Contains(body, "Led to <span class=\"count\">1</span>") || !strings.Contains(body, "comet.search_projects") || !strings.Contains(body, "Q4 plan.docx") {
		t.Errorf("the prompt does not show what it led to or touched")
	}
	_, body = r.page("/events/"+ids[1], pii)
	if !strings.Contains(body, "Set off by") || !strings.Contains(body, "prompt.send") {
		t.Errorf("the tool call does not show its prompt")
	}
}

// A token reads at its own level.
func TestAPITokenReadLevels(t *testing.T) {
	r := newRig(t)
	r.ingest("sharepoint-share")
	_, body := r.do("GET", "/v1/events?source=m365", r.token("m365"), nil)
	if strings.Contains(string(body), "auditor@example.org") || strings.Contains(string(body), "owner@example.com") {
		t.Errorf("a reader token saw people: %s", body)
	}
	_, body = r.do("GET", "/v1/events?source=m365", r.tokenReads("audit", "pii"), nil)
	if !strings.Contains(string(body), "auditor@example.org") {
		t.Errorf("a pii token sees people: %s", body)
	}
}

// A person with no name is listed by email for those who see people, and by
// ref for a reader, never by a bare directory id.
func TestAPersonWithNoNameIsListedByEmail(t *testing.T) {
	r := newRig(t)
	ev := example(t, "copilot-prompt")
	ev.Actor.Name = ""
	if rc := r.srv.Ingest(context.Background(), []annals.Event{ev}, ev.Source)[0]; rc.Status != "stored" {
		t.Fatal(rc)
	}
	_, body := r.page("/records?since=", pii)
	if !strings.Contains(body, ">writer@example.com</a>") {
		t.Error("a PII reader sees the email where there is no name")
	}
	_, body = r.page("/records?since=", reader)
	if strings.Contains(body, "writer@example.com") || strings.Contains(body, ">user-0002<") || !strings.Contains(body, ">"+r.store.Masker().Ref("user-0002")+"</a>") {
		t.Error("a reader sees the ref")
	}
}

// A change's events join it from every sender, on the board and the API.
func TestChangeEventsJoinAcrossSenders(t *testing.T) {
	r := newRig(t)
	ids := r.ingest("change-stated", "change-routed", "change-decided", "change-ended", "change-observed")
	_, body := r.page("/events/"+ids[0], pii)
	if !strings.Contains(body, "Led to <span class=\"count\">4</span>") || !strings.Contains(body, "change.observed") {
		t.Errorf("the change.stated record does not show the four events that followed it")
	}
	_, body = r.page("/events/"+ids[4], pii)
	if !strings.Contains(body, "Set off by") || !strings.Contains(body, "change.stated") {
		t.Errorf("the watch's record does not show the change it watched")
	}
	tok := r.token("reader")
	resp, body2 := r.do("GET", "/v1/events?cause=run:01JC0A:stated&cause_source=orrery&source=dispatch", tok, nil)
	var p store.Page
	json.Unmarshal(body2, &p)
	if resp.StatusCode != http.StatusOK || len(p.Events) != 1 || p.Events[0].Action.Verb != "change.observed" {
		t.Errorf("cause_source: %d %s", resp.StatusCode, body2)
	}
}

// A change.stated record posted over the API reads back with its whole
// statement, so a replay rebuilds the statement its digest names.
func TestAPIKeepsTheWholeStatement(t *testing.T) {
	r := newRig(t)
	b, err := os.ReadFile("../../schema/examples/change-stated.json")
	if err != nil {
		t.Fatal(err)
	}
	if resp, body := r.do("POST", "/v1/events", r.token("orrery"), b); resp.StatusCode != http.StatusCreated {
		t.Fatalf("post: %d %s", resp.StatusCode, body)
	}
	resp, body := r.do("GET", "/v1/events?verb=change.stated", r.tokenReads("trellis", "reader"), nil)
	var p store.Page
	if err := json.Unmarshal(body, &p); err != nil || resp.StatusCode != http.StatusOK || len(p.Events) != 1 {
		t.Fatalf("read: %d %s", resp.StatusCode, body)
	}
	ev := p.Events[0]
	if ev.Statement == nil || len(ev.Statement.Unknown) != 1 || ev.Statement.Unknown[0].Part != "site_config.cors" ||
		ev.Statement.Writer.Version != "3.1.0" || len(ev.Statement.Things) != 1 || ev.Statement.Artifact.Kind != "plan" || ev.Effects[0].ClaimRule != "app-settings-restart" {
		t.Fatalf("the record lost part of its statement: %s", body)
	}
	posted := example(t, "change-stated")
	st := ev.StatementOf()
	st.Actor = posted.StatementOf().Actor // a reader sees the person it acts for as a ref
	if d, err := st.Digest(); err != nil || d != ev.Change.AfterHash {
		t.Errorf("the rebuilt statement's digest is %s (%v), the record's %s", d, err, ev.Change.AfterHash)
	}
}

// GET /v1/proof answers for one key from what the watch saw, to a token.
func TestAPIProof(t *testing.T) {
	r := newRig(t)
	r.ingest("change-stated", "change-observed")
	tok := r.token("reader")
	resp, body := r.do("GET", "/v1/proof?kind=stops&blueprint=workload&path=in-place", tok, nil)
	var got store.Proof
	json.Unmarshal(body, &got)
	want := store.Proof{ProofKey: store.ProofKey{Kind: "stops", Blueprint: "workload", Path: "in-place"}, Watched: 1, Blind: 1,
		LastSeen: time.Date(2026, 11, 26, 15, 44, 5, 0, time.UTC)}
	if resp.StatusCode != http.StatusOK || got != want {
		t.Errorf("proof: %d %s", resp.StatusCode, body)
	}
	resp, body = r.do("GET", "/v1/proof?kind=grants&blueprint=identity&path=in-place", tok, nil)
	if resp.StatusCode != http.StatusOK || strings.Contains(string(body), "last_seen") || !strings.Contains(string(body), `"watched":0`) {
		t.Errorf("a stated effect is no proof: %d %s", resp.StatusCode, body)
	}
	if resp, _ := r.do("GET", "/v1/proof?kind=stops&blueprint=workload", tok, nil); resp.StatusCode != http.StatusBadRequest {
		t.Errorf("a key with no path: %d", resp.StatusCode)
	}
	if resp, _ := r.do("GET", "/v1/proof?kind=stops&blueprint=workload&path=in-place", "", nil); resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("proof with no token: %d", resp.StatusCode)
	}
}

// GET /v1/gate/families and /v1/gate/drafts score the weeks from the
// change events, as tables, to a token.
func TestAPIGate(t *testing.T) {
	r := newRig(t)
	r.srv.now = func() time.Time { return time.Date(2026, 11, 27, 12, 0, 0, 0, time.UTC) }
	r.ingest("change-stated", "change-routed", "change-decided", "change-ended", "change-observed")
	tok := r.token("reader")
	var fams struct {
		Columns []struct{ ID string }
		Rows    [][]any
	}
	resp, body := r.do("GET", "/v1/gate/families?weeks=2", tok, nil)
	if err := json.Unmarshal(body, &fams); err != nil || resp.StatusCode != http.StatusOK || len(fams.Rows) != 4 {
		t.Fatalf("families: %d %s", resp.StatusCode, body)
	}
	cell := func(row []any, id string) any {
		for i, c := range fams.Columns {
			if c.ID == id {
				return row[i]
			}
		}
		t.Fatalf("no column %s", id)
		return nil
	}
	got := map[string]string{}
	for _, row := range fams.Rows {
		if cell(row, "week") == "2026-11-23T00:00:00Z" {
			got[cell(row, "family").(string)] = fmt.Sprint(cell(row, "score"), " ", cell(row, "changes"), " ", cell(row, "asks"), " ", cell(row, "unknowns"), " ", cell(row, "wait_median"))
		}
	}
	// The example's one stops sight is clean and the other blind, so its
	// disruption is unknown; its grant is never watched.
	want := map[string]string{"disruption": "too blind 1 1 1 9.6", "exposure": "too blind 1 1 1 9.6"}
	if fmt.Sprint(got) != fmt.Sprint(want) {
		t.Errorf("this week: %v, want %v", got, want)
	}
	resp, body = r.do("GET", "/v1/gate/drafts?weeks=2", tok, nil)
	if resp.StatusCode != http.StatusOK || !strings.Contains(string(body), `"acme-gate-next",1,1,1,0,1,0]`) {
		t.Errorf("drafts: %d %s", resp.StatusCode, body)
	}
	if resp, _ := r.do("GET", "/v1/gate/families?weeks=53", tok, nil); resp.StatusCode != http.StatusBadRequest {
		t.Errorf("53 weeks: %d", resp.StatusCode)
	}
	if resp, _ := r.do("GET", "/v1/gate/drafts", "", nil); resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("no token: %d", resp.StatusCode)
	}
}
