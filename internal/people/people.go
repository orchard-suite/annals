// Package people decides what a caller sees of the people in a record.
//
// A record names people in its person fields: the actor when it is a
// person, the person an agent or system acted for, who granted a credential
// and who approved, every party of kind person, and a target of kind
// person. A reader sees each of those as a stable stand-in (a ref such as
// p-3f9a1c2b7e4d) with names and emails removed, so a reader can still
// follow one person through the board without learning who they are. A PII
// reader and an admin see the person. The directory makes one person one
// ref however many ids products posted them under.
package people

import (
	"cmp"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"slices"
	"strings"
	"sync/atomic"

	"github.com/orchard-suite/platformkit/v3/annals"
)

// Level is how much of a person a caller may see.
type Level int

const (
	// Reader sees what happened, with every person as a ref.
	Reader Level = iota
	// PII sees people: ids, names, and emails.
	PII
	// Admin sees everything and administers tokens.
	Admin
)

// SeesPeople reports whether the level shows people as they are.
func (l Level) SeesPeople() bool { return l >= PII }

func (l Level) String() string {
	switch l {
	case PII:
		return "pii"
	case Admin:
		return "admin"
	default:
		return "reader"
	}
}

// TokenLevels are the read levels a token may carry. Tokens never
// administer, so admin is not one of them.
var TokenLevels = []string{"reader", "pii"}

// ParseTokenLevel reads a token's read level.
func ParseTokenLevel(s string) (Level, error) {
	switch s {
	case "", "reader":
		return Reader, nil
	case "pii":
		return PII, nil
	}
	return Reader, fmt.Errorf("a token reads as reader or pii, not %q", s)
}

// refPrefix starts every ref, so one never passes for a real id.
const refPrefix = "p-"

// Masker turns person ids into refs with a key Annals keeps, so the same
// person is the same ref on every page and across restarts. It reads the
// directory, so one person posted under several ids is one ref.
//
// Two refs exist for an id. The filed ref is the id's own, written beside
// it when the record is stored and never changed. The shown ref is the
// person's: the filed ref of their directory id, or of the id itself when
// the directory does not know it. A reader only ever sees shown refs.
type Masker struct {
	key  []byte
	book *atomic.Pointer[book]
}

// book is the directory and the refs it implies.
type book struct {
	dir    *Directory
	person map[string]string // shown ref -> person id
	shown  map[string]string // filed ref of each id the directory knows -> shown ref
}

func NewMasker(key []byte) Masker {
	m := Masker{key: key, book: new(atomic.Pointer[book])}
	m.SetDirectory(nil)
	return m
}

// SetDirectory makes d the directory every later read matches people with.
func (m Masker) SetDirectory(d *Directory) {
	b := &book{dir: d, person: map[string]string{}, shown: map[string]string{}}
	if d != nil {
		d.each(func(id, person string) {
			ref := m.Filed(person)
			b.person[ref] = person
			b.shown[m.Filed(id)] = ref
		})
	}
	m.book.Store(b)
}

// Directory is the directory reads match people with.
func (m Masker) Directory() *Directory { return m.book.Load().dir }

// Filed is the ref of one id as it was posted, the one stored beside it.
// An empty id stays empty.
func (m Masker) Filed(id string) string {
	if id == "" {
		return ""
	}
	h := hmac.New(sha256.New, m.key)
	h.Write([]byte(id))
	return refPrefix + hex.EncodeToString(h.Sum(nil))[:12]
}

// Ref is the ref a reader sees for id: the person's.
func (m Masker) Ref(id string) string {
	if id == "" {
		return ""
	}
	return m.Filed(m.Directory().Owner(id))
}

// Shown turns a filed ref into the ref a reader sees. A value that is not a
// filed ref of a known id, such as an agent's id, is returned as it is.
func (m Masker) Shown(filed string) string {
	if ref, ok := m.book.Load().shown[filed]; ok {
		return ref
	}
	return filed
}

// Match is what a person filter value v stands for: the filed refs and the
// ids to look for. A shown ref matches every id of its person. A caller who
// sees people may also name a person by any of their ids or emails; one who
// does not gets no ids for an id, so a person cannot be found that way.
func (m Masker) Match(v string, seesPeople bool) (refs, ids []string) {
	b := m.book.Load()
	refs = []string{v}
	if person, ok := b.person[v]; ok {
		ids = b.dir.IDs(person)
	}
	if seesPeople {
		ids = append(ids, v, Key(v))
		if b.dir != nil {
			if _, ok := b.dir.Find(v); ok {
				ids = append(ids, b.dir.IDs(v)...)
			}
		}
	}
	slices.Sort(ids)
	return refs, slices.Compact(ids)
}

// IsRef reports whether v is a ref rather than an id.
func IsRef(v string) bool { return strings.HasPrefix(v, refPrefix) && len(v) == len(refPrefix)+12 }

// ActorRef is how the actor is filed: its filed ref when it is a person,
// its id otherwise.
func (m Masker) ActorRef(a annals.Actor) string {
	if a.Kind == annals.PersonKind {
		return m.Filed(a.ID)
	}
	return a.ID
}

// TargetRef is how the target is filed.
func (m Masker) TargetRef(t annals.Target) string {
	if t.Kind == annals.PersonKind {
		return m.Filed(t.ID)
	}
	return t.ID
}

// PartyRef is how a party is filed.
func (m Masker) PartyRef(p annals.Party) string {
	if p.Kind == annals.PersonKind {
		return m.Filed(p.ID)
	}
	return p.ID
}

// For returns the record as a caller at level may see it.
func (m Masker) For(level Level, ev annals.Event) annals.Event {
	if level.SeesPeople() {
		return m.Name(ev)
	}
	return m.Mask(ev)
}

// Name returns a copy of ev with each person the directory knows given the
// name and email it has for them, where the record has none.
func (m Masker) Name(ev annals.Event) annals.Event {
	dir := m.Directory()
	if dir == nil {
		return ev
	}
	if ev.Actor.Kind == annals.PersonKind {
		if p, ok := dir.Find(ev.Actor.ID); ok {
			ev.Actor.Name = cmp.Or(ev.Actor.Name, p.Name)
			ev.Actor.Email = cmp.Or(ev.Actor.Email, p.Email)
		}
	}
	if len(ev.Parties) > 0 {
		parties := make([]annals.Party, len(ev.Parties))
		for i, pt := range ev.Parties {
			if pt.Kind == annals.PersonKind {
				if p, ok := dir.Find(cmp.Or(pt.ID, pt.Email)); ok {
					pt.Name = cmp.Or(pt.Name, p.Name)
					pt.Email = cmp.Or(pt.Email, p.Email)
				}
			}
			parties[i] = pt
		}
		ev.Parties = parties
	}
	return ev
}

// Mask returns a copy of ev with every person field replaced by its ref and
// every name and email removed. ev itself is left alone.
func (m Masker) Mask(ev annals.Event) annals.Event {
	if ev.Actor.Kind == annals.PersonKind {
		ev.Actor.ID = m.Ref(ev.Actor.ID)
		ev.Actor.Name = ""
	}
	ev.Actor.Email = ""
	ev.Actor.OnBehalfOf = m.Ref(ev.Actor.OnBehalfOf)
	if ev.Authority != nil {
		a := *ev.Authority
		a.GrantedBy = m.Ref(a.GrantedBy)
		if a.Approval != nil {
			ap := *a.Approval
			ap.By = m.Ref(ap.By)
			a.Approval = &ap
		}
		ev.Authority = &a
	}
	if ev.Action.Target.Kind == annals.PersonKind {
		ev.Action.Target.ID = m.Ref(ev.Action.Target.ID)
		ev.Action.Target.Title = ""
	}
	if len(ev.Parties) > 0 {
		parties := make([]annals.Party, len(ev.Parties))
		for i, p := range ev.Parties {
			if p.Kind == annals.PersonKind {
				p.ID = m.Ref(p.ID)
				p.Name = ""
			}
			p.Email = ""
			parties[i] = p
		}
		ev.Parties = parties
	}
	return ev
}
