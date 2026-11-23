package store

import (
	"context"
	"encoding/json"
	"slices"
	"time"

	"github.com/orchard-suite/platformkit/v3/annals"
)

// Pattern is one actor doing one verb to one kind of target, over a window:
// how often, how it went, and when. Folding repeats into patterns is what
// lets a rare record stand out beside a machine that does the same thing
// hundreds of times a day.
type Pattern struct {
	// Key names the pattern the same way in every window, so two windows
	// can be compared.
	Key string
	// Latest is the newest record of the pattern, whole, so the caller masks
	// it like any other record and names the actor and target from it.
	Latest annals.Event
	// LatestBad is the newest record that failed or was refused, when one did.
	LatestBad *annals.Event
	Count     int
	Failed    int
	Refused   int
	// Targets is how many distinct targets the pattern touched.
	Targets int
	First   time.Time
	// Steps counts the records in each step of the window, oldest first.
	Steps []int
}

// patternKey names a pattern by its actor, verb, and target.
const patternKey = `concat(actor_ref, '|', verb, '|', target_system, '|', target_kind)`

// Patterns groups records matching f by actor, verb, and target kind,
// largest first. f.Since must be set; the window from f.Since to until is
// cut into steps of the given length for each pattern's Steps. It reads
// only the bodies of the records it returns whole.
func (s *Store) Patterns(ctx context.Context, f Filter, until time.Time, step time.Duration, limit int) ([]Pattern, error) {
	f.Cursor = ""
	where, args := s.where(f)
	since, stepMS := ms(f.Since), step.Milliseconds()
	n := int((ms(until)-since)/stepMS) + 1
	q := `SELECT ` + patternKey + `, argMax(id, at), argMaxIf(id, at, outcome != 'ok'), count(), countIf(outcome = 'failed'), countIf(outcome = 'refused'),
	uniqExact(target_id), min(at), tupleElement(sumMap([toInt64(intDiv(toUnixTimestamp64Milli(at) - ?, ?))], [toUInt64(1)]) AS h, 1), tupleElement(h, 2)
	FROM records` + where + `
	GROUP BY actor_ref, verb, target_system, target_kind
	ORDER BY count() DESC, max(at) DESC LIMIT ?`
	rows, err := s.conn.Query(ctx, q, append(append([]any{since, stepMS}, args...), limit)...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Pattern{}
	var latest, bad []string
	for rows.Next() {
		var (
			key, id, badID               string
			count, failed, refused, uniq uint64
			first                        time.Time
			keys                         []int64
			values                       []uint64
		)
		if err := rows.Scan(&key, &id, &badID, &count, &failed, &refused, &uniq, &first, &keys, &values); err != nil {
			return nil, err
		}
		p := Pattern{Key: key, Count: int(count), Failed: int(failed), Refused: int(refused), Targets: int(uniq), First: first, Steps: make([]int, n)}
		for i, k := range keys {
			if k >= 0 && int(k) < n {
				p.Steps[k] += int(values[i])
			}
		}
		out = append(out, p)
		latest, bad = append(latest, id), append(bad, badID)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	byID, err := s.bodies(ctx, f, append(slices.Clone(latest), bad...))
	if err != nil {
		return nil, err
	}
	for i := range out {
		out[i].Latest = byID[latest[i]]
		if ev, ok := byID[bad[i]]; ok {
			out[i].LatestBad = &ev
		}
	}
	return out, nil
}

// bodies reads the records matching f with the given ids, by id.
func (s *Store) bodies(ctx context.Context, f Filter, ids []string) (map[string]annals.Event, error) {
	ids = slices.DeleteFunc(ids, func(id string) bool { return id == "" })
	out := make(map[string]annals.Event, len(ids))
	if len(ids) == 0 {
		return out, nil
	}
	where, args := s.where(f)
	if where == "" {
		where = " WHERE id IN ?"
	} else {
		where += " AND id IN ?"
	}
	rows, err := s.conn.Query(ctx, `SELECT body FROM records`+where, append(args, ids)...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var body string
		if err := rows.Scan(&body); err != nil {
			return nil, err
		}
		var ev annals.Event
		if err := json.Unmarshal([]byte(body), &ev); err != nil {
			return nil, err
		}
		out[ev.ID] = ev
	}
	return out, rows.Err()
}

// PatternCounts counts the records matching f in each pattern, by its
// Key, for the largest limit patterns.
func (s *Store) PatternCounts(ctx context.Context, f Filter, limit int) (map[string]int, error) {
	f.Cursor = ""
	where, args := s.where(f)
	rows, err := s.conn.Query(ctx, `SELECT `+patternKey+` AS k, count() AS n FROM records`+where+`
	GROUP BY actor_ref, verb, target_system, target_kind ORDER BY n DESC LIMIT ?`, append(args, limit)...)
	if err != nil {
		return nil, err
	}
	bs, err := buckets(rows)
	if err != nil {
		return nil, err
	}
	out := make(map[string]int, len(bs))
	for _, b := range bs {
		out[b.Key] = b.Count
	}
	return out, nil
}

// Step is one step of a window: how many records, and how many failed or
// were refused.
type Step struct {
	Total   int
	Failed  int
	Refused int
}

// Pulse counts records matching f in each step from f.Since to until,
// oldest first, by outcome. f.Since must be set.
func (s *Store) Pulse(ctx context.Context, f Filter, until time.Time, step time.Duration) ([]Step, error) {
	f.Cursor = ""
	where, args := s.where(f)
	since, stepMS := ms(f.Since), step.Milliseconds()
	n := int((ms(until)-since)/stepMS) + 1
	rows, err := s.conn.Query(ctx, `SELECT toInt64(intDiv(toUnixTimestamp64Milli(at) - ?, ?)) AS k, count(), countIf(outcome = 'failed'), countIf(outcome = 'refused')
	FROM records`+where+` GROUP BY k`, append([]any{since, stepMS}, args...)...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := make([]Step, n)
	for rows.Next() {
		var k int64
		var total, failed, refused uint64
		if err := rows.Scan(&k, &total, &failed, &refused); err != nil {
			return nil, err
		}
		if k >= 0 && int(k) < n {
			out[k] = Step{Total: int(total), Failed: int(failed), Refused: int(refused)}
		}
	}
	return out, rows.Err()
}

// Oldest is when the oldest record matching f happened, or zero when none
// does.
func (s *Store) Oldest(ctx context.Context, f Filter) (time.Time, error) {
	f.Cursor = ""
	where, args := s.where(f)
	var n uint64
	var at time.Time
	if err := s.conn.QueryRow(ctx, `SELECT count(), min(at) FROM records`+where, args...).Scan(&n, &at); err != nil || n == 0 {
		return time.Time{}, err
	}
	return at, nil
}

// CursorAfter is the cursor for the records older than ev, for a caller
// that pages by something other than a fixed count of records.
func CursorAfter(ev annals.Event) string { return encodeCursor(ms(ev.At), ev.ID) }
