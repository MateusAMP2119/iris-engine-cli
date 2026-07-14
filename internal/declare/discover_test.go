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
	t.Run("workspace-tree-discovery", func(t *testing.T) {
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

// TestSampleWorkspaceShape pins the golden sample workspace shape exactly: two
// tables, three single-script pipelines under one ingest lane with its composer
// order, one read endpoint, plus the declared reads/writes on extract_orders and
// load_orders.
func TestSampleWorkspaceShape(t *testing.T) {
	t.Run("sample-workspace-shape", func(t *testing.T) {
		root := fixtures.WorkspaceGolden()

		ws, err := declare.DiscoverWorkspace(root)
		if err != nil {
			t.Fatalf("golden workspace discovery failed: %v", err)
		}

		// Exactly two tables.
		if len(ws.Schemas) != 2 {
			t.Errorf("schemas = %d, want 2 (raw.orders_staging, analytics.orders)", len(ws.Schemas))
		}
		tbls := map[string]bool{}
		for _, s := range ws.Schemas {
			tbls[s.Schema+"."+s.Table] = true
		}
		for _, tc := range []struct{ want string }{
			{"raw.orders_staging"},
			{"analytics.orders"},
		} {
			if !tbls[tc.want] {
				t.Errorf("missing table %q", tc.want)
			}
		}

		// Three single-script pipelines in one ingest lane, composed by
		// ingest/iris-declare.yaml with exact order.
		if len(ws.Pipelines) != 3 {
			t.Errorf("pipelines = %d, want 3", len(ws.Pipelines))
		}
		if len(ws.Composers) != 1 {
			t.Fatalf("composers = %d, want 1", len(ws.Composers))
		}
		comp := ws.Composers[0]
		if comp.Lane != "ingest" {
			t.Errorf("composer lane = %q, want ingest", comp.Lane)
		}
		wantOrder := []string{"extract_orders", "reset_counters", "load_orders"}
		if !equalSlices(comp.Spec.Order, wantOrder) {
			t.Errorf("composer order = %v, want %v", comp.Spec.Order, wantOrder)
		}

		// Map pipelines by name for content checks.
		byName := map[string]*declare.DiscoveredPipeline{}
		for i := range ws.Pipelines {
			p := &ws.Pipelines[i]
			byName[p.Declaration.Name] = p
		}
		// extract_orders reads and writes raw.orders_staging.
		ext, ok := byName["extract_orders"]
		if !ok {
			t.Fatal("extract_orders not discovered")
		}
		if len(ext.Declaration.Reads) != 1 || ext.Declaration.Reads[0].Table != "raw.orders_staging" {
			t.Errorf("extract_orders reads = %+v, want raw.orders_staging", ext.Declaration.Reads)
		}
		if len(ext.Declaration.Writes) != 1 || ext.Declaration.Writes[0].Table != "raw.orders_staging" {
			t.Errorf("extract_orders writes = %+v, want raw.orders_staging", ext.Declaration.Writes)
		}
		// load_orders reads staging and writes analytics.orders.
		ld, ok := byName["load_orders"]
		if !ok {
			t.Fatal("load_orders not discovered")
		}
		if len(ld.Declaration.Reads) != 1 || ld.Declaration.Reads[0].Table != "raw.orders_staging" {
			t.Errorf("load_orders reads = %+v, want raw.orders_staging", ld.Declaration.Reads)
		}
		if len(ld.Declaration.Writes) != 1 || ld.Declaration.Writes[0].Table != "analytics.orders" {
			t.Errorf("load_orders writes = %+v, want analytics.orders", ld.Declaration.Writes)
		}

		// One declared read endpoint orders_by_customer.
		eps, err := declare.DiscoverEndpoints(root)
		if err != nil {
			t.Fatalf("discover endpoints: %v", err)
		}
		if len(eps) != 1 {
			t.Fatalf("endpoints = %d, want 1", len(eps))
		}
		if eps[0].Name != "orders_by_customer" {
			t.Errorf("endpoint name = %q, want orders_by_customer", eps[0].Name)
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
