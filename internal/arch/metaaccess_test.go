package arch_test

import (
	"testing"
)

// TestCLINeverOpensMeta proves the CLI never opens a connection to meta (or the
// data database): the cli package and its subpackages import neither store (the
// meta client) nor pg (the data client), so the only route the CLI has to a state
// change is over the control connection to the leader daemon (specification section
// 2: "CLI never opens meta; every state change via leader"). The daemon, not the
// CLI, holds the database clients; the CLI speaks HTTP/JSON to it. Synthetic graphs
// plant the forbidden edges, then the real repo must be clean.
//
// spec: S02/cli-writes-via-leader
func TestCLINeverOpensMeta(t *testing.T) {
	t.Run("S02/cli-writes-via-leader", func(t *testing.T) {
		t.Run("cli importing the meta or data client is a violation", func(t *testing.T) {
			for _, dep := range []string{"store", "pg", "store/storetest"} {
				t.Run(dep, func(t *testing.T) {
					g := synthGraph(pkg("cli", internalImport(dep)))
					vs := g.CheckCLINoMetaAccess()
					if len(vs) != 1 {
						t.Fatalf("cli importing %q = %d violations, want 1: %v", dep, len(vs), vs)
					}
					if vs[0].Subject != "cli" {
						t.Errorf("violation subject = %q, want cli", vs[0].Subject)
					}
				})
			}
		})

		t.Run("a cli subpackage opening meta is also a violation", func(t *testing.T) {
			g := synthGraph(pkg("cli/render", internalImport("pg")))
			if vs := g.CheckCLINoMetaAccess(); len(vs) != 1 {
				t.Errorf("cli subpackage importing pg = %d violations, want 1: %v", len(vs), vs)
			}
		})

		t.Run("cli reaching meta through the daemon is fine", func(t *testing.T) {
			// The CLI dials the daemon (the leader) over the control connection; the
			// daemon holds the database clients. That indirection is exactly the design.
			g := synthGraph(
				pkg("cli", internalImport("daemon"), internalImport("config")),
				pkg("daemon", internalImport("store"), internalImport("pg")),
			)
			if vs := g.CheckCLINoMetaAccess(); len(vs) != 0 {
				t.Errorf("cli->daemon->store flagged, but the CLI reaches meta only via the leader: %v", vs)
			}
		})

		t.Run("the real repo's CLI opens no meta or data connection", func(t *testing.T) {
			g := loadRealGraph(t)
			if vs := g.CheckCLINoMetaAccess(); len(vs) != 0 {
				t.Errorf("the CLI imports a database client (it must reach meta only via the leader): %v", vs)
			}
		})
	})
}
