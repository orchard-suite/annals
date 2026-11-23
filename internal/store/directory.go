package store

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"time"

	"github.com/orchard-suite/platformkit/v3/annals"

	"github.com/orchard-suite/annals/internal/people"
)

// directorySchema holds the people directory: who each person is, and every
// other id they were posted under. Both keep the newest row per id.
var directorySchema = []string{
	`CREATE TABLE IF NOT EXISTS people (
		id String,
		name String,
		email String,
		left Bool,
		version UInt64
	) ENGINE = ReplacingMergeTree(version)
	ORDER BY id`,
	`CREATE TABLE IF NOT EXISTS identities (
		id String,
		person String,
		source LowCardinality(String),
		by String,
		removed Bool,
		version UInt64
	) ENGINE = ReplacingMergeTree(version)
	ORDER BY id`,
}

// ErrConflict is a link that would tie a person's own id to someone else.
var ErrConflict = errors.New("conflict")

// LoadDirectory reads the directory and makes every later read match
// people with it.
func (s *Store) LoadDirectory(ctx context.Context) (*people.Directory, error) {
	rows, err := s.conn.Query(ctx, `SELECT id, name, email, left FROM people FINAL`)
	if err != nil {
		return nil, err
	}
	var ps []people.Person
	for rows.Next() {
		var p people.Person
		if err := rows.Scan(&p.ID, &p.Name, &p.Email, &p.Left); err != nil {
			rows.Close()
			return nil, err
		}
		ps = append(ps, p)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}
	rows, err = s.conn.Query(ctx, `SELECT id, person, source FROM identities FINAL WHERE NOT removed`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var links []people.Link
	for rows.Next() {
		var l people.Link
		if err := rows.Scan(&l.ID, &l.Person, &l.Source); err != nil {
			return nil, err
		}
		links = append(links, l)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	d := people.NewDirectory(ps, links)
	s.masker.SetDirectory(d)
	return d, nil
}

// Directory is the directory reads match people with.
func (s *Store) Directory() *people.Directory { return s.masker.Directory() }

// WriteDirectory stores people and links to them, then reloads the
// directory.
func (s *Store) WriteDirectory(ctx context.Context, ps []people.Person, links []people.Link) error {
	version := uint64(time.Now().UnixNano())
	if len(ps) > 0 {
		batch, err := s.conn.PrepareBatch(ctx, `INSERT INTO people (id, name, email, left, version)`)
		if err != nil {
			return err
		}
		for _, p := range ps {
			if err := batch.Append(people.Key(p.ID), p.Name, people.Key(p.Email), p.Left, version); err != nil {
				batch.Abort()
				return err
			}
		}
		if err := batch.Send(); err != nil {
			return err
		}
	}
	if len(links) > 0 {
		batch, err := s.conn.PrepareBatch(ctx, `INSERT INTO identities (id, person, source, by, removed, version)`)
		if err != nil {
			return err
		}
		for _, l := range links {
			if err := batch.Append(people.Key(l.ID), people.Key(l.Person), l.Source, "", false, version); err != nil {
				batch.Abort()
				return err
			}
		}
		if err := batch.Send(); err != nil {
			return err
		}
	}
	_, err := s.LoadDirectory(ctx)
	return err
}

// Link ties id to the person named by person, any id of theirs, as an
// admin did. It refuses a person the directory does not hold, and an id
// that is some person's own id or email.
func (s *Store) Link(ctx context.Context, id, person, by string) (people.Link, people.Person, error) {
	dir := s.Directory()
	p, ok := dir.Find(person)
	if !ok {
		return people.Link{}, people.Person{}, ErrNotFound
	}
	if owner, ok := dir.Find(id); ok {
		if _, linked := dir.LinkOf(id); !linked && owner.ID != p.ID {
			return people.Link{}, people.Person{}, fmt.Errorf("%w: %s is another person's own id", ErrConflict, id)
		}
	}
	l := people.Link{ID: people.Key(id), Person: p.ID, Source: people.LinkAdmin}
	if err := s.conn.Exec(ctx, `INSERT INTO identities (id, person, source, by, removed, version) VALUES (?,?,?,?,?,?)`,
		l.ID, l.Person, l.Source, by, false, uint64(time.Now().UnixNano())); err != nil {
		return people.Link{}, people.Person{}, err
	}
	_, err := s.LoadDirectory(ctx)
	return l, p, err
}

// Unlink removes the link an admin made for id.
func (s *Store) Unlink(ctx context.Context, id, by string) (people.Link, error) {
	l, ok := s.Directory().LinkOf(id)
	if !ok || l.Source != people.LinkAdmin {
		return people.Link{}, ErrNotFound
	}
	if err := s.conn.Exec(ctx, `INSERT INTO identities (id, person, source, by, removed, version) VALUES (?,?,?,?,?,?)`,
		l.ID, "", people.LinkAdmin, by, true, uint64(time.Now().UnixNano())); err != nil {
		return people.Link{}, err
	}
	_, err := s.LoadDirectory(ctx)
	return l, err
}

// Setting reads a setting; a missing one is empty.
func (s *Store) Setting(ctx context.Context, name string) (string, error) {
	var v string
	err := s.conn.QueryRow(ctx, `SELECT value FROM settings FINAL WHERE name = ?`, name).Scan(&v)
	if isNoRows(err) {
		return "", nil
	}
	return v, err
}

// SetSetting writes a setting.
func (s *Store) SetSetting(ctx context.Context, name, value string) error {
	return s.conn.Exec(ctx, `INSERT INTO settings (name, value, version) VALUES (?, ?, ?)`, name, value, uint64(time.Now().UnixNano()))
}

// postedSetting keeps the received_at, in Unix milliseconds, up to which
// LinkPosted has read.
const postedSetting = "posted_links_until"

// LinkPosted ties each person id received since its last call and before
// until, which the directory does not know, to the person the same record
// names by email. A product that posts a person by its own id with their
// email then matches them, on its older records too. It returns how many
// ids it tied.
func (s *Store) LinkPosted(ctx context.Context, until time.Time) (int, error) {
	raw, err := s.Setting(ctx, postedSetting)
	if err != nil {
		return 0, err
	}
	var since int64
	if raw != "" {
		if since, err = strconv.ParseInt(raw, 10, 64); err != nil {
			return 0, fmt.Errorf("%s: %w", postedSetting, err)
		}
	}
	upto := ms(until)
	rows, err := s.conn.Query(ctx, `SELECT actor_id, argMax(lower(JSONExtractString(body, 'actor', 'email')), received_at) AS email
	FROM records
	WHERE actor_kind = ? AND received_at >= fromUnixTimestamp64Milli(?, 'UTC') AND received_at < fromUnixTimestamp64Milli(?, 'UTC')
	GROUP BY actor_id HAVING email != ''`, annals.PersonKind, since, upto)
	if err != nil {
		return 0, err
	}
	dir := s.Directory()
	var links []people.Link
	for rows.Next() {
		var id, email string
		if err := rows.Scan(&id, &email); err != nil {
			rows.Close()
			return 0, err
		}
		if _, known := dir.Find(id); known {
			continue
		}
		if p, ok := dir.Find(email); ok {
			links = append(links, people.Link{ID: id, Person: p.ID, Source: people.LinkPosted})
		}
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return 0, err
	}
	if len(links) > 0 {
		if err := s.WriteDirectory(ctx, nil, links); err != nil {
			return 0, err
		}
	}
	return len(links), s.SetSetting(ctx, postedSetting, strconv.FormatInt(upto, 10))
}
