package arch

import "strings"

// This file holds the CLI-never-opens-meta check: the CLI has no path to a
// database. Every state change the CLI requests is executed by
// the leader daemon over the control connection, so the CLI process never opens a
// connection to meta or the data database -- it holds neither database client. The
// daemon (which the CLI dials over the socket) holds store (meta) and pg (data);
// the CLI reaching meta through the daemon is exactly the design, but the CLI
// importing store or pg itself would give it a direct database path, which this
// check forbids.

// KindCLIMetaAccess is a cli package (or subpackage) importing a database client
// (store or pg): a direct path to meta the CLI must never have.
const KindCLIMetaAccess = "cli-meta-access"

// metaClientPackages are the two database-client product packages the CLI must not
// import: store owns meta, pg owns data.
var metaClientPackages = map[string]bool{"store": true, "pg": true}

// CheckCLINoMetaAccess returns a violation for every cli package (the cli roster
// package or any package under it) that imports a database client (store or pg, or
// a subpackage of either). The CLI must reach meta only through the leader daemon
// over the control connection, so it never holds a database client of its own.
func (g *Graph) CheckCLINoMetaAccess() []Violation {
	var vs []Violation
	for _, p := range g.sortedPackages() {
		if !isCLIPackage(p.Rel) {
			continue
		}
		for _, rel := range g.internalImports(p) {
			if client, ok := dbClientRoot(rel); ok {
				vs = append(vs, Violation{
					Kind:    KindCLIMetaAccess,
					Subject: p.Rel,
					Detail: "imports the " + client + " database client (" + rel +
						"); the CLI never opens meta -- every state change goes through the leader daemon over the control connection",
				})
			}
		}
	}
	return vs
}

// isCLIPackage reports whether a repo-relative package key is a front-end package
// that must never open a database client (cli, tui, or a nested package under either).
func isCLIPackage(rel string) bool {
	return rel == "cli" || strings.HasPrefix(rel, "cli/") ||
		rel == "tui" || strings.HasPrefix(rel, "tui/")
}

// dbClientRoot reports whether a repo-relative import key resolves to a database
// client package (store or pg, or a subpackage of either), and names the client.
func dbClientRoot(rel string) (client string, ok bool) {
	root := rel
	if i := strings.IndexByte(rel, '/'); i >= 0 {
		root = rel[:i]
	}
	if metaClientPackages[root] {
		return root, true
	}
	return "", false
}
