package arch_test

import (
	"testing"
)

// TestPgxOnlyNoSQLite proves the rule that all database access uses jackc/pgx
// directly -- never database/sql -- and that the module
// contains no SQLite driver. Statically, that is a module-wide ban on importing
// database/sql (or its subpackages) and on importing any SQLite driver; pgx
// itself is fine. Synthetic graphs plant the banned imports, then the real repo,
// which has neither, must be clean.
func TestPgxOnlyNoSQLite(t *testing.T) {
	t.Run("database/sql is banned anywhere", func(t *testing.T) {
		g := synthGraph(
			pkg("store", "database/sql"),
			pkg("pg", "database/sql/driver"),
		)
		vs := g.CheckPgxOnly()
		if len(vs) != 2 {
			t.Fatalf("CheckPgxOnly = %d violations, want 2 (database/sql + database/sql/driver): %v", len(vs), vs)
		}
		for _, v := range vs {
			if v.Kind != "forbidden-sql-import" {
				t.Errorf("violation kind = %q, want forbidden-sql-import", v.Kind)
			}
		}
	})

	t.Run("SQLite drivers are banned anywhere", func(t *testing.T) {
		for _, driver := range []string{
			"github.com/mattn/go-sqlite3",
			"modernc.org/sqlite",
			"github.com/ncruces/go-sqlite3",
		} {
			t.Run(driver, func(t *testing.T) {
				g := synthGraph(pkg("pg", driver))
				if vs := g.CheckPgxOnly(); len(vs) != 1 {
					t.Errorf("CheckPgxOnly on %q = %d violations, want 1: %v", driver, len(vs), vs)
				}
			})
		}
	})

	t.Run("pgx and ordinary stdlib imports are fine", func(t *testing.T) {
		g := synthGraph(
			pkg("store", pgxImport, "context", "errors"),
			pkg("pg", pgxImport, "context"),
			pkg("dispatch", "context", "fmt"),
		)
		if vs := g.CheckPgxOnly(); len(vs) != 0 {
			t.Errorf("CheckPgxOnly flagged a clean pgx/stdlib graph: %v", vs)
		}
	})

	t.Run("real repo uses no database/sql and no SQLite", func(t *testing.T) {
		g := loadRealGraph(t)
		if vs := g.CheckPgxOnly(); len(vs) != 0 {
			t.Errorf("real module imports database/sql or a SQLite driver: %v", vs)
		}
	})
}
