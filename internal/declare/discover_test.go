package declare_test

import (
	"testing"

	"github.com/MateusAMP2119/iris-engine-cli/internal/declare"
	"github.com/MateusAMP2119/iris-engine-cli/internal/fixtures"
)

// TestWorkspaceTreeDiscovery proves that, given a workspace tree, the engine
// discovers lane composers, pipeline declarations with their single script, and
// table schemas from their canonical locations.
func TestWorkspaceTreeDiscovery(t *testing.T) {
	t.Run("S10/workspace-tree-discovery", func(t *testing.T) {
		ws, err := declare.DiscoverWorkspace(fixtures.WorkspaceGolden())
		if err != nil {
			t.Fatalf("golden workspace discovery failed: %v", err)
		}

		// One lane composer: ingest, at pipelines/ingest/iris-declare.yaml,
		// carrying the lane's serial order.
		if len(ws.Composers) != 1 {
			t.Fatalf("composers = %d, want 1", len(ws.Composers))
		}
		comp := ws.Composers[0]
		if comp.Lane != "ingest" {
			t.Errorf("composer lane = %q, want ingest", comp.Lane)
		}
		if comp.Spec == nil {
			t.Fatal("composer spec is nil")
		}
		wantOrder := []string{"extract_orders", "reset_counters", "load_orders"}
		if got := comp.Spec.Order; !equalSlices(got, wantOrder) {
			t.Errorf("composer order = %v, want %v", got, wantOrder)
		}

		// Three pipeline declarations under pipelines/ingest/<pipeline>/, each
		// with its single script, all attributed to the ingest lane.
		pipes := map[string]declare.DiscoveredPipeline{}
		for _, p := range ws.Pipelines {
			pipes[p.Declaration.Name] = p
		}
		for _, name := range []string{"extract_orders", "reset_counters", "load_orders"} {
			p, ok := pipes[name]
			if !ok {
				t.Errorf("pipeline %q not discovered", name)
				continue
			}
			if p.Lane != "ingest" {
				t.Errorf("pipeline %q lane = %q, want ingest", name, p.Lane)
			}
			if p.Script != "main.py" {
				t.Errorf("pipeline %q script = %q, want main.py", name, p.Script)
			}
		}
		if len(ws.Pipelines) != 3 {
			t.Errorf("pipelines = %d, want 3", len(ws.Pipelines))
		}

		// Two table schemas under schemas/<schema>/<table>/table.yaml.
		schemas := map[string]bool{}
		for _, tbl := range ws.Schemas {
			schemas[tbl.Schema+"."+tbl.Table] = true
		}
		for _, key := range []string{"raw.orders_staging", "analytics.orders"} {
			if !schemas[key] {
				t.Errorf("table schema %q not discovered", key)
			}
		}
		if len(ws.Schemas) != 2 {
			t.Errorf("schemas = %d, want 2", len(ws.Schemas))
		}
	})
}

// equalSlices reports whether two string slices are equal in order.
func equalSlices(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
