package people

import (
	"slices"
	"testing"

	"github.com/orchard-suite/platformkit/v3/annals"
)

const oid = "00000000-0000-0000-0000-000000000001"

// directory holds one person, posted by object id, by email, and by an old
// product's own user id an admin tied to them.
func directory() *Directory {
	return NewDirectory(
		[]Person{{ID: oid, Name: "Pat Example", Email: "pat@example.com"}},
		[]Link{{ID: "pat.alias@example.com", Person: oid, Source: LinkDirectory}, {ID: "beacon-user-7", Person: oid, Source: LinkAdmin}, {ID: "x", Person: "nobody", Source: LinkAdmin}},
	)
}

func TestEveryIDOfAPersonFindsThem(t *testing.T) {
	d := directory()
	for _, id := range []string{oid, "PAT@example.com", "pat.alias@example.com", "beacon-user-7"} {
		if p, ok := d.Find(id); !ok || p.ID != oid {
			t.Errorf("%s is not the person: %+v %v", id, p, ok)
		}
	}
	if _, ok := d.Find("x"); ok {
		t.Error("a link to someone the directory does not hold is dropped")
	}
	if got := d.IDs("beacon-user-7"); len(got) != 4 || got[0] != oid {
		t.Errorf("every id of the person, theirs first: %v", got)
	}
	if got := d.IDs("stranger@example.com"); !slices.Equal(got, []string{"stranger@example.com"}) {
		t.Errorf("an unknown id is only itself: %v", got)
	}
	if l, ok := d.LinkOf("beacon-user-7"); !ok || l.Source != LinkAdmin {
		t.Errorf("the admin's link: %+v", l)
	}
	if _, ok := d.LinkOf("pat@example.com"); ok {
		t.Error("a person's own email is not a link")
	}
}

// A reader follows one person as one ref, whichever id a product posted.
func TestOnePersonIsOneRef(t *testing.T) {
	m := NewMasker([]byte("key"))
	before := m.Ref("pat@example.com")
	m.SetDirectory(directory())
	ref := m.Ref(oid)
	for _, id := range []string{"pat@example.com", "beacon-user-7"} {
		if m.Ref(id) != ref {
			t.Errorf("%s has its own ref", id)
		}
	}
	if before == ref {
		t.Error("the email's ref was its own before the directory knew it")
	}
	if m.Ref(oid) != m.Filed(oid) {
		t.Error("a person posted by their directory id keeps the ref they had")
	}
	if m.Shown(m.Filed("beacon-user-7")) != ref || m.Shown("agent-1") != "agent-1" {
		t.Error("a filed ref shows as the person's; anything else as itself")
	}
	if m.Ref("stranger@example.com") != m.Filed("stranger@example.com") {
		t.Error("an id the directory does not know keeps its own ref")
	}
}

func TestAFilterMatchesEveryIDOfThePerson(t *testing.T) {
	m := NewMasker([]byte("key"))
	m.SetDirectory(directory())
	refs, ids := m.Match(m.Ref(oid), false)
	if !slices.Equal(refs, []string{m.Ref(oid)}) || !slices.Contains(ids, "beacon-user-7") || !slices.Contains(ids, "pat@example.com") {
		t.Errorf("a reader's ref matches the person's ids: %v %v", refs, ids)
	}
	if _, ids := m.Match("pat@example.com", false); len(ids) != 0 {
		t.Errorf("a reader cannot find a person by email: %v", ids)
	}
	if _, ids := m.Match("pat@example.com", true); !slices.Contains(ids, oid) || !slices.Contains(ids, "beacon-user-7") {
		t.Errorf("a PII reader finds the person by any email: %v", ids)
	}
}

func TestAPIIReaderSeesTheDirectorysName(t *testing.T) {
	m := NewMasker([]byte("key"))
	m.SetDirectory(directory())
	ev := annals.Event{Actor: annals.Actor{Kind: "person", ID: "beacon-user-7"}, Parties: []annals.Party{{Role: "recipient", Kind: "person", ID: "pat.alias@example.com"}}}
	got := m.For(PII, ev)
	if got.Actor.Name != "Pat Example" || got.Actor.Email != "pat@example.com" || got.Actor.ID != "beacon-user-7" {
		t.Errorf("the actor is named and keeps the id it was posted with: %+v", got.Actor)
	}
	if got.Parties[0].Name != "Pat Example" {
		t.Errorf("a party is named: %+v", got.Parties[0])
	}
	if ev.Actor.Name != "" {
		t.Error("the record itself is left alone")
	}
	if r := m.For(Reader, ev); r.Actor.ID != m.Ref(oid) || r.Actor.Name != "" || r.Parties[0].ID != m.Ref(oid) {
		t.Errorf("a reader sees the person's ref and no name: %+v %+v", r.Actor, r.Parties[0])
	}
}
