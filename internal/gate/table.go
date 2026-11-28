package gate

import "time"

// Column heads one column of a table: its id, and how its values read.
type Column struct {
	ID       string `json:"id"`
	Title    string `json:"title,omitempty"`
	Unit     string `json:"unit,omitempty"`
	Decimals *int   `json:"decimals,omitempty"`
}

// Table is a score as one table: its columns, then one row a list of
// values in their order. A value that cannot be said is null.
type Table struct {
	Columns []Column `json:"columns"`
	Rows    [][]any  `json:"rows"`
}

func places(n int) *int { return &n }

var familyColumns = []Column{
	{ID: "week", Title: "Week"},
	{ID: "through", Title: "Through"},
	{ID: "family", Title: "Family"},
	{ID: "score", Title: "Score"},
	{ID: "changes", Title: "Changes"},
	{ID: "asks", Title: "Asks"},
	{ID: "ran", Title: "Ran"},
	{ID: "needless", Title: "Needless asks"},
	{ID: "misses", Title: "Misses"},
	{ID: "unknowns", Title: "Unknowns"},
	{ID: "coverage", Title: "Coverage", Unit: "%", Decimals: places(1)},
	{ID: "wait_median", Title: "Median wait", Unit: "min", Decimals: places(1)},
	{ID: "wait_hours", Title: "Wait", Unit: "h", Decimals: places(1)},
	{ID: "waited", Title: "Asks answered"},
}

var draftColumns = []Column{
	{ID: "week", Title: "Week"},
	{ID: "through", Title: "Through"},
	{ID: "pack", Title: "Draft pack"},
	{ID: "judged", Title: "Judged"},
	{ID: "asks", Title: "Asks"},
	{ID: "to_no_one", Title: "Would send to no one"},
	{ID: "harmed", Title: "Of those, did harm"},
	{ID: "unseen", Title: "Of those, unseen"},
	{ID: "coverage", Title: "Coverage", Unit: "%", Decimals: places(1)},
}

func day(t time.Time) string { return t.UTC().Format(time.RFC3339) }

func ptr[T any](p *T) any {
	if p == nil {
		return nil
	}
	return *p
}

// FamilyTable is the families' numbers, newest week first.
func FamilyTable(rows []Family) Table {
	t := Table{Columns: familyColumns, Rows: [][]any{}}
	for _, r := range rows {
		t.Rows = append(t.Rows, []any{
			day(r.Week), day(r.Through), r.Family, r.Score, r.Changes, r.Asks, r.Ran,
			ptr(r.Needless), ptr(r.Misses), r.Unknowns, ptr(r.Coverage), ptr(r.WaitMedian), r.WaitHours, r.Waited,
		})
	}
	return t
}

// DraftTable is the draft packs' replays, newest week first.
func DraftTable(rows []Draft) Table {
	t := Table{Columns: draftColumns, Rows: [][]any{}}
	for _, r := range rows {
		t.Rows = append(t.Rows, []any{
			day(r.Week), day(r.Through), r.Pack, r.Judged, r.Asks, r.ToNoOne, r.Harmed, r.Unseen, ptr(r.Coverage),
		})
	}
	return t
}
