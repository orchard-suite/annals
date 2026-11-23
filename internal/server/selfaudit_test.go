package server

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/url"
	"strings"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/orchard-suite/platformkit/v3/annals"

	"github.com/orchard-suite/annals/internal/people"
	"github.com/orchard-suite/annals/internal/selfaudit"
	"github.com/orchard-suite/annals/internal/store"
)

// selfRecords waits for the auditor and returns Annals' own records
// with verb, newest first.
func (r *rig) selfRecords(verb string) []annals.Event {
	r.t.Helper()
	r.audit.Wait()
	p, err := r.store.Query(context.Background(), store.Filter{Source: selfaudit.Source, Verb: verb, Limit: store.MaxLimit})
	if err != nil {
		r.t.Fatal(err)
	}
	return p.Events
}

// form posts a board form as headers' person, from the board's origin.
func (r *rig) form(path string, form url.Values, headers map[string]string) (*http.Response, string) {
	r.t.Helper()
	req, _ := http.NewRequest("POST", r.session.URL+path, strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Origin", "https://annals.example.com")
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	c := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	resp, err := c.Do(req)
	if err != nil {
		r.t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return resp, string(b)
}

func TestSelfRecordsTokenIssueAndRevoke(t *testing.T) {
	r := newRig(t)
	resp, body := r.form("/tokens", url.Values{"name": {"beacon prod"}, "source": {"beacon"}, "reads": {"pii"}}, admin)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("mint: %d", resp.StatusCode)
	}
	i := strings.Index(body, "an_")
	secret := body[i : i+3+52]
	list, _ := r.store.Tokens(context.Background())
	tok := list[0]

	issued := r.selfRecords(selfaudit.VerbTokenIssue)
	if len(issued) != 1 {
		t.Fatalf("token.issue records: %d", len(issued))
	}
	ev := issued[0]
	if ev.Actor.Kind != "person" || ev.Actor.ID != "admin@example.com" || ev.Actor.Email != "admin@example.com" {
		t.Errorf("actor: %+v", ev.Actor)
	}
	if ev.Action.Target != (annals.Target{System: "annals", Kind: "token", ID: tok.ID, Title: "beacon prod"}) || ev.Action.Outcome != "ok" {
		t.Errorf("action: %+v", ev.Action)
	}
	if ev.Labels["source"] != "beacon" || ev.Labels["reads"] != "pii" || ev.EventKey != "token.issue:"+tok.ID {
		t.Errorf("labels and key: %v %q", ev.Labels, ev.EventKey)
	}
	if raw, _ := json.Marshal(ev); strings.Contains(string(raw), secret) || strings.Contains(string(raw), "an_") {
		t.Errorf("the record holds the secret: %s", raw)
	}

	if resp, _ := r.form("/tokens/"+tok.ID+"/revoke", nil, admin); resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("revoke: %d", resp.StatusCode)
	}
	// Revoking a token that is no longer live records nothing more.
	r.form("/tokens/"+tok.ID+"/revoke", nil, admin)
	revoked := r.selfRecords(selfaudit.VerbTokenRevoke)
	if len(revoked) != 1 || revoked[0].Action.Target.ID != tok.ID || revoked[0].Actor.ID != "admin@example.com" || revoked[0].Labels["source"] != "beacon" {
		t.Fatalf("token.revoke records: %+v", revoked)
	}

	// A reader sees the admin as a ref.
	_, page := r.page("/?since=&source=annals", reader)
	if strings.Contains(page, "admin@example.com") || !strings.Contains(page, r.store.Masker().Ref("admin@example.com")) {
		t.Error("a reader sees the admin behind the token records as a ref only")
	}
}

func TestSelfRecordsPeopleViewOnTheBoard(t *testing.T) {
	r := newRig(t)
	ids := r.ingest("sharepoint-share")
	for _, path := range []string{"/?since=", "/events/" + ids[0], "/approvals", "/?since=7d"} {
		r.page(path, pii)
		r.page(path, reader)
	}
	views := r.selfRecords(selfaudit.VerbPeopleView)
	if len(views) != 1 {
		t.Fatalf("people.view records after a PII reader's four pages: %d", len(views))
	}
	ev := views[0]
	if ev.Actor.ID != "hr@example.com" || ev.Labels["level"] != "pii" || ev.Labels["door"] != "board" {
		t.Errorf("record: %+v", ev)
	}
	if strings.Contains(ev.EventKey, "hr@example.com") || !strings.Contains(ev.EventKey, r.store.Masker().Ref("hr@example.com")) {
		t.Errorf("the key names the person, not their ref: %q", ev.EventKey)
	}

	// An admin is a viewer of their own, at their own level.
	r.page("/", admin)
	r.page("/tokens", admin)
	if views := r.selfRecords(selfaudit.VerbPeopleView); len(views) != 2 || views[0].Labels["level"] != "admin" {
		t.Errorf("after an admin's pages: %+v", views)
	}

	// A restarted Annals forgets what it wrote today; the key still holds
	// one record.
	fresh := selfaudit.New(r.store, r.srv.metrics, r.srv.log)
	fresh.PeopleViewed(context.Background(), fresh.PersonViewer("hr@example.com"), people.PII, selfaudit.DoorBoard)
	fresh.Wait()
	if views := r.selfRecords(selfaudit.VerbPeopleView); len(views) != 2 {
		t.Errorf("after a restart: %d records", len(views))
	}
}

func TestSelfRecordsPeopleViewOnTheAPI(t *testing.T) {
	r := newRig(t)
	r.ingest("sharepoint-share")
	piiTok := r.tokenReads("audit", "pii")
	for range 3 {
		r.do("GET", "/v1/events", piiTok, nil)
	}
	r.do("GET", "/v1/events", r.token("m365"), nil)
	views := r.selfRecords(selfaudit.VerbPeopleView)
	if len(views) != 1 {
		t.Fatalf("people.view records: %d", len(views))
	}
	ev := views[0]
	if ev.Actor.Kind != "system" || ev.Actor.Name != "audit test" || ev.Authority == nil || ev.Authority.TokenID != ev.Actor.ID || ev.Labels["door"] != "api" || ev.Labels["level"] != "pii" {
		t.Errorf("record: %+v", ev)
	}

	// The MCP door, through a token of its own.
	mcpTok := r.tokenReads("agent", "pii")
	client := mcp.NewClient(&mcp.Implementation{Name: "test", Version: "0"}, nil)
	sess, err := client.Connect(context.Background(), &mcp.StreamableClientTransport{
		Endpoint: r.api.URL + "/mcp", HTTPClient: &http.Client{Transport: bearer{mcpTok}},
	}, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer sess.Close()
	if views := r.selfRecords(selfaudit.VerbPeopleView); len(views) != 1 {
		t.Errorf("connecting showed nobody, yet %d records", len(views))
	}
	if _, err := sess.CallTool(context.Background(), &mcp.CallToolParams{Name: "annals_search", Arguments: map[string]any{}}); err != nil {
		t.Fatal(err)
	}
	views = r.selfRecords(selfaudit.VerbPeopleView)
	if len(views) != 2 || views[0].Labels["door"] != "mcp" || views[0].Actor.Name != "agent test" {
		t.Errorf("after an MCP search: %+v", views)
	}
}

// whoami names who issued the token; a reader token sees them as a ref.
func TestWhoamiMasksTheIssuerForAReader(t *testing.T) {
	r := newRig(t)
	_, body := r.do("GET", "/v1/whoami", r.token("m365"), nil)
	if strings.Contains(string(body), "admin@example.com") || !strings.Contains(string(body), r.store.Masker().Ref("admin@example.com")) {
		t.Errorf("a reader token saw its issuer: %s", body)
	}
	_, body = r.do("GET", "/v1/whoami", r.tokenReads("audit", "pii"), nil)
	if !strings.Contains(string(body), "admin@example.com") {
		t.Errorf("a pii token sees its issuer: %s", body)
	}
}

type bearer struct{ token string }

func (b bearer) RoundTrip(r *http.Request) (*http.Response, error) {
	r = r.Clone(r.Context())
	r.Header.Set("Authorization", "Bearer "+b.token)
	return http.DefaultTransport.RoundTrip(r)
}
