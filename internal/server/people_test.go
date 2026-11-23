package server

import (
	"context"
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/orchard-suite/platformkit/v3/annals"

	"github.com/orchard-suite/annals/internal/people"
	"github.com/orchard-suite/annals/internal/selfaudit"
)

const patOID = "00000000-0000-0000-0000-000000000001"

// twoIDs stores one record for a person posted by their directory id and
// one posted by an old product's own user id, and lets the directory know
// the person.
func twoIDs(t *testing.T, r *rig) {
	t.Helper()
	for i, id := range []string{patOID, "beacon-user-7"} {
		ev := annals.Event{At: time.Now().Add(-time.Duration(i) * time.Minute), Source: "test", Actor: annals.Actor{Kind: "person", ID: id},
			Action: annals.Action{Verb: "file.share", Target: annals.Target{System: "sharepoint", Kind: "file", ID: "f1"}, Outcome: "ok"}}
		if rc := r.srv.Ingest(context.Background(), []annals.Event{ev}, "test")[0]; rc.Status != "stored" {
			t.Fatal(rc)
		}
	}
	if err := r.store.WriteDirectory(context.Background(), []people.Person{{ID: patOID, Name: "Pat Example", Email: "pat@example.com"}}, nil); err != nil {
		t.Fatal(err)
	}
}

func TestAnAdminTiesAnIDToAPerson(t *testing.T) {
	r := newRig(t)
	twoIDs(t, r)
	_, body := r.page("/actors/beacon-user-7?since=", admin)
	if !strings.Contains(body, "Same person as") || strings.Contains(body, "One person") {
		t.Error("an id the directory does not know offers the admin a tie")
	}
	if _, body := r.page("/actors/beacon-user-7?since=", pii); strings.Contains(body, "Same person as") {
		t.Error("only an admin ties")
	}
	if resp, _ := r.form("/actors/beacon-user-7/link", url.Values{"person": {"pat@example.com"}}, pii); resp.StatusCode != http.StatusForbidden {
		t.Errorf("a PII reader cannot tie: %d", resp.StatusCode)
	}
	if resp, _ := r.form("/actors/beacon-user-7/link", url.Values{"person": {"nobody@example.com"}}, admin); resp.StatusCode != http.StatusBadRequest {
		t.Errorf("a person the directory does not hold: %d", resp.StatusCode)
	}
	resp, _ := r.form("/actors/beacon-user-7/link", url.Values{"person": {"pat@example.com"}}, admin)
	if resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("tie: %d", resp.StatusCode)
	}

	_, body = r.page("/actors/beacon-user-7?since=", pii)
	if !strings.Contains(body, "One person") || !strings.Contains(body, "Pat Example") || !strings.Contains(body, patOID) || !strings.Contains(body, "<b>2</b> records since") {
		t.Error("a PII reader sees one person with both ids and both records")
	}
	_, body = r.page("/actors/"+r.store.Masker().Ref(patOID)+"?since=", reader)
	if strings.Contains(body, "Pat Example") || strings.Contains(body, "beacon-user-7") || strings.Contains(body, "One person") || !strings.Contains(body, "<b>2</b> records since") {
		t.Error("a reader follows the person's one ref to both records, unnamed")
	}

	rec := r.selfRecords(selfaudit.VerbPeopleLink)
	if len(rec) != 1 || rec[0].Actor.Email != "admin@example.com" || rec[0].Action.Target.ID != patOID || rec[0].Parties[0].ID != "beacon-user-7" {
		t.Fatalf("the tie is on record: %+v", rec)
	}

	resp, _ = r.form("/actors/beacon-user-7/unlink", url.Values{"back": {"/actors/" + patOID}}, admin)
	if resp.StatusCode != http.StatusSeeOther || resp.Header.Get("Location") != "/actors/"+patOID {
		t.Fatalf("untie: %d %s", resp.StatusCode, resp.Header.Get("Location"))
	}
	if _, ok := r.store.Directory().Find("beacon-user-7"); ok {
		t.Error("the tie is gone")
	}
	if rec := r.selfRecords(selfaudit.VerbPeopleUnlink); len(rec) != 1 {
		t.Errorf("the untie is on record: %d", len(rec))
	}
}
