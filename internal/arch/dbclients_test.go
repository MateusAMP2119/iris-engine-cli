package arch_test

import (
	"testing"
)

// TestStorePgSoleDBClients proves the invariant that store (meta) and pg (data)
// are the only database clients: no other shipped package -- archive included --
// may open a third path by importing the pgx driver. archive reuses store and pg;
// it never holds pgx itself. The check is the static proxy for "two clients, two
// databases, never crossed". Synthetic graphs plant a pgx import in packages
// that must not have one, then the real repo (which has no pgx import at all yet)
// must be clean.
func TestStorePgSoleDBClients(t *testing.T) {
	t.Run("store and pg may import pgx", func(t *testing.T) {
		g := synthGraph(
			pkg("store", pgxImport),
			pkg("pg", pgxImport),
		)
		if vs := g.CheckDBClients(); len(vs) != 0 {
			t.Errorf("CheckDBClients flagged the two legitimate clients: %v", vs)
		}
	})

	t.Run("a harness fake may import pgx", func(t *testing.T) {
		// pg/pgtest drives Postgres in tests; the sole-client rule bounds the
		// shipped product, not the scaffolding.
		g := synthGraph(
			pkg("pg/pgtest", pgxImport),
		)
		if vs := g.CheckDBClients(); len(vs) != 0 {
			t.Errorf("CheckDBClients flagged harness pgx imports: %v", vs)
		}
	})

	t.Run("any other shipped package importing pgx is a third path", func(t *testing.T) {
		cases := []string{"archive", "dispatch", "daemon", "api", "cli", "tui", "cmd/iris"}
		for _, rel := range cases {
			t.Run(rel, func(t *testing.T) {
				g := synthGraph(pkg(rel, pgxImport))
				vs := g.CheckDBClients()
				if len(vs) != 1 {
					t.Fatalf("CheckDBClients on %q importing pgx = %d violations, want 1: %v", rel, len(vs), vs)
				}
				if vs[0].Subject != rel {
					t.Errorf("violation subject = %q, want %q", vs[0].Subject, rel)
				}
			})
		}
	})

	t.Run("real repo has no third database client", func(t *testing.T) {
		g := loadRealGraph(t)
		if vs := g.CheckDBClients(); len(vs) != 0 {
			t.Errorf("a package other than store/pg opens a database path: %v", vs)
		}
	})
}
