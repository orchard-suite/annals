package people

import (
	"testing"
	"time"

	"github.com/orchard-suite/platformkit/v3/annals"
)

func share() annals.Event {
	return annals.Event{
		At: time.Now(), Source: "m365",
		Actor:     annals.Actor{Kind: "person", ID: "user-1", Name: "Owner One", Email: "owner@example.com"},
		Authority: &annals.Authority{GrantedBy: "user-9", Approval: &annals.Approval{Required: true, By: "user-8"}},
		Action:    annals.Action{Verb: "site.share", Target: annals.Target{System: "sharepoint", Kind: "site", ID: "s1", Title: "Finance"}, Outcome: "ok"},
		Parties: []annals.Party{
			{Role: "recipient", Kind: "person", ID: "guest-1", Email: "guest@example.org", Name: "Guest One", External: true},
			{Role: "recipient", Kind: "group", ID: "g1", Name: "Finance team"},
		},
		Items: []annals.Item{{System: "sharepoint", Kind: "file", ID: "f1", Title: "budget.xlsx"}},
	}
}

func TestReaderSeesRefsNotPeople(t *testing.T) {
	m := NewMasker([]byte("key"))
	ev := share()
	got := m.For(Reader, ev)
	if got.Actor.ID != m.Ref("user-1") || got.Actor.Name != "" || got.Actor.Email != "" {
		t.Errorf("actor not masked: %+v", got.Actor)
	}
	if got.Authority.GrantedBy != m.Ref("user-9") || got.Authority.Approval.By != m.Ref("user-8") {
		t.Errorf("authority not masked: %+v %+v", got.Authority, got.Authority.Approval)
	}
	p := got.Parties[0]
	if p.ID != m.Ref("guest-1") || p.Email != "" || p.Name != "" || !p.External {
		t.Errorf("person party not masked: %+v", p)
	}
	if g := got.Parties[1]; g.ID != "g1" || g.Name != "Finance team" {
		t.Errorf("a group is not a person and stays: %+v", g)
	}
	if got.Action.Target.Title != "Finance" || got.Items[0].Title != "budget.xlsx" {
		t.Error("what was acted on stays visible")
	}
	if ev.Actor.Email != "owner@example.com" || ev.Parties[0].Email != "guest@example.org" || ev.Authority.Approval.By != "user-8" {
		t.Error("masking changed the original record")
	}
}

func TestPIIReaderSeesPeople(t *testing.T) {
	m := NewMasker([]byte("key"))
	got := m.For(PII, share())
	if got.Actor.Email != "owner@example.com" || got.Parties[0].Email != "guest@example.org" {
		t.Errorf("a PII reader sees people: %+v %+v", got.Actor, got.Parties[0])
	}
	if !Admin.SeesPeople() {
		t.Error("an admin sees people")
	}
}

func TestAgentStaysVisibleButNotWhoItActedFor(t *testing.T) {
	m := NewMasker([]byte("key"))
	ev := share()
	ev.Actor = annals.Actor{Kind: "agent", ID: "m365-copilot", Name: "Copilot", OnBehalfOf: "user-1"}
	got := m.For(Reader, ev)
	if got.Actor.ID != "m365-copilot" || got.Actor.Name != "Copilot" {
		t.Errorf("an agent is not a person: %+v", got.Actor)
	}
	if got.Actor.OnBehalfOf != m.Ref("user-1") {
		t.Errorf("the person it acted for is masked: %q", got.Actor.OnBehalfOf)
	}
}

func TestPersonTargetIsMasked(t *testing.T) {
	m := NewMasker([]byte("key"))
	ev := share()
	ev.Action.Target = annals.Target{System: "entra", Kind: "person", ID: "user-5", Title: "Five Person"}
	got := m.For(Reader, ev)
	if got.Action.Target.ID != m.Ref("user-5") || got.Action.Target.Title != "" {
		t.Errorf("a person target is masked: %+v", got.Action.Target)
	}
}

func TestRefIsStableAndKeyed(t *testing.T) {
	a, b := NewMasker([]byte("one")), NewMasker([]byte("two"))
	if a.Ref("x") != a.Ref("x") {
		t.Error("the same id gives the same ref")
	}
	if a.Ref("x") == b.Ref("x") {
		t.Error("a different key gives a different ref")
	}
	if !IsRef(a.Ref("x")) || IsRef("x") || a.Ref("") != "" {
		t.Error("refs are recognizable and an empty id stays empty")
	}
}
