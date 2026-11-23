package people

import (
	"slices"
	"strings"
)

// Person is one human the directory knows. ID is their directory object id,
// the one id every product should post for them; Name and Email are what
// the directory calls them. Left is set when the directory no longer has
// them: their records outlive their account, so they stay.
type Person struct {
	ID    string `json:"id"`
	Name  string `json:"name,omitempty"`
	Email string `json:"email,omitempty"`
	Left  bool   `json:"left,omitempty"`
}

// Link ties one id a product posted to a person: an email alias, an old
// product's own user id. Source says who made it: the directory's sync, a
// record that named the person by that id and an email of theirs, or an
// admin on the board.
type Link struct {
	ID     string `json:"id"`
	Person string `json:"person"`
	Source string `json:"source"`
}

// The sources of a link.
const (
	LinkDirectory = "directory"
	LinkPosted    = "posted"
	LinkAdmin     = "admin"
)

// Directory matches every id products post for a person to that person.
// Records keep the ids they were posted with; a read asks the directory who
// each id is, so a link made today also covers last year's records. It is
// built once and never changed; a new one replaces it.
type Directory struct {
	people map[string]Person
	owner  map[string]string   // id -> person id
	ids    map[string][]string // person id -> every id, the person id first
	links  map[string]Link     // id -> the link that made it
}

// Key folds an id for matching: emails and object ids differ only in case.
func Key(id string) string { return strings.ToLower(strings.TrimSpace(id)) }

// NewDirectory builds a directory. A person's own id and email always
// match them; a link to a person the directory does not hold is dropped.
func NewDirectory(ps []Person, links []Link) *Directory {
	d := &Directory{people: map[string]Person{}, owner: map[string]string{}, ids: map[string][]string{}, links: map[string]Link{}}
	add := func(id, person string) {
		id = Key(id)
		if id == "" {
			return
		}
		if _, taken := d.owner[id]; taken {
			return
		}
		d.owner[id] = person
		d.ids[person] = append(d.ids[person], id)
	}
	for _, p := range ps {
		p.ID = Key(p.ID)
		if p.ID == "" {
			continue
		}
		d.people[p.ID] = p
		add(p.ID, p.ID)
	}
	for _, p := range ps {
		add(p.Email, Key(p.ID))
	}
	for _, l := range links {
		person := Key(l.Person)
		if _, ok := d.people[person]; !ok {
			continue
		}
		l.ID, l.Person = Key(l.ID), person
		add(l.ID, person)
		if d.owner[l.ID] == person {
			d.links[l.ID] = l
		}
	}
	return d
}

// Find returns the person id names, by any of their ids.
func (d *Directory) Find(id string) (Person, bool) {
	if d == nil {
		return Person{}, false
	}
	person, ok := d.owner[Key(id)]
	if !ok {
		return Person{}, false
	}
	return d.people[person], true
}

// Owner is the person id an id belongs to, or the id itself when the
// directory does not know it.
func (d *Directory) Owner(id string) string {
	if p, ok := d.Find(id); ok {
		return p.ID
	}
	return id
}

// IDs is every id of the person id names, the person id first; an id the
// directory does not know is only itself.
func (d *Directory) IDs(id string) []string {
	if p, ok := d.Find(id); ok {
		return slices.Clone(d.ids[p.ID])
	}
	return []string{id}
}

// LinkOf is the link that ties id to its person, when one does: the
// person's own id and email are not links.
func (d *Directory) LinkOf(id string) (Link, bool) {
	if d == nil {
		return Link{}, false
	}
	l, ok := d.links[Key(id)]
	return l, ok
}

// People is how many people the directory holds.
func (d *Directory) People() int {
	if d == nil {
		return 0
	}
	return len(d.people)
}

// each calls fn with every id the directory knows and its person.
func (d *Directory) each(fn func(id, person string)) {
	for id, person := range d.owner {
		fn(id, person)
	}
}
