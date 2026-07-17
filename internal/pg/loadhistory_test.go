package pg_test

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/MateusAMP2119/iris-lakehouse/internal/pg"
	"github.com/MateusAMP2119/iris-lakehouse/internal/pg/pgtest"
)

// TestEnsureLoadHistory proves the load-history ensure issues exactly the
// create-if-missing DDL -- schema, table, prune index, in that order -- and
// surfaces a failing data database instead of half-ensuring.
func TestEnsureLoadHistory(t *testing.T) {
	t.Run("ensure-load-history", func(t *testing.T) {
		rec := pgtest.New()
		if err := pg.EnsureLoadHistory(context.Background(), rec); err != nil {
			t.Fatalf("EnsureLoadHistory: %v", err)
		}
		stmts := rec.Statements()
		if len(stmts) != 3 {
			t.Fatalf("issued %d statements, want schema + table + index", len(stmts))
		}
		wants := []string{
			"CREATE SCHEMA IF NOT EXISTS iris",
			"CREATE TABLE IF NOT EXISTS " + pg.LoadHistoryName,
			"CREATE INDEX IF NOT EXISTS load_history_bucket",
		}
		for i, want := range wants {
			if !strings.Contains(stmts[i], want) {
				t.Errorf("statement %d = %q, want it to carry %q", i, stmts[i], want)
			}
		}
		// The absence doctrine lives in the schema: load columns are nullable
		// (no NOT NULL), so an unsampled bucket persists as NULL, never zero.
		if table := stmts[1]; strings.Contains(table, "cpu_max double precision NOT NULL") ||
			strings.Contains(table, "rss_max bigint NOT NULL") {
			t.Errorf("load columns must stay nullable for absent buckets:\n%s", table)
		}

		rec.FailWith(errors.New("no data database"))
		if err := pg.EnsureLoadHistory(context.Background(), rec); err == nil {
			t.Fatal("a failing data database must surface, not half-ensure")
		}
	})
}
