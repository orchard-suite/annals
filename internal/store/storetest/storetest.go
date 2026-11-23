// Package storetest opens a store on a test ClickHouse, in a database of its
// own that is dropped when the test ends. `make check` starts the
// ClickHouse and sets ANNALS_TEST_CLICKHOUSE.
package storetest

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"os"
	"strings"
	"testing"

	"github.com/ClickHouse/clickhouse-go/v2"

	"github.com/orchard-suite/annals/internal/store"
)

// Env names the test ClickHouse, such as clickhouse://default@localhost:19000.
const Env = "ANNALS_TEST_CLICKHOUSE"

// Open returns a store on a fresh database.
func Open(t testing.TB) *store.Store {
	t.Helper()
	base := strings.TrimRight(os.Getenv(Env), "/")
	if base == "" {
		t.Fatalf("%s is not set; run make check, which starts a test ClickHouse", Env)
	}
	b := make([]byte, 6)
	rand.Read(b)
	name := "t_" + hex.EncodeToString(b)
	ctx := context.Background()
	st, err := store.Open(ctx, base+"/"+name, "")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		st.Close()
		opts, err := clickhouse.ParseDSN(base)
		if err != nil {
			return
		}
		if conn, err := clickhouse.Open(opts); err == nil {
			_ = conn.Exec(ctx, "DROP DATABASE IF EXISTS "+name)
			conn.Close()
		}
	})
	return st
}
