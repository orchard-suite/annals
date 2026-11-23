package store_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/orchard-suite/platformkit/v3/annals"

	"github.com/orchard-suite/annals/internal/people"
	. "github.com/orchard-suite/annals/internal/store"
)

const oid = "00000000-0000-0000-0000-000000000001"

// byPerson is one record for one person under each id products posted
// them by: their directory id, their email, and an old product's user id.
func byPerson(t *testing.T, s *Store) {
	t.Helper()
	now := time.Now()
	for i, id := range []string{oid, "pat@example.com", "beacon-user-7"} {
		ev := mk("m365", id, "file.share", "sharepoint", "f1", "ok", now.Add(-time.Duration(i)*time.Minute))
		ev.Actor.Kind = annals.PersonKind
		ev.Authority = &annals.Authority{Approval: &annals.Approval{Required: true, By: id}}
		if _, _, err := put(t, s, ev); err != nil {
			t.Fatal(err)
		}
	}
	other := mk("m365", "someone@example.com", "file.share", "sharepoint", "f1", "ok", now)
	other.Actor.Kind = annals.PersonKind
	if _, _, err := put(t, s, other); err != nil {
		t.Fatal(err)
	}
}

func knowPat(t *testing.T, s *Store) {
	t.Helper()
	if err := s.WriteDirectory(context.Background(), []people.Person{{ID: oid, Name: "Pat Example", Email: "pat@example.com"}}, nil); err != nil {
		t.Fatal(err)
	}
}

func TestAPersonUnderEveryIDIsOneActor(t *testing.T) {
	s := open(t)
	ctx := context.Background()
	byPerson(t, s)
	knowPat(t, s)
	if _, _, err := s.Link(ctx, "beacon-user-7", "PAT@example.com", "admin@example.com"); err != nil {
		t.Fatal(err)
	}
	for _, v := range []string{oid, "pat@example.com", "beacon-user-7"} {
		if n, err := s.Count(ctx, Filter{Actor: v}); err != nil || n != 3 {
			t.Errorf("a PII reader filtering by %s finds every id of the person: %d %v", v, n, err)
		}
	}
	ref := s.Masker().Ref(oid)
	if n, _ := s.Count(ctx, Filter{Actor: ref, Masked: true}); n != 3 {
		t.Errorf("a reader filtering by the person's ref finds all three: %d", n)
	}
	if n, _ := s.Count(ctx, Filter{Actor: "pat@example.com", Masked: true}); n != 0 {
		t.Errorf("a reader cannot find a person by email: %d", n)
	}

	bs, err := s.Breakdown(ctx, Filter{}, "actor_id", 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(bs) != 2 || bs[0].Key != oid || bs[0].Count != 3 || bs[0].Label != "Pat Example" {
		t.Errorf("one bucket for the person, named: %+v", bs)
	}
	masked, _ := s.Breakdown(ctx, Filter{Masked: true}, "actor_id", 10)
	if len(masked) != 2 || masked[0].Key != ref || masked[0].Count != 3 || masked[0].Label != "" {
		t.Errorf("one ref for the person, unnamed: %+v", masked)
	}
	approvers, _ := s.Breakdown(ctx, Filter{ApprovalRequired: true}, "approval_by", 10)
	if len(approvers) != 1 || approvers[0].Count != 3 {
		t.Errorf("approvers are merged too: %+v", approvers)
	}
	st, err := s.Stats(ctx)
	if err != nil || st.Actors != 2 {
		t.Errorf("stats count the person once: %d %v", st.Actors, err)
	}
}

func TestAnAdminLinkIsKeptAndUndone(t *testing.T) {
	s := open(t)
	ctx := context.Background()
	knowPat(t, s)
	if _, _, err := s.Link(ctx, "x", "nobody@example.com", "admin@example.com"); !errors.Is(err, ErrNotFound) {
		t.Errorf("a person the directory does not hold: %v", err)
	}
	if err := s.WriteDirectory(ctx, []people.Person{{ID: "other-oid", Email: "other@example.com"}}, nil); err != nil {
		t.Fatal(err)
	}
	if _, _, err := s.Link(ctx, "other@example.com", oid, "admin@example.com"); !errors.Is(err, ErrConflict) {
		t.Errorf("another person's own email cannot be tied to someone else: %v", err)
	}
	if _, _, err := s.Link(ctx, "beacon-user-7", oid, "admin@example.com"); err != nil {
		t.Fatal(err)
	}
	// The directory is read back from the database, as on a restart.
	d, err := s.LoadDirectory(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if p, ok := d.Find("beacon-user-7"); !ok || p.ID != oid {
		t.Errorf("the link survives a reload: %+v %v", p, ok)
	}
	l, err := s.Unlink(ctx, "beacon-user-7", "admin@example.com")
	if err != nil || l.Person != oid {
		t.Fatalf("unlink: %+v %v", l, err)
	}
	if _, ok := s.Directory().Find("beacon-user-7"); ok {
		t.Error("the link is gone")
	}
	if _, err := s.Unlink(ctx, "pat@example.com", "admin@example.com"); !errors.Is(err, ErrNotFound) {
		t.Errorf("a person's own email is not a link to undo: %v", err)
	}
}

func TestADirectoryLinkIsReplacedByItsNewestOwner(t *testing.T) {
	s := open(t)
	ctx := context.Background()
	knowPat(t, s)
	if err := s.WriteDirectory(ctx, []people.Person{{ID: "other-oid"}}, []people.Link{{ID: "old@example.com", Person: oid, Source: people.LinkDirectory}}); err != nil {
		t.Fatal(err)
	}
	if err := s.WriteDirectory(ctx, nil, []people.Link{{ID: "old@example.com", Person: "other-oid", Source: people.LinkDirectory}}); err != nil {
		t.Fatal(err)
	}
	if p, _ := s.Directory().Find("old@example.com"); p.ID != "other-oid" {
		t.Errorf("the newest owner of an address has it: %+v", p)
	}
}

func TestAnIDPostedWithAKnownEmailMatchesThePerson(t *testing.T) {
	s := open(t)
	ctx := context.Background()
	knowPat(t, s)
	login := "i:0h.f|membership|0000000000004242@live.example"
	ev := mk("m365", login, "file.share", "onedrive", "f1", "ok", time.Now())
	ev.Actor.Kind, ev.Actor.Email = annals.PersonKind, "Pat@Example.com"
	if _, _, err := put(t, s, ev); err != nil {
		t.Fatal(err)
	}
	if n, err := s.LinkPosted(ctx, time.Now().Add(time.Second)); err != nil || n != 1 {
		t.Fatalf("the login is tied to Pat: %d %v", n, err)
	}
	if n, _ := s.Count(ctx, Filter{Actor: oid}); n != 1 {
		t.Errorf("Pat's record under the login is Pat's: %d", n)
	}
	if n, _ := s.Count(ctx, Filter{Actor: s.Masker().Ref(oid), Masked: true}); n != 1 {
		t.Errorf("a reader finds it by Pat's ref: %d", n)
	}
	if l, ok := s.Directory().LinkOf(login); !ok || l.Source != people.LinkPosted {
		t.Errorf("the link says a record made it: %+v %v", l, ok)
	}
	if n, err := s.LinkPosted(ctx, time.Now().Add(time.Second)); err != nil || n != 0 {
		t.Errorf("a later call reads only newer records: %d %v", n, err)
	}
}

func TestAnIDPostedWithAnUnknownEmailStaysItsOwn(t *testing.T) {
	s := open(t)
	ctx := context.Background()
	knowPat(t, s)
	ev := mk("m365", "beacon-user-9", "file.share", "onedrive", "f1", "ok", time.Now())
	ev.Actor.Kind, ev.Actor.Email = annals.PersonKind, "stranger@example.org"
	if _, _, err := put(t, s, ev); err != nil {
		t.Fatal(err)
	}
	if n, err := s.LinkPosted(ctx, time.Now().Add(time.Second)); err != nil || n != 0 {
		t.Errorf("nobody owns the email, so nothing is tied: %d %v", n, err)
	}
}
