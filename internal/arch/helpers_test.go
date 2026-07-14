package arch_test

import "github.com/MateusAMP2119/iris-engine-cli/internal/arch"

// Shared fixtures for the graph checks. The synthetic module lets a test build an
// import graph in memory -- no I/O -- while the real-repo subtests drive the same
// checks over the module on disk.

// synthModule is the module path the in-memory graph fixtures live under.
const synthModule = "example.com/m"

// pgxImport is the pgx driver import path the database-client fixtures use. Held
// as data in a string, it is never a real import of the driver.
const pgxImport = "github.com/jackc/pgx/v5"

// internalImport builds the full import path of an internal package from its
// repo-relative key, as it would appear in a real import statement.
func internalImport(rel string) string {
	return synthModule + "/internal/" + rel
}

// synthGraph assembles an in-memory graph under the synthetic module.
func synthGraph(pkgs ...arch.Package) *arch.Graph {
	return &arch.Graph{Module: synthModule, Packages: pkgs}
}

// pkg is a terse constructor for a graph node.
func pkg(rel string, imports ...string) arch.Package {
	return arch.Package{Rel: rel, Imports: imports}
}
