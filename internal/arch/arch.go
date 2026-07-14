// Package arch is the Iris structural gate: the executable form of the engine's
// module-layout invariants. It is a static-analysis leaf, importing no other Iris
// package, whose job is to prove -- by parsing go.mod, go.sum, and the module's
// own Go source, never by executing the Go toolchain -- the four load-bearing
// structural contracts every later epic leans on:
//
//   - the direct-dependency allowlist and the forbidden-anywhere ban on ORMs,
//     migration frameworks, schedulers, parquet, and cloud object-store clients;
//   - pgx-only database access with no database/sql and no SQLite driver;
//   - the acyclic, one-direction internal import graph;
//   - store and pg as the sole database clients, no third path opened by any
//     other package.
//
// # Package classification
//
// The check divides the module's packages into three classes, because the
// layering governs the shipped product roster, not the test scaffolding:
//
//   - Product: the product roster (cli, daemon, api, dispatch, exec, archive,
//     store, pg, declare, build, pat, version), each carrying a layer rank. The
//     one-direction rule governs edges between product packages.
//   - Main: cmd/iris, the single main package, the top of the direction (it may
//     import product packages downward and nothing may import it).
//   - Harness: every other package under internal/ -- the test-support
//     scaffolding (golden, roundtrip, fixtures, socketio, conformance) and the
//     recording-fake subpackages (store/storetest, pg/pgtest, exec/exectest).
//     Harness packages carry no layer rank and sit outside the product graph:
//     shipped code (Product or Main) never imports harness, but harness freely
//     imports product (a fake wraps its seam), and a harness package may hold a
//     pgx or Postgres-driving import a shipped non-database package never could.
//
// Every package, harness included, is still swept for import cycles and for the
// forbidden database/sql and SQLite imports: those bans are module-wide.
package arch

import "fmt"

// The Violation kinds this package reports, one per structural rule.
const (
	// KindDependencyAllowlist is a direct dependency outside the allowlist.
	KindDependencyAllowlist = "dependency-allowlist"
	// KindForbiddenModule is a module (direct or indirect) of a banned class:
	// ORM, migration framework, scheduler, parquet, cloud object store, or a
	// SQLite driver.
	KindForbiddenModule = "forbidden-module"
	// KindImportCycle is a cycle in the internal import graph.
	KindImportCycle = "import-cycle"
	// KindImportDirection is an internal edge that flows the wrong way through the
	// product layering (upward or an illegal same-rank crossing).
	KindImportDirection = "import-direction"
	// KindHarnessIsolation is shipped code (a product or main package) importing a
	// harness (test-support) package.
	KindHarnessIsolation = "harness-isolation"
	// KindDBClient is a package other than store or pg (and not a harness package)
	// importing the pgx driver: a third path to a database.
	KindDBClient = "db-client"
	// KindForbiddenSQLImport is an import of database/sql or a SQLite driver
	// anywhere in the module.
	KindForbiddenSQLImport = "forbidden-sql-import"
)

// Violation is one finding from a structural check: the rule it breaks (Kind),
// the package or module at fault (Subject), and a human-readable explanation
// (Detail).
type Violation struct {
	// Kind is the structural rule the finding breaks, one of the Kind constants.
	Kind string
	// Subject is the package (repo-relative key) or module path at fault.
	Subject string
	// Detail explains the violation.
	Detail string
}

// String renders the violation for test diagnostics and reports.
func (v Violation) String() string {
	return fmt.Sprintf("%s: %s: %s", v.Kind, v.Subject, v.Detail)
}
