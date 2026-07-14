package arch_test

import (
	"path/filepath"
	"testing"

	"github.com/MateusAMP2119/iris-engine-cli/internal/arch"
)

// TestImportGraphOneDirection proves the layering as static analysis: the
// internal import graph is acyclic and flows one direction
// (cli -> daemon/api -> dispatch -> store/pg/exec; archive beside dispatch;
// declare/build/pat/buildinfo as leaves), and shipped code never reaches into the
// test-support harness. It plants upward edges, same-rank crossings, cycles, and
// harness dependencies in synthetic graphs, then runs the same three checks over
// the real repo, which must be clean.
func TestImportGraphOneDirection(t *testing.T) {
	t.Run("acyclic graph passes, a cycle is caught", func(t *testing.T) {
		ok := synthGraph(
			pkg("golden", internalImport("spec")),
			pkg("spec"),
		)
		if vs := ok.CheckAcyclic(); len(vs) != 0 {
			t.Errorf("CheckAcyclic on an acyclic graph = %v, want none", vs)
		}
		cyclic := synthGraph(
			pkg("golden", internalImport("roundtrip")),
			pkg("roundtrip", internalImport("golden")),
		)
		if vs := cyclic.CheckAcyclic(); len(vs) == 0 {
			t.Error("CheckAcyclic missed a two-node cycle")
		}
	})

	t.Run("downward edges pass", func(t *testing.T) {
		g := synthGraph(
			pkg("cli", internalImport("daemon"), internalImport("api")),
			pkg("daemon", internalImport("api"), internalImport("dispatch")),
			pkg("api", internalImport("dispatch")),
			pkg("dispatch", internalImport("store"), internalImport("pg"), internalImport("exec")),
			pkg("archive", internalImport("store"), internalImport("pg")),
			pkg("store"),
			pkg("pg"),
			pkg("exec"),
			pkg("declare"),
			pkg("build"),
			pkg("pat"),
			pkg("buildinfo"),
		)
		if vs := g.CheckDirection(); len(vs) != 0 {
			t.Errorf("CheckDirection on a conformant graph = %v, want none", vs)
		}
		if vs := g.CheckAcyclic(); len(vs) != 0 {
			t.Errorf("CheckAcyclic on a conformant graph = %v, want none", vs)
		}
	})

	t.Run("upward edges are caught", func(t *testing.T) {
		cases := map[string]arch.Package{
			"store imports dispatch (upward)": pkg("store", internalImport("dispatch")),
			"api imports daemon (upward)":     pkg("api", internalImport("daemon")),
			"exec imports dispatch (upward)":  pkg("exec", internalImport("dispatch")),
			"declare imports store (upward)":  pkg("declare", internalImport("store")),
		}
		for name, p := range cases {
			t.Run(name, func(t *testing.T) {
				g := synthGraph(p)
				if vs := g.CheckDirection(); len(vs) == 0 {
					t.Errorf("CheckDirection missed the upward edge in %q", name)
				}
			})
		}
	})

	t.Run("illegal same-rank crossings are caught", func(t *testing.T) {
		// store<->pg: two clients, two databases, never crossed. archive<->dispatch:
		// siblings, coordinated from above, neither importing the other.
		cases := map[string]arch.Package{
			"store imports pg":         pkg("store", internalImport("pg")),
			"pg imports store":         pkg("pg", internalImport("store")),
			"archive imports dispatch": pkg("archive", internalImport("dispatch")),
			"dispatch imports archive": pkg("dispatch", internalImport("archive")),
		}
		for name, p := range cases {
			t.Run(name, func(t *testing.T) {
				g := synthGraph(p)
				if vs := g.CheckDirection(); len(vs) == 0 {
					t.Errorf("CheckDirection missed the same-rank crossing in %q", name)
				}
			})
		}
	})

	t.Run("harness isolation: shipped code must not import harness", func(t *testing.T) {
		bad := synthGraph(
			pkg("dispatch", internalImport("trace")),    // product -> harness
			pkg("cmd/iris", internalImport("fixtures")), // main -> harness
			pkg("trace"),
			pkg("fixtures"),
		)
		vs := bad.CheckHarnessIsolation()
		if len(vs) != 2 {
			t.Fatalf("CheckHarnessIsolation = %d violations, want 2: %v", len(vs), vs)
		}

		// The reverse -- a harness fake importing its product seam -- is exactly how
		// the fakes work and must stay clean.
		good := synthGraph(
			pkg("store/storetest", internalImport("store")),
			pkg("pg/pgtest", internalImport("pg")),
			pkg("exec/exectest", internalImport("exec")),
			pkg("store"),
			pkg("pg"),
			pkg("exec"),
		)
		if vs := good.CheckHarnessIsolation(); len(vs) != 0 {
			t.Errorf("CheckHarnessIsolation flagged harness->product imports: %v", vs)
		}
	})

	t.Run("real repo import graph is acyclic, one-direction, harness-isolated", func(t *testing.T) {
		g := loadRealGraph(t)
		if vs := g.CheckAcyclic(); len(vs) != 0 {
			t.Errorf("real internal import graph has a cycle: %v", vs)
		}
		if vs := g.CheckDirection(); len(vs) != 0 {
			t.Errorf("real internal import graph flows the wrong direction: %v", vs)
		}
		if vs := g.CheckHarnessIsolation(); len(vs) != 0 {
			t.Errorf("real shipped code imports harness packages: %v", vs)
		}
	})
}

// loadRealGraph builds the import graph of the repo on disk, resolving the module
// path from the real go.mod so the check follows the module wherever it is
// checked out.
func loadRealGraph(t *testing.T) *arch.Graph {
	t.Helper()
	root := filepath.Join("..", "..")
	gm, err := arch.LoadGoMod(filepath.Join(root, "go.mod"))
	if err != nil {
		t.Fatalf("LoadGoMod: %v", err)
	}
	g, err := arch.LoadGraph(root, gm.Module)
	if err != nil {
		t.Fatalf("LoadGraph: %v", err)
	}
	if len(g.Packages) == 0 {
		t.Fatal("LoadGraph found no packages under the repo root")
	}
	return g
}
