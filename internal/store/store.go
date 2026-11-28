// Package store keeps records and tokens in ClickHouse. Records are
// append-only rows: a row is written once and deleted only by retention.
//
// ClickHouse has no unique keys, so the store keeps "the same source and
// event_key is stored once" itself: it is the only writer, it claims each
// key in memory while the key is being written, and it looks the key up
// before it writes.
package store

import (
	"cmp"
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/ClickHouse/clickhouse-go/v2"
	"github.com/ClickHouse/clickhouse-go/v2/lib/driver"
	"github.com/oklog/ulid/v2"

	"github.com/orchard-suite/platformkit/v3/annals"

	"github.com/orchard-suite/annals/internal/people"
)

var ErrNotFound = errors.New("not found")

type Store struct {
	conn   driver.Conn
	db     string
	masker people.Masker

	mu       sync.Mutex
	entropy  *ulid.MonotonicEntropy
	inflight map[string]chan struct{}

	tokMu  sync.RWMutex
	tokens map[string]Token // live tokens by secret hash
}

var dbNameRe = regexp.MustCompile(`^[a-z][a-z0-9_]{0,62}$`)

// Open connects to the ClickHouse at dsn, such as
// clickhouse://annals@clickhouse:9000/annals, creates the database and its
// tables when they are missing, and loads the live tokens. password, when
// set, overrides the one in dsn so it can come from a secret.
func Open(ctx context.Context, dsn, password string) (*Store, error) {
	opts, err := clickhouse.ParseDSN(dsn)
	if err != nil {
		return nil, fmt.Errorf("CLICKHOUSE_URL: %w", err)
	}
	if password != "" {
		opts.Auth.Password = password
	}
	db := opts.Auth.Database
	if db == "" || db == "default" {
		db = "annals"
	}
	if !dbNameRe.MatchString(db) {
		return nil, fmt.Errorf("CLICKHOUSE_URL: database %q must be a lowercase name", db)
	}
	opts.Auth.Database = "default"
	boot, err := clickhouse.Open(opts)
	if err != nil {
		return nil, err
	}
	err = boot.Exec(ctx, "CREATE DATABASE IF NOT EXISTS "+db)
	boot.Close()
	if err != nil {
		return nil, fmt.Errorf("create database: %w", err)
	}
	opts.Auth.Database = db
	conn, err := clickhouse.Open(opts)
	if err != nil {
		return nil, err
	}
	s := &Store{conn: conn, db: db, entropy: ulid.Monotonic(rand.Reader, 0), inflight: map[string]chan struct{}{}, tokens: map[string]Token{}}
	if err := s.migrate(ctx); err != nil {
		conn.Close()
		return nil, err
	}
	return s, nil
}

func (s *Store) Close() error { return s.conn.Close() }

// Masker turns person ids into the refs readers see.
func (s *Store) Masker() people.Masker { return s.masker }

func (s *Store) migrate(ctx context.Context) error {
	for _, stmt := range slices.Concat(schema, directorySchema, changeSchema) {
		if err := s.conn.Exec(ctx, stmt); err != nil {
			return fmt.Errorf("migrate: %w", err)
		}
	}
	key, err := s.setting(ctx, "people_key", func() (string, error) {
		b := make([]byte, 32)
		if _, err := rand.Read(b); err != nil {
			return "", err
		}
		return hex.EncodeToString(b), nil
	})
	if err != nil {
		return err
	}
	raw, err := hex.DecodeString(key)
	if err != nil {
		return fmt.Errorf("people_key: %w", err)
	}
	s.masker = people.NewMasker(raw)
	if _, err := s.LoadDirectory(ctx); err != nil {
		return fmt.Errorf("load the people directory: %w", err)
	}
	return s.loadTokens(ctx)
}

// schema creates every table. A table is only ever added to, never
// rewritten, so each statement is safe to run on every start.
var schema = []string{
	`CREATE TABLE IF NOT EXISTS records (
		id String,
		at DateTime64(3, 'UTC'),
		received_at DateTime64(3, 'UTC'),
		source LowCardinality(String),
		event_key String,
		actor_kind LowCardinality(String),
		actor_id String,
		actor_ref String,
		runtime LowCardinality(String),
		verb LowCardinality(String),
		target_system LowCardinality(String),
		target_kind LowCardinality(String),
		target_id String,
		target_ref String,
		outcome LowCardinality(String),
		approval_required Bool,
		approval_by String,
		approval_ref String,
		trace_id String,
		corrects String,
		cause String,
		party_ids Array(String),
		party_refs Array(String),
		party_emails Array(String),
		body String CODEC(ZSTD(3)),
		INDEX idx_id id TYPE bloom_filter GRANULARITY 4,
		INDEX idx_key event_key TYPE bloom_filter GRANULARITY 4,
		INDEX idx_actor actor_id TYPE bloom_filter GRANULARITY 4,
		INDEX idx_actor_ref actor_ref TYPE bloom_filter GRANULARITY 4,
		INDEX idx_target target_id TYPE bloom_filter GRANULARITY 4,
		INDEX idx_target_ref target_ref TYPE bloom_filter GRANULARITY 4,
		INDEX idx_cause cause TYPE bloom_filter GRANULARITY 4,
		INDEX idx_party_ids party_ids TYPE bloom_filter GRANULARITY 4,
		INDEX idx_party_refs party_refs TYPE bloom_filter GRANULARITY 4,
		INDEX idx_party_emails party_emails TYPE bloom_filter GRANULARITY 4
	) ENGINE = MergeTree
	PARTITION BY toYYYYMM(at)
	ORDER BY (at, id)`,
	`CREATE TABLE IF NOT EXISTS tokens (
		id String,
		name String,
		source String,
		reads LowCardinality(String),
		hash String,
		created DateTime64(3, 'UTC'),
		created_by String,
		last_used Nullable(DateTime64(3, 'UTC')),
		revoked_at Nullable(DateTime64(3, 'UTC')),
		version UInt64
	) ENGINE = ReplacingMergeTree(version)
	ORDER BY id`,
	`CREATE TABLE IF NOT EXISTS settings (
		name String,
		value String,
		version UInt64
	) ENGINE = ReplacingMergeTree(version)
	ORDER BY name`,
}

// changeSchema is what change events need: the source of a record's
// cause, the effects as columns, and proof. Each ALTER adds a column and
// rewrites no part: a part written before it reads the column's default.
// The default of cause_source is the record's own source, which is what a
// cause meant before a cause could name another sender.
//
// proof counts what the watch saw, by effect kind, the subject's
// blueprint, the path, and the sender. proof_feed fills it from each
// change.observed record as it is written, so a read of one key reads one
// row and never the records. A change counts once per key, by the worst
// of its sightings: harm, then blind, then clean. Retention prunes records,
// never proof.
var changeSchema = []string{
	`ALTER TABLE records ADD COLUMN IF NOT EXISTS cause_source LowCardinality(String) DEFAULT if(cause = '', '', source)`,
	`ALTER TABLE records ADD COLUMN IF NOT EXISTS effect_kinds Array(LowCardinality(String))`,
	`ALTER TABLE records ADD COLUMN IF NOT EXISTS effect_blueprints Array(LowCardinality(String))`,
	`ALTER TABLE records ADD COLUMN IF NOT EXISTS effect_paths Array(LowCardinality(String))`,
	`ALTER TABLE records ADD COLUMN IF NOT EXISTS effect_verdicts Array(LowCardinality(String))`,
	`CREATE TABLE IF NOT EXISTS proof (
		kind LowCardinality(String),
		blueprint LowCardinality(String),
		path LowCardinality(String),
		source LowCardinality(String),
		watched SimpleAggregateFunction(sum, UInt64),
		clean SimpleAggregateFunction(sum, UInt64),
		harm SimpleAggregateFunction(sum, UInt64),
		blind SimpleAggregateFunction(sum, UInt64),
		last_seen SimpleAggregateFunction(max, DateTime64(3, 'UTC'))
	) ENGINE = AggregatingMergeTree
	ORDER BY (kind, blueprint, path, source)`,
	`CREATE MATERIALIZED VIEW IF NOT EXISTS proof_feed TO proof AS
	SELECT kind, blueprint, path, source,
		toUInt64(1) AS watched,
		toUInt64(worst = 0) AS clean,
		toUInt64(worst = 2) AS harm,
		toUInt64(worst = 1) AS blind,
		last_seen
	FROM (
		SELECT id, source, kind, blueprint, path, max(at) AS last_seen,
			max(multiIf(verdict = 'harm', 2, verdict = 'blind', 1, 0)) AS worst
		FROM records
		ARRAY JOIN effect_kinds AS kind, effect_blueprints AS blueprint, effect_paths AS path, effect_verdicts AS verdict
		WHERE verb = 'change.observed' AND verdict != ''
		GROUP BY id, source, kind, blueprint, path
	)`,
}

// setting reads a setting, writing make's value first when it is missing.
func (s *Store) setting(ctx context.Context, name string, make func() (string, error)) (string, error) {
	var v string
	err := s.conn.QueryRow(ctx, `SELECT value FROM settings FINAL WHERE name = ?`, name).Scan(&v)
	if err == nil {
		return v, nil
	}
	if !isNoRows(err) {
		return "", err
	}
	if v, err = make(); err != nil {
		return "", err
	}
	if err := s.conn.Exec(ctx, `INSERT INTO settings (name, value, version) VALUES (?, ?, ?)`, name, v, uint64(time.Now().UnixNano())); err != nil {
		return "", err
	}
	return v, nil
}

// Result is what happened to one event in a Put.
type Result struct {
	Event     annals.Event
	Duplicate bool
}

// insertCtx asks ClickHouse to buffer small inserts into one part and to
// answer only once the rows are written, so a key written is a key found.
func insertCtx(ctx context.Context) context.Context {
	return clickhouse.Context(ctx, clickhouse.WithSettings(clickhouse.Settings{
		"async_insert":          1,
		"wait_for_async_insert": 1,
	}))
}

// Put stores validated events and returns one result per event, in order.
// An event whose source already holds its event_key, in the store or
// earlier in the same call, comes back as the stored one with Duplicate
// set, and nothing is written for it.
func (s *Store) Put(ctx context.Context, evs []annals.Event) ([]Result, error) {
	out := make([]Result, len(evs))
	claimed := map[string]bool{}
	firstInCall := map[string]int{}
	var waits []chan struct{}
	s.mu.Lock()
	for i, ev := range evs {
		if ev.EventKey == "" {
			continue
		}
		k := ev.Source + "\x00" + ev.EventKey
		if _, ok := firstInCall[k]; ok {
			continue
		}
		firstInCall[k] = i
		if ch, ok := s.inflight[k]; ok {
			waits = append(waits, ch)
			continue
		}
		s.inflight[k] = make(chan struct{})
		claimed[k] = true
	}
	s.mu.Unlock()
	defer func() {
		s.mu.Lock()
		for k := range claimed {
			close(s.inflight[k])
			delete(s.inflight, k)
		}
		s.mu.Unlock()
	}()
	for _, ch := range waits {
		select {
		case <-ch:
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}

	existing, err := s.byKeys(ctx, firstInCall, evs)
	if err != nil {
		return nil, err
	}
	now := time.Now().UTC().Truncate(time.Millisecond)
	var fresh []annals.Event
	for i, ev := range evs {
		k := ev.Source + "\x00" + ev.EventKey
		if ev.EventKey != "" {
			if stored, ok := existing[k]; ok {
				out[i] = Result{Event: stored, Duplicate: true}
				continue
			}
			if first := firstInCall[k]; first != i {
				out[i] = Result{Duplicate: true} // filled from the first below
				continue
			}
		}
		s.mu.Lock()
		ev.ID = ulid.MustNew(ulid.Timestamp(now), s.entropy).String()
		s.mu.Unlock()
		ev.ReceivedAt = now
		ev.At = ev.At.UTC().Truncate(time.Millisecond)
		out[i] = Result{Event: ev}
		fresh = append(fresh, ev)
	}
	if err := s.insert(insertCtx(ctx), fresh); err != nil {
		return nil, err
	}
	for i, ev := range evs {
		if out[i].Duplicate && out[i].Event.ID == "" {
			out[i].Event = out[firstInCall[ev.Source+"\x00"+ev.EventKey]].Event
		}
	}
	return out, nil
}

// byKeys finds the stored records for the keys named in want.
func (s *Store) byKeys(ctx context.Context, want map[string]int, evs []annals.Event) (map[string]annals.Event, error) {
	bySource := map[string][]string{}
	for _, i := range want {
		bySource[evs[i].Source] = append(bySource[evs[i].Source], evs[i].EventKey)
	}
	found := map[string]annals.Event{}
	for source, keys := range bySource {
		rows, err := s.conn.Query(ctx, `SELECT body FROM records WHERE source = ? AND event_key IN ?`, source, keys)
		if err != nil {
			return nil, err
		}
		for rows.Next() {
			var body string
			if err := rows.Scan(&body); err != nil {
				rows.Close()
				return nil, err
			}
			var ev annals.Event
			if err := json.Unmarshal([]byte(body), &ev); err != nil {
				rows.Close()
				return nil, err
			}
			found[ev.Source+"\x00"+ev.EventKey] = ev
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return nil, err
		}
	}
	return found, nil
}

// insert writes records as they are, ids and times included.
func (s *Store) insert(ctx context.Context, evs []annals.Event) error {
	if len(evs) == 0 {
		return nil
	}
	batch, err := s.conn.PrepareBatch(ctx, `INSERT INTO records (
		id, at, received_at, source, event_key,
		actor_kind, actor_id, actor_ref, runtime,
		verb, target_system, target_kind, target_id, target_ref,
		outcome, approval_required, approval_by, approval_ref,
		trace_id, corrects, cause, cause_source,
		party_ids, party_refs, party_emails,
		effect_kinds, effect_blueprints, effect_paths, effect_verdicts,
		body)`)
	if err != nil {
		return err
	}
	for _, ev := range evs {
		body, err := json.Marshal(ev)
		if err != nil {
			batch.Abort()
			return err
		}
		var runtime string
		if ev.Agent != nil {
			runtime = ev.Agent.Runtime
		}
		approvalRequired, approvalBy := false, ""
		if ev.Authority != nil && ev.Authority.Approval != nil {
			approvalRequired = ev.Authority.Approval.Required
			approvalBy = ev.Authority.Approval.By
		}
		partyIDs := make([]string, 0, len(ev.Parties))
		partyRefs := make([]string, 0, len(ev.Parties))
		partyEmails := make([]string, 0, len(ev.Parties))
		for _, p := range ev.Parties {
			if p.ID != "" {
				partyIDs = append(partyIDs, p.ID)
				partyRefs = append(partyRefs, s.masker.PartyRef(p))
			}
			if p.Email != "" {
				partyEmails = append(partyEmails, strings.ToLower(p.Email))
			}
		}
		var causeSource string
		if ev.Cause != "" {
			causeSource = ev.CauseFrom()
		}
		n := len(ev.Effects)
		kinds, blueprints, paths, verdicts := make([]string, n), make([]string, n), make([]string, n), make([]string, n)
		for i, ef := range ev.Effects {
			kinds[i], blueprints[i], paths[i], verdicts[i] = ef.Kind, ef.Subject.Blueprint, ef.Path, ef.Verdict
		}
		err = batch.Append(
			ev.ID, ev.At, ev.ReceivedAt, ev.Source, ev.EventKey,
			ev.Actor.Kind, ev.Actor.ID, s.masker.ActorRef(ev.Actor), runtime,
			ev.Action.Verb, ev.Action.Target.System, ev.Action.Target.Kind, ev.Action.Target.ID, s.masker.TargetRef(ev.Action.Target),
			ev.Action.Outcome, approvalRequired, approvalBy, s.masker.Filed(approvalBy),
			ev.TraceID, ev.Corrects, ev.Cause, causeSource,
			partyIDs, partyRefs, partyEmails,
			kinds, blueprints, paths, verdicts,
			string(body),
		)
		if err != nil {
			batch.Abort()
			return err
		}
	}
	return batch.Send()
}

// ByKey returns the record a source stored under a sender's key.
func (s *Store) ByKey(ctx context.Context, source, key string) (annals.Event, error) {
	return s.one(ctx, `SELECT body FROM records WHERE source = ? AND event_key = ? LIMIT 1`, source, key)
}

// Get returns the record with id.
func (s *Store) Get(ctx context.Context, id string) (annals.Event, error) {
	return s.one(ctx, `SELECT body FROM records WHERE id = ? LIMIT 1`, id)
}

func (s *Store) one(ctx context.Context, query string, args ...any) (annals.Event, error) {
	var body string
	err := s.conn.QueryRow(ctx, query, args...).Scan(&body)
	if isNoRows(err) {
		return annals.Event{}, ErrNotFound
	}
	if err != nil {
		return annals.Event{}, err
	}
	var ev annals.Event
	if err := json.Unmarshal([]byte(body), &ev); err != nil {
		return annals.Event{}, err
	}
	return ev, nil
}

func isNoRows(err error) bool {
	return err != nil && strings.Contains(err.Error(), "no rows")
}

// Filter is every way the API, the board, and the MCP tools narrow records.
type Filter struct {
	Since, Until time.Time
	Actor        string
	Source       string
	System       string
	Outcome      string
	Verb         string
	TargetKind   string
	TargetID     string
	Party        string
	Cause        string
	// CauseSource narrows to records whose cause is from this source,
	// their own when they name none.
	CauseSource      string
	ApprovalRequired bool
	Limit            int
	Cursor           string
	// Masked limits every person filter to refs, for a caller who may not
	// see people, so a person cannot be found by their id or email.
	Masked bool
	// masker matches a person filter to every id of the person; the store
	// sets it.
	masker *people.Masker
}

// match is what a person filter value stands for. Without a masker it is
// the value itself, as a ref and, for a caller who sees people, as an id.
func (f Filter) match(v string) (refs, ids []string) {
	if f.masker != nil {
		return f.masker.Match(v, !f.Masked)
	}
	if f.Masked {
		return []string{v}, nil
	}
	return []string{v}, []string{v, strings.ToLower(v)}
}

const (
	DefaultLimit = 50
	MaxLimit     = 500
)

func ms(t time.Time) int64 { return t.UTC().UnixMilli() }

// where is f's condition, with person filters matched through the
// directory.
func (s *Store) where(f Filter) (string, []any) {
	f.masker = &s.masker
	return f.where()
}

func (f Filter) where() (string, []any) {
	var w []string
	var args []any
	add := func(cond string, vs ...any) { w = append(w, cond); args = append(args, vs...) }
	if !f.Since.IsZero() {
		add("at >= fromUnixTimestamp64Milli(?, 'UTC')", ms(f.Since))
	}
	if !f.Until.IsZero() {
		add("at < fromUnixTimestamp64Milli(?, 'UTC')", ms(f.Until))
	}
	match := func(v string) (refs, ids []string) {
		refs, ids = f.match(v)
		if ids == nil {
			ids = []string{}
		}
		return refs, ids
	}
	if f.Actor != "" {
		refs, ids := match(f.Actor)
		add("(actor_ref IN ? OR actor_id IN ?)", refs, ids)
	}
	if f.Source != "" {
		add("source = ?", f.Source)
	}
	if f.System != "" {
		add("target_system = ?", f.System)
	}
	if f.Outcome != "" {
		add("outcome = ?", f.Outcome)
	}
	if f.Verb != "" {
		add("verb = ?", f.Verb)
	}
	if f.TargetKind != "" {
		add("target_kind = ?", f.TargetKind)
	}
	if f.TargetID != "" {
		refs, ids := match(f.TargetID)
		add("(target_ref IN ? OR target_id IN ?)", refs, ids)
	}
	if f.Party != "" {
		refs, ids := match(f.Party)
		add("(hasAny(party_refs, ?) OR hasAny(party_ids, ?) OR hasAny(party_emails, ?))", refs, ids, ids)
	}
	if f.Cause != "" {
		add("cause = ?", f.Cause)
	}
	if f.CauseSource != "" {
		add("cause_source = ?", f.CauseSource)
	}
	if f.ApprovalRequired {
		w = append(w, "approval_required")
	}
	if f.Cursor != "" {
		if at, id, ok := decodeCursor(f.Cursor); ok {
			add("(at, id) < (fromUnixTimestamp64Milli(?, 'UTC'), ?)", at, id)
		}
	}
	if len(w) == 0 {
		return "", args
	}
	return " WHERE " + strings.Join(w, " AND "), args
}

// Page is one page of records, newest first, and the cursor for the next.
type Page struct {
	Events []annals.Event `json:"events"`
	Next   string         `json:"next,omitempty"`
}

// Query returns records matching f, newest first.
func (s *Store) Query(ctx context.Context, f Filter) (Page, error) {
	limit := f.Limit
	if limit <= 0 {
		limit = DefaultLimit
	}
	if limit > MaxLimit {
		limit = MaxLimit
	}
	where, args := s.where(f)
	args = append(args, limit+1)
	rows, err := s.conn.Query(ctx, `SELECT body FROM records`+where+` ORDER BY at DESC, id DESC LIMIT ?`, args...)
	if err != nil {
		return Page{}, err
	}
	defer rows.Close()
	page := Page{Events: []annals.Event{}}
	for rows.Next() {
		var body string
		if err := rows.Scan(&body); err != nil {
			return Page{}, err
		}
		if len(page.Events) == limit {
			last := page.Events[limit-1]
			page.Next = encodeCursor(ms(last.At), last.ID)
			break
		}
		var ev annals.Event
		if err := json.Unmarshal([]byte(body), &ev); err != nil {
			return Page{}, err
		}
		page.Events = append(page.Events, ev)
	}
	return page, rows.Err()
}

// Count returns how many records match f, ignoring its page settings.
func (s *Store) Count(ctx context.Context, f Filter) (int, error) {
	f.Cursor = ""
	where, args := s.where(f)
	var n uint64
	err := s.conn.QueryRow(ctx, `SELECT count() FROM records`+where, args...).Scan(&n)
	return int(n), err
}

// Bucket is one value of a column and how many records carry it.
type Bucket struct {
	Key string `json:"key"`
	// Label names a person bucket for a caller who sees people.
	Label string `json:"label,omitempty"`
	Count int    `json:"count"`
}

// groupable is every column Breakdown may group by. A person column has a
// ref column a masked filter groups by instead.
var groupable = map[string]string{
	"source": "source", "actor_id": "actor_ref", "actor_kind": "actor_kind", "outcome": "outcome", "verb": "verb",
	"target_system": "target_system", "target_kind": "target_kind", "runtime": "runtime", "approval_by": "approval_ref",
}

// personColumns hold people, so their buckets are merged by person.
var personColumns = map[string]bool{"actor_id": true, "approval_by": true}

// Breakdown counts records matching f by column, largest first. A person
// column counts each person once however many ids they were posted under:
// by their shown ref when masked, by their directory id when not.
func (s *Store) Breakdown(ctx context.Context, f Filter, column string, limit int) ([]Bucket, error) {
	masked, ok := groupable[column]
	if !ok {
		return nil, fmt.Errorf("cannot group by %q", column)
	}
	byPerson := personColumns[column]
	if f.Masked {
		column = masked
	}
	f.Cursor = ""
	where, args := s.where(f)
	query := `SELECT toString(` + column + `) AS k, count() AS n FROM records` + where + ` GROUP BY k ORDER BY n DESC, k`
	if !byPerson {
		query += ` LIMIT ?`
		args = append(args, limit)
	}
	rows, err := s.conn.Query(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	out, err := buckets(rows)
	if err != nil || !byPerson {
		return out, err
	}
	return s.byPerson(out, f.Masked, limit), nil
}

// byPerson merges person buckets into one per person, largest first, and
// names each for a caller who sees people.
func (s *Store) byPerson(in []Bucket, masked bool, limit int) []Bucket {
	dir := s.masker.Directory()
	merged := map[string]*Bucket{}
	var order []*Bucket
	for _, b := range in {
		key, label := b.Key, ""
		if b.Key != "" {
			if masked {
				key = s.masker.Shown(b.Key)
			} else if p, ok := dir.Find(b.Key); ok {
				key, label = p.ID, cmp.Or(p.Name, p.Email)
			}
		}
		if m, ok := merged[key]; ok {
			m.Count += b.Count
			continue
		}
		nb := &Bucket{Key: key, Label: label, Count: b.Count}
		merged[key] = nb
		order = append(order, nb)
	}
	out := make([]Bucket, 0, len(order))
	for _, b := range order {
		out = append(out, *b)
	}
	slices.SortStableFunc(out, func(a, b Bucket) int { return cmp.Or(cmp.Compare(b.Count, a.Count), cmp.Compare(a.Key, b.Key)) })
	if len(out) > limit {
		out = out[:limit]
	}
	return out
}

func buckets(rows driver.Rows) ([]Bucket, error) {
	defer rows.Close()
	out := []Bucket{}
	for rows.Next() {
		var k string
		var n uint64
		if err := rows.Scan(&k, &n); err != nil {
			return nil, err
		}
		out = append(out, Bucket{Key: k, Count: int(n)})
	}
	return out, rows.Err()
}

// Series counts records matching f per hour or day.
func (s *Store) Series(ctx context.Context, f Filter, step time.Duration) ([]Bucket, error) {
	f.Cursor = ""
	where, args := s.where(f)
	bucket := `formatDateTime(toStartOfHour(at), '%Y-%m-%dT%H:00:00Z', 'UTC')`
	if step >= 24*time.Hour {
		bucket = `formatDateTime(toStartOfDay(at), '%Y-%m-%dT00:00:00Z', 'UTC')`
	}
	rows, err := s.conn.Query(ctx, `SELECT `+bucket+` AS k, count() AS n FROM records`+where+` GROUP BY k ORDER BY k`, args...)
	if err != nil {
		return nil, err
	}
	return buckets(rows)
}

// Stats is Annals at a glance.
type Stats struct {
	Events   int       `json:"events"`
	Oldest   time.Time `json:"oldest,omitzero"`
	Newest   time.Time `json:"newest,omitzero"`
	Sources  []Bucket  `json:"sources"`
	Outcomes []Bucket  `json:"outcomes"`
	Actors   int       `json:"actors"`
	Tokens   int       `json:"tokens"`
	DBBytes  int64     `json:"db_bytes"`
}

// actors counts the actors in Annals, each person once however many
// ids they were posted under.
func (s *Store) actors(ctx context.Context) (int, error) {
	rows, err := s.conn.Query(ctx, `SELECT DISTINCT actor_ref FROM records`)
	if err != nil {
		return 0, err
	}
	defer rows.Close()
	seen := map[string]bool{}
	for rows.Next() {
		var ref string
		if err := rows.Scan(&ref); err != nil {
			return 0, err
		}
		seen[s.masker.Shown(ref)] = true
	}
	return len(seen), rows.Err()
}

func (s *Store) Stats(ctx context.Context) (Stats, error) {
	var st Stats
	var n uint64
	var oldest, newest time.Time
	if err := s.conn.QueryRow(ctx, `SELECT count(), min(at), max(at) FROM records`).Scan(&n, &oldest, &newest); err != nil {
		return st, err
	}
	st.Events = int(n)
	if n > 0 {
		st.Oldest, st.Newest = oldest, newest
	}
	var err error
	if st.Actors, err = s.actors(ctx); err != nil {
		return st, err
	}
	if st.Sources, err = s.Breakdown(ctx, Filter{}, "source", 50); err != nil {
		return st, err
	}
	if st.Outcomes, err = s.Breakdown(ctx, Filter{}, "outcome", 3); err != nil {
		return st, err
	}
	st.Tokens = s.liveTokens()
	var bytes uint64
	if err := s.conn.QueryRow(ctx, `SELECT sum(bytes_on_disk) FROM system.parts WHERE database = ? AND active`, s.db).Scan(&bytes); err != nil {
		return st, err
	}
	st.DBBytes = int64(bytes)
	return st, nil
}

// ProofKey names what proof is counted by: an effect kind, the class of
// the subject that felt it, and the path the change took.
type ProofKey struct {
	Kind      string `json:"kind"`
	Blueprint string `json:"blueprint"`
	Path      string `json:"path"`
	// Source, when set, counts only what that sender saw.
	Source string `json:"source,omitempty"`
}

// Proof is how many changes of one key were watched, and what was seen.
type Proof struct {
	ProofKey
	Watched  int       `json:"watched"`
	Clean    int       `json:"clean"`
	Harm     int       `json:"harm"`
	Blind    int       `json:"blind"`
	LastSeen time.Time `json:"last_seen,omitzero"`
}

// Proof reads the counts for one key from proof, never the records.
func (s *Store) Proof(ctx context.Context, k ProofKey) (Proof, error) {
	query := `SELECT sum(watched), sum(clean), sum(harm), sum(blind), max(last_seen) FROM proof WHERE kind = ? AND blueprint = ? AND path = ?`
	args := []any{k.Kind, k.Blueprint, k.Path}
	if k.Source != "" {
		query += ` AND source = ?`
		args = append(args, k.Source)
	}
	var watched, clean, harm, blind uint64
	var last time.Time
	if err := s.conn.QueryRow(ctx, query, args...).Scan(&watched, &clean, &harm, &blind, &last); err != nil {
		return Proof{}, err
	}
	p := Proof{ProofKey: k, Watched: int(watched), Clean: int(clean), Harm: int(harm), Blind: int(blind)}
	if watched > 0 {
		p.LastSeen = last.UTC()
	}
	return p, nil
}

// Prune deletes records from source older than before and returns how many
// went. Source "" means every source.
func (s *Store) Prune(ctx context.Context, source string, before time.Time) (int64, error) {
	cond, args := "at < fromUnixTimestamp64Milli(?, 'UTC')", []any{ms(before)}
	if source != "" {
		cond += " AND source = ?"
		args = append(args, source)
	}
	return s.delete(ctx, cond, args)
}

// PruneExcept deletes records older than before from every source not in
// keep and returns how many went from each source.
func (s *Store) PruneExcept(ctx context.Context, keep []string, before time.Time) (map[string]int64, error) {
	cond, args := "at < fromUnixTimestamp64Milli(?, 'UTC')", []any{ms(before)}
	if len(keep) > 0 {
		cond += " AND source NOT IN ?"
		args = append(args, keep)
	}
	rows, err := s.conn.Query(ctx, `SELECT source, count() FROM records WHERE `+cond+` GROUP BY source`, args...)
	if err != nil {
		return nil, err
	}
	counts, err := buckets(rows)
	if err != nil {
		return nil, err
	}
	out := make(map[string]int64, len(counts))
	for _, b := range counts {
		out[b.Key] = int64(b.Count)
	}
	if len(out) == 0 {
		return out, nil
	}
	if err := s.conn.Exec(ctx, `DELETE FROM records WHERE `+cond, args...); err != nil {
		return nil, err
	}
	return out, nil
}

// delete counts the rows cond matches, then deletes them.
func (s *Store) delete(ctx context.Context, cond string, args []any) (int64, error) {
	var n uint64
	if err := s.conn.QueryRow(ctx, `SELECT count() FROM records WHERE `+cond, args...).Scan(&n); err != nil {
		return 0, err
	}
	if n == 0 {
		return 0, nil
	}
	if err := s.conn.Exec(ctx, `DELETE FROM records WHERE `+cond, args...); err != nil {
		return 0, err
	}
	return int64(n), nil
}

func encodeCursor(atMs int64, id string) string {
	return base64.RawURLEncoding.EncodeToString([]byte(strconv.FormatInt(atMs, 10) + "|" + id))
}

func decodeCursor(c string) (atMs int64, id string, ok bool) {
	b, err := base64.RawURLEncoding.DecodeString(c)
	if err != nil {
		return 0, "", false
	}
	at, id, ok := strings.Cut(string(b), "|")
	if !ok || id == "" {
		return 0, "", false
	}
	n, err := strconv.ParseInt(at, 10, 64)
	return n, id, err == nil
}

// Changes returns every record of the verbs given since a time, oldest
// first, for a reader that joins a change's events itself.
func (s *Store) Changes(ctx context.Context, verbs []string, since time.Time) ([]annals.Event, error) {
	rows, err := s.conn.Query(ctx, `SELECT body FROM records WHERE verb IN ? AND at >= fromUnixTimestamp64Milli(?, 'UTC') ORDER BY at, id`, verbs, ms(since))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []annals.Event
	for rows.Next() {
		var body string
		if err := rows.Scan(&body); err != nil {
			return nil, err
		}
		var ev annals.Event
		if err := json.Unmarshal([]byte(body), &ev); err != nil {
			return nil, err
		}
		out = append(out, ev)
	}
	return out, rows.Err()
}
