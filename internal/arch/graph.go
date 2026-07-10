package arch

import (
	"fmt"
	"go/parser"
	"go/token"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
)

// This file holds the import-graph model and the four static checks over it
// (specification section 10): acyclicity, one-direction layering, harness
// isolation, and store/pg as the sole database clients, plus the module-wide
// pgx-only / no-database-sql / no-SQLite import ban (specification section 9).

// Class is a package's structural class: Product (a spec section 10 roster
// package), Main (cmd/iris), or Harness (test-support scaffolding).
type Class int

// The package classes.
const (
	// ClassHarness is a test-support / executable-spec package, outside the
	// product graph and carrying no layer rank.
	ClassHarness Class = iota
	// ClassProduct is a specification section 10 roster package, carrying a layer
	// rank the one-direction rule enforces.
	ClassProduct
	// ClassMain is the single main package, cmd/iris, the top of the direction.
	ClassMain
)

// rankMain is the direction rank of the main package: above every product
// package, so cmd/iris may import product packages downward and nothing may
// import it.
const rankMain = 100

// productRanks assigns each specification section 10 product package its layer
// rank. The one-direction rule is: a shipped package may import another shipped
// package only when the target's rank is strictly lower. That single rule
// captures the spec's whole layering at once -- cli -> daemon -> api -> dispatch
// -> store/pg/exec, archive beside dispatch, config/declare/build/pat/buildinfo as
// leaves -- and, because a strictly-lower target is required, it also forbids the
// same-rank crossings the spec bans: store<->pg ("two clients, two databases,
// never crossed") and archive<->dispatch (siblings coordinated from above).
// daemon outranks api because listener wiring (which mounts the api mux) lives in
// the daemon; api renders read views and never reaches back up. config is a leaf
// carrying no dependencies of its own -- pure configuration resolution the CLI
// (and later the daemon) reads. buildinfo is a leaf carrying only the linker-
// stamped build version any layer may read. Later epics add a package by giving it
// a rank here; the checks do not change.
var productRanks = map[string]int{
	"buildinfo": 1,
	"config":    1,
	"declare":   1,
	"build":     1,
	"pat":       1,
	"store":     2,
	"pg":        2,
	"exec":      2,
	"dispatch":  3,
	"archive":   3,
	"api":       4,
	"daemon":    5,
	"cli":       6,
}

// Package is one node in the module import graph: its repo-relative key (the
// subpath under internal/, e.g. "store" or "store/storetest", or "cmd/iris") and
// the full import paths of its non-test Go files.
type Package struct {
	// Rel is the package's repo-relative key.
	Rel string
	// Imports are the full import paths drawn from the package's non-test files.
	Imports []string
}

// Graph is the module's non-test import graph: the module path and one Package
// per package directory under internal/ and cmd/.
type Graph struct {
	// Module is the module path (from go.mod), the prefix that identifies an
	// import as internal.
	Module string
	// Packages are the graph's nodes.
	Packages []Package
}

// classify maps a repo-relative package key to its structural class and, for a
// product package, its short name and layer rank. cmd/... is the main package;
// a single-element key naming a roster package is product; everything else under
// internal/ (nested subpackages and non-roster packages alike) is harness.
func classify(rel string) (class Class, product string, rank int) {
	if rel == "cmd/iris" || strings.HasPrefix(rel, "cmd/") {
		return ClassMain, "", rankMain
	}
	if !strings.Contains(rel, "/") {
		if r, ok := productRanks[rel]; ok {
			return ClassProduct, rel, r
		}
	}
	return ClassHarness, "", 0
}

// relOf maps a full import path to its repo-relative package key, reporting
// whether the import is internal to this module (under internal/ or cmd/).
func (g *Graph) relOf(path string) (rel string, internal bool) {
	if p, ok := strings.CutPrefix(path, g.Module+"/internal/"); ok {
		return p, true
	}
	if p, ok := strings.CutPrefix(path, g.Module+"/cmd/"); ok {
		return "cmd/" + p, true
	}
	return "", false
}

// LoadGraph walks root's internal/ and cmd/ trees, parsing the imports of every
// non-test Go file (testdata, vendor, and hidden directories skipped) into one
// Package per directory, and returns the module import graph. module is the
// module path used to tell internal imports from third-party ones.
func LoadGraph(root, module string) (*Graph, error) {
	byRel := map[string]map[string]bool{}
	var order []string

	collect := func(base string) error {
		return filepath.WalkDir(base, func(path string, d fs.DirEntry, err error) error {
			if err != nil {
				if os.IsNotExist(err) {
					return nil // an absent internal/ or cmd/ tree is not an error.
				}
				return err
			}
			if d.IsDir() {
				name := d.Name()
				if path != base && (strings.HasPrefix(name, ".") || name == "testdata" || name == "vendor") {
					return fs.SkipDir
				}
				return nil
			}
			if !strings.HasSuffix(d.Name(), ".go") || strings.HasSuffix(d.Name(), "_test.go") {
				return nil
			}
			imports, err := parseImports(path)
			if err != nil {
				return err
			}
			rel, err := filepath.Rel(root, filepath.Dir(path))
			if err != nil {
				return err
			}
			key := strings.TrimPrefix(filepath.ToSlash(rel), "internal/")
			if byRel[key] == nil {
				byRel[key] = map[string]bool{}
				order = append(order, key)
			}
			for _, imp := range imports {
				byRel[key][imp] = true
			}
			return nil
		})
	}

	if err := collect(filepath.Join(root, "internal")); err != nil {
		return nil, fmt.Errorf("arch: walk internal: %w", err)
	}
	if err := collect(filepath.Join(root, "cmd")); err != nil {
		return nil, fmt.Errorf("arch: walk cmd: %w", err)
	}

	sort.Strings(order)
	g := &Graph{Module: module}
	for _, key := range order {
		imps := make([]string, 0, len(byRel[key]))
		for imp := range byRel[key] {
			imps = append(imps, imp)
		}
		sort.Strings(imps)
		g.Packages = append(g.Packages, Package{Rel: key, Imports: imps})
	}
	return g, nil
}

// parseImports returns the import paths declared in one Go source file.
func parseImports(path string) ([]string, error) {
	src, err := os.ReadFile(path) //nolint:gosec // G304: path is a repo file under the module root the structural gate walks, never user or network input.
	if err != nil {
		return nil, fmt.Errorf("arch: read %s: %w", path, err)
	}
	f, err := parser.ParseFile(token.NewFileSet(), path, src, parser.ImportsOnly)
	if err != nil {
		return nil, fmt.Errorf("arch: parse %s: %w", path, err)
	}
	out := make([]string, 0, len(f.Imports))
	for _, spec := range f.Imports {
		p, err := strconv.Unquote(spec.Path.Value)
		if err != nil {
			continue
		}
		out = append(out, p)
	}
	return out, nil
}

// sortedPackages returns g's packages ordered by key, for deterministic reports.
func (g *Graph) sortedPackages() []Package {
	out := append([]Package(nil), g.Packages...)
	sort.Slice(out, func(i, j int) bool { return out[i].Rel < out[j].Rel })
	return out
}

// internalImports returns p's imports that resolve to internal packages, as
// repo-relative keys, deduplicated and sorted.
func (g *Graph) internalImports(p Package) []string {
	seen := map[string]bool{}
	var out []string
	for _, imp := range p.Imports {
		if rel, ok := g.relOf(imp); ok && !seen[rel] {
			seen[rel] = true
			out = append(out, rel)
		}
	}
	sort.Strings(out)
	return out
}

// CheckAcyclic returns a violation for each cycle in the internal import graph
// (every class included; the ban on cycles is module-wide).
func (g *Graph) CheckAcyclic() []Violation {
	adj := map[string][]string{}
	nodeSet := map[string]bool{}
	for _, p := range g.Packages {
		nodeSet[p.Rel] = true
		for _, rel := range g.internalImports(p) {
			adj[p.Rel] = append(adj[p.Rel], rel)
			nodeSet[rel] = true
		}
	}
	nodes := make([]string, 0, len(nodeSet))
	for n := range nodeSet {
		nodes = append(nodes, n)
	}
	sort.Strings(nodes)

	const (
		white = 0
		gray  = 1
		black = 2
	)
	color := make(map[string]int, len(nodes))
	reported := map[string]bool{}
	var vs []Violation
	var stack []string

	var visit func(u string)
	visit = func(u string) {
		color[u] = gray
		stack = append(stack, u)
		for _, v := range adj[u] {
			switch color[v] {
			case white:
				visit(v)
			case gray:
				cyc := cycleFrom(stack, v)
				key := strings.Join(cyc, ">")
				if !reported[key] {
					reported[key] = true
					vs = append(vs, Violation{
						Kind:    KindImportCycle,
						Subject: v,
						Detail:  "import cycle: " + strings.Join(cyc, " -> ") + " -> " + v,
					})
				}
			}
		}
		stack = stack[:len(stack)-1]
		color[u] = black
	}
	for _, n := range nodes {
		if color[n] == white {
			visit(n)
		}
	}
	return vs
}

// cycleFrom returns the suffix of stack starting at the first occurrence of v:
// the nodes on the cycle a back-edge to v closes.
func cycleFrom(stack []string, v string) []string {
	for i, s := range stack {
		if s == v {
			return append([]string(nil), stack[i:]...)
		}
	}
	return append([]string(nil), stack...)
}

// CheckDirection returns a violation for every internal edge between two shipped
// (product or main) packages that does not flow strictly downward through the
// layer ranks: an upward edge, or an illegal same-rank crossing (store<->pg,
// dispatch<->archive). Edges into harness packages are the isolation check's
// concern, not this one.
func (g *Graph) CheckDirection() []Violation {
	var vs []Violation
	for _, p := range g.sortedPackages() {
		fromClass, _, fromRank := classify(p.Rel)
		if fromClass == ClassHarness {
			continue // harness carries no layer, so it has no direction to violate.
		}
		for _, rel := range g.internalImports(p) {
			toClass, _, toRank := classify(rel)
			if toClass == ClassHarness {
				continue
			}
			if toRank >= fromRank {
				vs = append(vs, Violation{
					Kind:    KindImportDirection,
					Subject: p.Rel,
					Detail: fmt.Sprintf(
						"imports %q against the layering (rank %d must not import rank %d); the internal graph flows one direction, strictly downward",
						rel, fromRank, toRank),
				})
			}
		}
	}
	return vs
}

// CheckHarnessIsolation returns a violation for every shipped (product or main)
// package that imports a harness package: shipped code never depends on the test
// scaffolding. The reverse -- a harness fake importing its product seam -- is how
// the fakes work and is left alone.
func (g *Graph) CheckHarnessIsolation() []Violation {
	var vs []Violation
	for _, p := range g.sortedPackages() {
		if fromClass, _, _ := classify(p.Rel); fromClass == ClassHarness {
			continue
		}
		for _, rel := range g.internalImports(p) {
			if toClass, _, _ := classify(rel); toClass == ClassHarness {
				vs = append(vs, Violation{
					Kind:    KindHarnessIsolation,
					Subject: p.Rel,
					Detail:  fmt.Sprintf("imports harness package %q; shipped code must not depend on test-support scaffolding", rel),
				})
			}
		}
	}
	return vs
}

// CheckDBClients returns a violation for every package that imports the pgx
// driver yet is neither store nor pg nor a harness package: a third path to a
// database (specification section 10, store and pg the sole database clients;
// archive reuses them and never holds pgx itself). Harness packages may drive
// Postgres via pgx in tests, so they are exempt.
func (g *Graph) CheckDBClients() []Violation {
	var vs []Violation
	for _, p := range g.sortedPackages() {
		class, name, _ := classify(p.Rel)
		if class == ClassHarness {
			continue
		}
		if class == ClassProduct && (name == "store" || name == "pg") {
			continue
		}
		for _, imp := range p.Imports {
			if isPgxImport(imp) {
				vs = append(vs, Violation{
					Kind:    KindDBClient,
					Subject: p.Rel,
					Detail:  fmt.Sprintf("imports the pgx driver %q; only store and pg may open a database", imp),
				})
				break // one finding per package is enough.
			}
		}
	}
	return vs
}

// CheckPgxOnly returns a violation for every package that imports database/sql
// or a SQLite driver: all database access goes through pgx directly, with no
// SQLite anywhere (specification section 9). The ban is module-wide, harness
// included.
func (g *Graph) CheckPgxOnly() []Violation {
	var vs []Violation
	for _, p := range g.sortedPackages() {
		for _, imp := range p.Imports {
			switch {
			case isDatabaseSQL(imp):
				vs = append(vs, Violation{
					Kind:    KindForbiddenSQLImport,
					Subject: p.Rel,
					Detail:  fmt.Sprintf("imports %q; database access must use pgx directly, never database/sql", imp),
				})
			case isSQLiteDriver(imp):
				vs = append(vs, Violation{
					Kind:    KindForbiddenSQLImport,
					Subject: p.Rel,
					Detail:  fmt.Sprintf("imports SQLite driver %q; the module contains no SQLite", imp),
				})
			}
		}
	}
	return vs
}

// isPgxImport reports whether an import path is the pgx driver (or one of its
// subpackages).
func isPgxImport(path string) bool {
	return path == "github.com/jackc/pgx" || strings.HasPrefix(path, "github.com/jackc/pgx/")
}

// isDatabaseSQL reports whether an import path is database/sql or a subpackage.
func isDatabaseSQL(path string) bool {
	return path == "database/sql" || strings.HasPrefix(path, "database/sql/")
}

// isSQLiteDriver reports whether an import path names a SQLite driver.
func isSQLiteDriver(path string) bool {
	return strings.Contains(strings.ToLower(path), "sqlite")
}
