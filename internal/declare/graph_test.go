package declare_test

import (
	"strings"
	"testing"

	"github.com/MateusAMP2119/iris-engine-cli/internal/declare"
)

// newDecl builds a pipeline declaration carrying only the fields the dependency
// graph check reads: its name and its depends_on edges. Run and the rest are
// irrelevant to ValidateDependencies, so they stay zero.
func newDecl(name string, dependsOn ...string) *declare.Pipeline {
	return &declare.Pipeline{Name: name, DependsOn: dependsOn}
}

// mustReject asserts ValidateDependencies rejects decl against reg with an error
// whose message contains want (the named chain or the missing pipeline).
func mustReject(t *testing.T, reg declare.Graph, decl *declare.Pipeline, want string) {
	t.Helper()
	err := declare.ValidateDependencies(reg, decl)
	if err == nil {
		t.Fatalf("ValidateDependencies(%q) = nil, want a rejection naming %q", decl.Name, want)
	}
	if !strings.Contains(err.Error(), want) {
		t.Errorf("rejection %q does not name %q", err, want)
	}
}

// mustAccept asserts ValidateDependencies accepts decl against reg.
func mustAccept(t *testing.T, reg declare.Graph, decl *declare.Pipeline) {
	t.Helper()
	if err := declare.ValidateDependencies(reg, decl); err != nil {
		t.Fatalf("ValidateDependencies(%q) = %v, want nil (accepted)", decl.Name, err)
	}
}

// TestDependencyCycleRejected proves a depends_on graph containing a cycle --
// including a self-reference -- is rejected with the offending chain named in the
// error, and that the chain is deterministic (neighbours are walked in a stable
// order so the reported chain does not flake).
func TestDependencyCycleRejected(t *testing.T) {
	t.Run("S03/depends-on-cycle-rejected", func(t *testing.T) {
		cases := []struct {
			name  string
			reg   *declare.Registry
			decl  *declare.Pipeline
			chain string
		}{
			{
				// Self-reference on a first registration: a depends on itself. The
				// referenced name equals the declaration's own, so it is a cycle, not
				// an unregistered reference.
				name:  "self-reference-first-apply",
				reg:   declare.NewRegistry(),
				decl:  newDecl("a", "a"),
				chain: "a -> a",
			},
			{
				// Self-reference on re-apply: a is already registered and re-applies
				// depending on itself.
				name:  "self-reference-re-apply",
				reg:   declare.NewRegistry().Add("a"),
				decl:  newDecl("a", "a"),
				chain: "a -> a",
			},
			{
				// Two-node cycle: b depends on a is registered; re-applying a with
				// depends_on b closes a -> b -> a.
				name:  "two-node-cycle",
				reg:   declare.NewRegistry().Add("a").Add("b", "a"),
				decl:  newDecl("a", "b"),
				chain: "a -> b -> a",
			},
			{
				// Three-node cycle: a <- b <- c registered; re-applying a with
				// depends_on c closes a -> c -> b -> a. The chain is the stable DFS
				// walk, asserted verbatim.
				name:  "three-node-cycle",
				reg:   declare.NewRegistry().Add("a").Add("b", "a").Add("c", "b"),
				decl:  newDecl("a", "c"),
				chain: "a -> c -> b -> a",
			},
		}
		for _, tc := range cases {
			t.Run(tc.name, func(t *testing.T) {
				mustReject(t, tc.reg, tc.decl, tc.chain)
			})
		}

		// Determinism: the same cycle input yields the same named chain every call.
		reg := declare.NewRegistry().Add("a").Add("b", "a").Add("c", "b")
		first := declare.ValidateDependencies(reg, newDecl("a", "c"))
		second := declare.ValidateDependencies(reg, newDecl("a", "c"))
		if first == nil || second == nil || first.Error() != second.Error() {
			t.Errorf("cycle chain is not deterministic: %v vs %v", first, second)
		}
	})
}

// TestUnregisteredRefRejected proves a depends_on reference to a pipeline not
// already registered is rejected immediately (upstream-first, single-file),
// never deferred for later resolution, and that the error names the missing
// pipeline.
func TestUnregisteredRefRejected(t *testing.T) {
	t.Run("S03/unregistered-ref-rejected", func(t *testing.T) {
		// A reference to a wholly unregistered pipeline is rejected, naming it.
		mustReject(t, declare.NewRegistry(), newDecl("b", "a"), "a")

		// The rejection is synchronous: ValidateDependencies is a pure function that
		// returns the error on the calling goroutine, so an unresolved reference can
		// never be "deferred" -- there is nowhere to defer it to.
		if err := declare.ValidateDependencies(declare.NewRegistry(), newDecl("b", "a")); err == nil {
			t.Fatal("unregistered reference was not rejected on the call, want an immediate error")
		}

		// Among several references, the first unregistered one is named even when a
		// sibling reference does resolve: the missing pipeline "c" is named.
		reg := declare.NewRegistry().Add("a")
		mustReject(t, reg, newDecl("b", "a", "c"), "c")

		// Contrast: once every reference is registered, the declaration is accepted.
		mustAccept(t, declare.NewRegistry().Add("a"), newDecl("b", "a"))
	})
}

// TestDependenciesCrossLaneOK proves a depends_on edge between pipelines in
// different lanes is accepted: lane membership is not an input to the dependency
// check at all (depends_on is a data gate, not lane order), so a cross-lane edge
// and a same-lane edge get the identical verdict.
func TestDependenciesCrossLaneOK(t *testing.T) {
	t.Run("S04/dependencies-cross-lane-ok", func(t *testing.T) {
		// extract sits in the ingest lane; load sits in a different lane and depends
		// on it. The edge crosses lanes and is accepted.
		reg := declare.NewRegistry().Add("extract")
		crossLane := &declare.Pipeline{Name: "load", Lane: "warehouse", DependsOn: []string{"extract"}}
		mustAccept(t, reg, crossLane)

		// The verdict is identical for a same-lane edge: lane never enters
		// ValidateDependencies (its signature carries no lane), so ordering and
		// gating stay separate mechanisms from acyclicity and upstream-first.
		sameLane := &declare.Pipeline{Name: "load", Lane: "ingest", DependsOn: []string{"extract"}}
		crossErr := declare.ValidateDependencies(reg, crossLane)
		sameErr := declare.ValidateDependencies(reg, sameLane)
		if crossErr != nil || sameErr != nil {
			t.Fatalf("cross-lane verdict %v, same-lane verdict %v; want both accepted", crossErr, sameErr)
		}
	})
}

// TestApplyRejectsCycles proves every apply checks acyclicity over the registered
// graph plus the new declaration and rejects a cycle by naming the chain. Because
// a first registration cannot reference forward, cycles close only via re-apply:
// the canonical scenario is a and b registered acyclically, then a re-applied to
// depend on b.
func TestApplyRejectsCycles(t *testing.T) {
	t.Run("S06.3/apply-rejects-cycles", func(t *testing.T) {
		// The re-apply scenario from the spec: a registered with no deps, b depends
		// on a; re-applying a with depends_on b closes the cycle a -> b -> a.
		reg := declare.NewRegistry().Add("a").Add("b", "a")
		mustReject(t, reg, newDecl("a", "b"), "a -> b -> a")

		// A deeper registered chain x <- y <- z; re-applying x to depend on z closes
		// x -> z -> y -> x. The check spans the whole registered graph, not just
		// direct neighbours.
		deep := declare.NewRegistry().Add("x").Add("y", "x").Add("z", "y")
		mustReject(t, deep, newDecl("x", "z"), "x -> z -> y -> x")

		// An acyclic new declaration over the same registered graph is accepted: c
		// depending on both a and b introduces no cycle.
		mustAccept(t, reg, newDecl("c", "a", "b"))
	})
}

// TestApplyUpstreamFirst proves apply rejects a declaration whose depends_on
// names an unregistered pipeline, naming the missing pipeline, and that once the
// upstream is registered the same declaration applies -- the graph builds
// upstream first, apply by apply.
func TestApplyUpstreamFirst(t *testing.T) {
	t.Run("S06.3/apply-upstream-first", func(t *testing.T) {
		// A first registration cannot reference forward: b depends on a with nothing
		// registered yet is rejected, and the rejection names the missing pipeline a.
		mustReject(t, declare.NewRegistry(), newDecl("b", "a"), "a")

		// Upstream first: register a, then b depends_on a applies cleanly.
		reg := declare.NewRegistry().Add("a")
		mustAccept(t, reg, newDecl("b", "a"))

		// A pipeline with no dependencies is always applicable, no upstream needed.
		mustAccept(t, declare.NewRegistry(), newDecl("solo"))
	})
}

// TestRegistryGraphView proves the in-memory Registry satisfies the Graph view
// the persisted registry (E03.9) will also satisfy: registered names report
// registered, unknown names do not, and DependsOn returns a node's recorded
// upstreams. This is the seam ValidateDependencies reads through.
func TestRegistryGraphView(t *testing.T) {
	t.Run("S04/dependencies-cross-lane-ok", func(t *testing.T) {
		var g declare.Graph = declare.NewRegistry().Add("a").Add("b", "a")
		if !g.Registered("a") || !g.Registered("b") {
			t.Error("registered pipelines a and b not reported as registered")
		}
		if g.Registered("missing") {
			t.Error("unregistered pipeline reported as registered")
		}
		if deps := g.DependsOn("b"); len(deps) != 1 || deps[0] != "a" {
			t.Errorf("DependsOn(b) = %v, want [a]", deps)
		}
		if deps := g.DependsOn("a"); len(deps) != 0 {
			t.Errorf("DependsOn(a) = %v, want empty", deps)
		}
		// A returned slice is a copy: mutating it must not corrupt the registry's
		// own edges (partial-failure safety for the shared view).
		deps := g.DependsOn("b")
		if len(deps) > 0 {
			deps[0] = "tampered"
		}
		if again := g.DependsOn("b"); len(again) != 1 || again[0] != "a" {
			t.Errorf("DependsOn(b) after caller mutation = %v, want [a] (returned slice must be a copy)", again)
		}
	})
}
