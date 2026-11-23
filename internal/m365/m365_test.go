package m365

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/orchard-suite/platformkit/v3/annals"

	"github.com/orchard-suite/annals/internal/graph"
)

func record(t *testing.T, raw string) Record {
	t.Helper()
	var r Record
	if err := json.Unmarshal([]byte(raw), &r); err != nil {
		t.Fatal(err)
	}
	return r
}

const guestShare = `{"id":"rec-1","createdDateTime":"2026-11-04T15:20:07Z","auditLogRecordType":"sharePointSharingOperation","operation":"SharingInvitationCreated","service":"SharePoint","userId":"owner@example.com","userPrincipalName":"owner@example.com","userType":"Regular",
 "auditData":{"Workload":"SharePoint","ItemType":"Web","ObjectId":"https://example.sharepoint.com/sites/finance-close","SiteUrl":"https://example.sharepoint.com/sites/finance-close","TargetUserOrGroupName":"Auditor@Example.org","TargetUserOrGroupType":"Guest","EventData":"<PermissionsGranted>Edit</PermissionsGranted>"}}`

const copilotRecord = `{"id":"rec-2","createdDateTime":"2026-11-04T16:02:11Z","auditLogRecordType":"copilotInteraction","operation":"CopilotInteraction","service":"Copilot","userId":"writer@example.com","userPrincipalName":"writer@example.com","userType":"Regular",
 "auditData":{"CopilotEventData":{"AppHost":"Word","ThreadId":"thread-91ac",
  "AccessedResources":[{"Id":"https://example.sharepoint.com/sites/plan/Q4.docx","Name":"Q4 plan for owner@example.com.docx","Type":"File","Action":"Read","SensitivityLabelId":"label-conf"}],
  "AISystemPlugin":[{"Id":"comet.search_projects","Name":"Comet project search"},{"Id":"","Name":""}]}}}`

// Every event a record maps to holds to Annals' contract.
func valid(t *testing.T, evs []annals.Event) {
	t.Helper()
	for _, ev := range evs {
		if err := ev.Validate("m365"); err != nil {
			t.Errorf("%s breaks the contract: %v", ev.EventKey, err)
		}
	}
}

func TestASharingRecordNamesWhoAndWithWhom(t *testing.T) {
	evs, ok := Map(record(t, guestShare))
	if !ok || len(evs) != 1 {
		t.Fatalf("mapped %v %d", ok, len(evs))
	}
	valid(t, evs)
	ev := evs[0]
	if ev.EventKey != "ual:rec-1" || ev.Action.Verb != "site.share" || ev.Action.Target.System != "sharepoint" || ev.Action.Target.Kind != "site" {
		t.Errorf("action %+v key %q", ev.Action, ev.EventKey)
	}
	if ev.Actor.Kind != "person" || ev.Actor.Email != "owner@example.com" {
		t.Errorf("actor %+v", ev.Actor)
	}
	if len(ev.Parties) != 1 || ev.Parties[0].Email != "auditor@example.org" || !ev.Parties[0].External || ev.Parties[0].Kind != "person" {
		t.Errorf("the guest is the recipient, by full email: %+v", ev.Parties)
	}
	if ev.Change == nil || ev.Change.Summary != "edit access" {
		t.Errorf("the access given: %+v", ev.Change)
	}
}

// Graph answers record types in Pascal case, as it found them in the audit
// log, though a query filters on them in camel case.
func TestRecordTypesMatchInEitherCase(t *testing.T) {
	r := record(t, guestShare)
	r.RecordType = "SharePointSharingOperation"
	if _, ok := Map(r); !ok {
		t.Error("a Pascal-case sharing record is skipped")
	}
	c := record(t, copilotRecord)
	c.RecordType = "CopilotInteraction"
	if _, ok := Map(c); !ok {
		t.Error("a Pascal-case Copilot record is skipped")
	}
}

func TestLinksAndOtherSharingOperations(t *testing.T) {
	r := record(t, guestShare)
	r.Operation = "AnonymousLinkCreated"
	evs, _ := Map(r)
	valid(t, evs)
	if p := evs[0].Parties; len(p) != 1 || p[0].Kind != "anyone" {
		t.Errorf("an anonymous link is shared with anyone: %+v", p)
	}
	r.Operation = "SharingInheritanceBroken"
	evs, _ = Map(r)
	valid(t, evs)
	if evs[0].Action.Verb != "site.sharing.sharinginheritancebroken" {
		t.Errorf("another sharing operation keeps its own verb: %s", evs[0].Action.Verb)
	}
	r.RecordType = "exchangeItem"
	if evs, ok := Map(r); ok || len(evs) != 0 {
		t.Error("a record type the collector does not read is skipped")
	}
}

func TestACopilotPromptLeadsToItsToolCalls(t *testing.T) {
	evs, ok := Map(record(t, copilotRecord))
	if !ok || len(evs) != 2 {
		t.Fatalf("a prompt and one tool call (the empty plugin is skipped): %v %d", ok, len(evs))
	}
	valid(t, evs)
	prompt, tool := evs[0], evs[1]
	if prompt.Action.Verb != "prompt.send" || prompt.Agent.Runtime != "copilot" || prompt.Agent.SessionID != "thread-91ac" || prompt.Actor.Email != "writer@example.com" {
		t.Errorf("prompt %+v %+v", prompt.Action, prompt.Agent)
	}
	if len(prompt.Items) != 1 || prompt.Items[0].Access != "read" || prompt.Items[0].Label != "label-conf" || strings.Contains(prompt.Items[0].Title, "@") {
		t.Errorf("the file it read, its label, and no email in its title: %+v", prompt.Items)
	}
	if tool.Cause != prompt.EventKey || tool.Action.Verb != "tool.call" || tool.Action.Target.ID != "comet.search_projects" || tool.Actor.Kind != "agent" || tool.Actor.OnBehalfOf != prompt.Actor.ID {
		t.Errorf("tool %+v %+v cause %q", tool.Actor, tool.Action, tool.Cause)
	}
}

func TestSharePointsOwnAccountIsASystem(t *testing.T) {
	r := record(t, guestShare)
	r.UserID, r.UserPrincipalName = "app@sharepoint", "app@sharepoint"
	evs, _ := Map(r)
	valid(t, evs)
	if a := evs[0].Actor; a.Kind != "system" || a.Email != "" {
		t.Errorf("SharePoint's app account is not a person: %+v", a)
	}
}

func TestAClaimsLoginNamesThePersonByTheirPrincipalName(t *testing.T) {
	r := record(t, guestShare)
	r.UserID, r.UserPrincipalName = "i:0h.f|membership|0000000000004242@live.example", "Owner@Example.com"
	evs, _ := Map(r)
	valid(t, evs)
	if a := evs[0].Actor; a.Kind != annals.PersonKind || a.ID != "owner@example.com" || a.Email != "owner@example.com" {
		t.Errorf("a claims login is posted as the person's principal name: %+v", a)
	}
}

func TestSharePointsOwnAccountIsASystemUnderAClaimsLogin(t *testing.T) {
	r := record(t, guestShare)
	r.UserID, r.UserPrincipalName = "i:0i.t|00000003-0000-0ff1-ce00-000000000000|app@sharepoint", ""
	evs, _ := Map(r)
	valid(t, evs)
	if a := evs[0].Actor; a.Kind != "system" || a.ID != "app@sharepoint" {
		t.Errorf("SharePoint's app account under a claims login is a system: %+v", a)
	}
}

func TestLongIDsStandInByHash(t *testing.T) {
	long := strings.Repeat("a", 400)
	if got := shortID(long); !strings.HasPrefix(got, "sha256:") || shortID(long+"b") == got {
		t.Errorf("a long id stands in by its hash: %q", got)
	}
}

// fakeGraph answers sign-in, one query that runs once before it succeeds,
// and its records in two pages.
func fakeGraph(t *testing.T, records []string) (*httptest.Server, *atomic.Int32) {
	var polls atomic.Int32
	var srv *httptest.Server
	srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case strings.HasSuffix(r.URL.Path, "/oauth2/v2.0/token"):
			r.ParseForm()
			if r.Form.Get("client_assertion") != "workload-token" || r.Form.Get("client_id") != "app" {
				w.WriteHeader(401)
				return
			}
			w.Write([]byte(`{"access_token":"graph-token","expires_in":3600}`))
		case r.Header.Get("Authorization") != "Bearer graph-token":
			w.WriteHeader(401)
		case r.Method == http.MethodPost && r.URL.Path == "/security/auditLog/queries":
			var body map[string]any
			json.NewDecoder(r.Body).Decode(&body)
			if types, _ := body["recordTypeFilters"].([]any); len(types) != 2 {
				t.Errorf("record types %v", body["recordTypeFilters"])
			}
			w.Write([]byte(`{"id":"q1","status":"notStarted"}`))
		case r.URL.Path == "/security/auditLog/queries/q1":
			if polls.Add(1) == 1 {
				w.Write([]byte(`{"id":"q1","status":"running"}`))
				return
			}
			w.Write([]byte(`{"id":"q1","status":"succeeded"}`))
		case r.URL.Path == "/security/auditLog/queries/q1/records" && r.URL.Query().Get("page") == "":
			w.Write([]byte(`{"value":[` + records[0] + `],"@odata.nextLink":"` + srv.URL + `/security/auditLog/queries/q1/records?page=2"}`))
		case r.URL.Path == "/security/auditLog/queries/q1/records":
			w.Write([]byte(`{"value":[` + strings.Join(records[1:], ",") + `]}`))
		default:
			t.Errorf("unexpected %s %s", r.Method, r.URL)
			w.WriteHeader(404)
		}
	}))
	return srv, &polls
}

type fakePoster struct{ posted []annals.Event }

func (f *fakePoster) PostBatch(_ context.Context, evs []annals.Event) ([]annals.Receipt, error) {
	f.posted = append(f.posted, evs...)
	out := make([]annals.Receipt, len(evs))
	for i, ev := range evs {
		out[i] = annals.Receipt{Index: i, Status: "stored", EventKey: ev.EventKey}
	}
	return out, nil
}

func TestAPassReadsTheQueryAndPostsEveryPage(t *testing.T) {
	srv, polls := fakeGraph(t, []string{guestShare, copilotRecord})
	defer srv.Close()
	tokenFile := filepath.Join(t.TempDir(), "token")
	os.WriteFile(tokenFile, []byte("workload-token\n"), 0o600)
	tokens := &graph.FederatedToken{LoginURL: srv.URL, TenantID: "tenant", ClientID: "app", AssertionFile: tokenFile, Scope: "s", HTTP: srv.Client()}
	post := &fakePoster{}
	c := &Collector{Graph: NewGraph(graph.New(srv.URL, tokens, srv.Client())), Post: post, Log: slog.New(slog.NewTextHandler(io.Discard, nil)),
		Lookback: 48 * time.Hour, Lag: 15 * time.Minute, Poll: time.Millisecond}
	if err := c.Pass(context.Background(), time.Now()); err != nil {
		t.Fatal(err)
	}
	if polls.Load() != 2 {
		t.Errorf("polled %d times, want until it succeeded", polls.Load())
	}
	if len(post.posted) != 3 {
		t.Errorf("posted %d events, want the share, the prompt, and its tool call", len(post.posted))
	}
}

func TestPassesReadTheRecentStretchAndSweepOnceADay(t *testing.T) {
	c := &Collector{Lookback: 48 * time.Hour, Recent: 3 * time.Hour, Sweep: 24 * time.Hour, Lag: 15 * time.Minute}
	start := time.Date(2026, 11, 11, 12, 0, 0, 0, time.UTC)
	reads := func(now time.Time) (time.Duration, bool) {
		from, to, sweep := c.window(now)
		if want := now.Add(-c.Lag); !to.Equal(want) {
			t.Errorf("a pass stops %s before now: %s", c.Lag, to)
		}
		return now.Sub(from), sweep
	}
	if reach, sweep := reads(start); !sweep || reach != 48*time.Hour {
		t.Errorf("the first pass sweeps: %s %v", reach, sweep)
	}
	c.swept = start
	if reach, sweep := reads(start.Add(30 * time.Minute)); sweep || reach != 3*time.Hour {
		t.Errorf("a pass between sweeps reads the recent stretch: %s %v", reach, sweep)
	}
	if _, sweep := reads(start.Add(24 * time.Hour)); !sweep {
		t.Error("a day after the last sweep, a pass sweeps again")
	}
	c.Recent = 0
	if _, sweep := reads(start.Add(time.Hour)); !sweep {
		t.Error("without a recent stretch every pass sweeps")
	}
}

func TestAFailedQueryFailsThePass(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"id":"q1","status":"failed"}`))
	}))
	defer srv.Close()
	c := &Collector{Graph: NewGraph(graph.New(srv.URL, staticToken("t"), srv.Client())), Post: &fakePoster{}, Log: slog.New(slog.NewTextHandler(io.Discard, nil)), Lookback: time.Hour, Poll: time.Millisecond}
	if err := c.Pass(context.Background(), time.Now()); err == nil {
		t.Error("a failed query fails the pass")
	}
}

type staticToken string

func (s staticToken) Token(context.Context) (string, error) { return string(s), nil }
