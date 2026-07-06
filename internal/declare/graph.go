package declare

import (
	"fmt"
	"sort"
	"strings"
)

// Graph is a read-only view of the registered dependency graph: which pipeline
// names are registered and, for each, the pipelines it depends on (its direct
// upstreams -- the depends_on edges, "from depends_on to" with from the
// dependent, specification section 4). ValidateDependencies reads a declaration
// against this view.
//
// The view is deliberately minimal so more than one backing satisfies it: the
// in-memory Registry here (used to validate a declaration before it is
// persisted, and in tests), and E03.9's persisted registry (the pipelines and
// dependencies meta tables). Lane membership is intentionally absent: depends_on
// is a data gate that may cross lanes (specification section 3), so lane never
// enters this check.
type Graph interface {
	// Registered reports whether name is a registered pipeline.
	Registered(name string) bool
	// DependsOn returns the pipelines name depends on (its direct upstreams). The
	// order is unspecified; ValidateDependencies sorts before walking so the
	// chains it names are deterministic. Called only for registered names.
	DependsOn(name string) []string
}

// Registry is an in-memory Graph: each registered pipeline name mapped to the
// pipelines it depends on. It is the view ValidateDependencies reads at apply
// time before the declaration is persisted, and the graph builder tests use.
type Registry struct {
	edges map[string][]string
}

// NewRegistry returns an empty Registry with no pipelines registered.
func NewRegistry() *Registry {
	return &Registry{edges: make(map[string][]string)}
}

// Add registers name with the given depends_on edges and returns the Registry so
// calls chain (upstream-first: register a pipeline's upstreams before it).
// Re-adding a name replaces its prior edges, mirroring re-apply. The edge slice
// is copied, so a later mutation of the caller's slice cannot corrupt the view.
func (r *Registry) Add(name string, dependsOn ...string) *Registry {
	edges := make([]string, len(dependsOn))
	copy(edges, dependsOn)
	r.edges[name] = edges
	return r
}

// Registered reports whether name is a registered pipeline.
func (r *Registry) Registered(name string) bool {
	_, ok := r.edges[name]
	return ok
}

// DependsOn returns a copy of name's recorded upstreams, so a caller mutating the
// result cannot corrupt the registry's own edges.
func (r *Registry) DependsOn(name string) []string {
	edges := r.edges[name]
	out := make([]string, len(edges))
	copy(out, edges)
	return out
}

// ValidateDependencies checks a pipeline declaration's depends_on edges against
// the registered graph, the check every apply runs (specification sections 3 and
// 6.3). It enforces two rules and returns the first violation:
//
//   - upstream-first: every depends_on name must already be registered.
//     A reference to an unregistered pipeline is rejected immediately, naming the
//     missing pipeline; the graph builds upstream first, apply by apply, so a
//     reference is never deferred for later resolution. A self-reference (a name
//     equal to the declaration's own) is exempt here -- it is a cycle, reported
//     by the acyclicity rule below, not an unregistered reference.
//   - acyclicity: the registered graph plus this declaration (which replaces its
//     own prior edges) must stay acyclic. A cycle, including a self-reference, is
//     rejected with the offending chain named, e.g. "a -> b -> a". Because the
//     registered graph is already acyclic and a first registration cannot
//     reference forward, a cycle can only close through this declaration, so the
//     search starts from it.
//
// It reads only decl.Name and decl.DependsOn; lane, run, and access play no part.
func ValidateDependencies(registered Graph, decl *Pipeline) error {
	// Upstream-first: every reference must be registered, except a self-reference
	// (handled as a cycle below).
	for _, dep := range decl.DependsOn {
		if dep == decl.Name {
			continue
		}
		if !registered.Registered(dep) {
			return fmt.Errorf("declare: pipeline %q depends_on unregistered pipeline %q; references must be registered first (apply upstream first)", decl.Name, dep)
		}
	}

	// Acyclicity over the registered graph plus the new declaration.
	if chain := findCycle(registered, decl); chain != nil {
		return fmt.Errorf("declare: pipeline %q depends_on closes a cycle: %s", decl.Name, strings.Join(chain, " -> "))
	}
	return nil
}

// findCycle returns the chain of a cycle reachable from decl (its first node
// repeated at the end, e.g. [a, b, a]) or nil when none exists. It walks the
// registered graph with decl's edges substituted for decl.Name's (re-apply
// replaces prior edges), starting the depth-first search at decl.Name: since the
// registered graph is acyclic, any cycle must pass through the declaration.
// Neighbours are walked in sorted order so the reported chain is deterministic.
func findCycle(registered Graph, decl *Pipeline) []string {
	// neighbours returns node's direct upstreams, sorted and de-duplicated: decl's
	// declared edges for its own name, the registered edges for every other node.
	neighbours := func(node string) []string {
		if node == decl.Name {
			return sortedUnique(decl.DependsOn)
		}
		return sortedUnique(registered.DependsOn(node))
	}

	var path []string
	onPath := make(map[string]bool)
	done := make(map[string]bool)

	var dfs func(node string) []string
	dfs = func(node string) []string {
		path = append(path, node)
		onPath[node] = true
		for _, next := range neighbours(node) {
			if onPath[next] {
				// Back edge: extract the cycle from next's position on the path,
				// closing it by repeating next at the end.
				for i, p := range path {
					if p == next {
						cycle := append([]string{}, path[i:]...)
						return append(cycle, next)
					}
				}
			}
			if !done[next] {
				if cycle := dfs(next); cycle != nil {
					return cycle
				}
			}
		}
		onPath[node] = false
		path = path[:len(path)-1]
		done[node] = true
		return nil
	}
	return dfs(decl.Name)
}

// sortedUnique returns names sorted ascending with duplicates removed, leaving
// the input untouched. The stable order makes the cycle chains deterministic.
func sortedUnique(names []string) []string {
	if len(names) == 0 {
		return nil
	}
	out := make([]string, len(names))
	copy(out, names)
	sort.Strings(out)
	deduped := out[:0]
	var last string
	for i, n := range out {
		if i == 0 || n != last {
			deduped = append(deduped, n)
			last = n
		}
	}
	return deduped
}
