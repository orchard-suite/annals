package directory

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/orchard-suite/annals/internal/graph"
	"github.com/orchard-suite/annals/internal/store/storetest"
)

type token string

func (t token) Token(context.Context) (string, error) { return string(t), nil }

type counter map[string]int

func (c counter) DirectoryPass(_ context.Context, outcome string) { c[outcome]++ }

// fakeGraph answers the user delta: the first pass in two pages, then each
// later pass with the page set in later, keyed by the delta link it is on.
func fakeGraph(t *testing.T, later map[string]string) *httptest.Server {
	var srv *httptest.Server
	srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.Header.Get("Authorization") != "Bearer t" {
			w.WriteHeader(401)
			return
		}
		if r.URL.Path != "/users/delta" {
			t.Errorf("unexpected %s", r.URL)
			w.WriteHeader(404)
			return
		}
		q := r.URL.Query()
		switch {
		case q.Get("$deltatoken") != "":
			body, ok := later[q.Get("$deltatoken")]
			if !ok {
				w.WriteHeader(http.StatusGone)
				w.Write([]byte(`{"error":{"code":"syncStateNotFound","message":"start over"}}`))
				return
			}
			w.Write([]byte(strings.ReplaceAll(body, "SRV", srv.URL)))
		case q.Get("$skiptoken") == "2":
			w.Write([]byte(`{"value":[{"id":"OID-2","displayName":"Sam Example","mail":null,"userPrincipalName":"sam@example.com","proxyAddresses":[],"otherMails":["sam.home@example.org"]}],
				"@odata.deltaLink":"` + srv.URL + `/users/delta?$deltatoken=d1"}`))
		default:
			if q.Get("$select") == "" {
				t.Errorf("the first page asks for the fields it keeps: %s", r.URL)
			}
			w.Write([]byte(`{"value":[{"id":"oid-1","displayName":"Pat Example","mail":"Pat@example.com","userPrincipalName":"pat@example.com","proxyAddresses":["SMTP:pat@example.com","smtp:pat.alias@example.com","SIP:pat@example.com"]}],
				"@odata.nextLink":"` + srv.URL + `/users/delta?$skiptoken=2"}`))
		}
	}))
	t.Cleanup(srv.Close)
	return srv
}

func TestThePassesKeepTheDirectoryInStep(t *testing.T) {
	ctx := context.Background()
	st := storetest.Open(t)
	srv := fakeGraph(t, map[string]string{
		// Pat changes name only; Sam is deleted; then nothing changes.
		"d1": `{"value":[{"id":"oid-1","displayName":"Pat Renamed"},{"id":"oid-2","@removed":{"reason":"changed"}}],"@odata.deltaLink":"SRV/users/delta?$deltatoken=d2"}`,
		"d2": `{"value":[],"@odata.deltaLink":"SRV/users/delta?$deltatoken=d3"}`,
	})
	c := counter{}
	s := &Sync{Graph: graph.New(srv.URL, token("t"), srv.Client()), Store: st, Counter: c, Log: slog.New(slog.NewTextHandler(io.Discard, nil))}

	if err := s.Pass(ctx); err != nil {
		t.Fatal(err)
	}
	dir := st.Directory()
	for _, id := range []string{"oid-1", "PAT@example.com", "pat.alias@example.com"} {
		if p, ok := dir.Find(id); !ok || p.Name != "Pat Example" || p.Email != "pat@example.com" {
			t.Errorf("%s: %+v %v", id, p, ok)
		}
	}
	if _, ok := dir.Find("sip:pat@example.com"); ok {
		t.Error("a SIP address is not an email")
	}
	if p, ok := dir.Find("sam.home@example.org"); !ok || p.ID != "oid-2" || p.Email != "sam@example.com" {
		t.Errorf("a user with no mail is known by sign-in name and other mails: %+v %v", p, ok)
	}

	if err := s.Pass(ctx); err != nil {
		t.Fatal(err)
	}
	dir = st.Directory()
	if p, _ := dir.Find("pat.alias@example.com"); p.Name != "Pat Renamed" || p.Email != "pat@example.com" {
		t.Errorf("a field Graph left out keeps its value: %+v", p)
	}
	if p, ok := dir.Find("sam@example.com"); !ok || !p.Left || p.Name != "Sam Example" {
		t.Errorf("a deleted user stays, marked left: %+v %v", p, ok)
	}

	if err := s.Pass(ctx); err != nil {
		t.Fatal(err)
	}
	// d3 is unknown to the fake, as an expired delta link is to Graph.
	if err := s.Pass(ctx); err != nil {
		t.Fatalf("an expired delta link starts over: %v", err)
	}
	if p, _ := st.Directory().Find("oid-1"); p.Name != "Pat Example" {
		t.Errorf("starting over reads every user again: %+v", p)
	}
	if c["ok"] != 4 || c["failed"] != 0 {
		t.Errorf("passes counted: %v", c)
	}
}
