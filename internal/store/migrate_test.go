package store

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/ClickHouse/clickhouse-go/v2"
)

// A records table written before change events gains their columns in
// place, and its records read as they did: a cause from the same source.
func TestMigrateKeepsOlderRecords(t *testing.T) {
	base := strings.TrimRight(os.Getenv("ANNALS_TEST_CLICKHOUSE"), "/")
	if base == "" {
		t.Fatal("ANNALS_TEST_CLICKHOUSE is not set; run make check, which starts a test ClickHouse")
	}
	ctx := context.Background()
	b := make([]byte, 6)
	rand.Read(b)
	db := "t_" + hex.EncodeToString(b)
	opts, err := clickhouse.ParseDSN(base)
	if err != nil {
		t.Fatal(err)
	}
	conn, err := clickhouse.Open(opts)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	t.Cleanup(func() { _ = conn.Exec(ctx, "DROP DATABASE IF EXISTS "+db) })
	for _, stmt := range []string{"CREATE DATABASE " + db, strings.Replace(schema[0], "records", db+".records", 1)} {
		if err := conn.Exec(ctx, stmt); err != nil {
			t.Fatal(err)
		}
	}
	body := `{"id":"01JBX7Y8Z9ABCDEFGHJKMNPQRS","at":"2026-10-31T14:00:00Z","source":"ion","event_key":"k2","actor":{"kind":"agent","id":"a"},"action":{"verb":"tool.call","target":{"system":"workstation","kind":"dir","id":"/x"},"outcome":"ok"},"cause":"k1"}`
	at := time.Date(2026, 10, 31, 14, 0, 0, 0, time.UTC)
	if err := conn.Exec(ctx, `INSERT INTO `+db+`.records (id, at, received_at, source, event_key, actor_kind, actor_id, verb, outcome, cause, body) VALUES (?, ?, ?, 'ion', 'k2', 'agent', 'a', 'tool.call', 'ok', 'k1', ?)`,
		"01JBX7Y8Z9ABCDEFGHJKMNPQRS", at, at, body); err != nil {
		t.Fatal(err)
	}

	s, err := Open(ctx, base+"/"+db, "")
	if err != nil {
		t.Fatalf("migrate a live table: %v", err)
	}
	defer s.Close()
	page, err := s.Query(ctx, Filter{Cause: "k1", CauseSource: "ion"})
	if err != nil {
		t.Fatal(err)
	}
	if len(page.Events) != 1 || page.Events[0].EventKey != "k2" || page.Events[0].CauseSource != "" {
		t.Errorf("an older record's cause is from its own source: %+v", page.Events)
	}
	page, err = s.Query(ctx, Filter{Source: "ion", Cause: "k1"})
	if err != nil || len(page.Events) != 1 {
		t.Errorf("an older read reads the same: %v %+v", err, page.Events)
	}
	var rows uint64
	if err := conn.QueryRow(ctx, `SELECT count() FROM system.mutations WHERE database = ? AND table = 'records'`, db).Scan(&rows); err != nil {
		t.Fatal(err)
	}
	if rows != 0 {
		t.Errorf("the migration rewrote parts: %d mutations", rows)
	}
	if _, err := Open(ctx, base+"/"+db, ""); err != nil {
		t.Errorf("a second start migrates nothing and fails nothing: %v", err)
	}
}
