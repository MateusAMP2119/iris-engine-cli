package store_test

import (
	"context"
	"strings"
	"testing"

	"github.com/MateusAMP2119/iris-lakehouse/internal/store"
	"github.com/MateusAMP2119/iris-lakehouse/internal/store/storetest"
)

// This file proves the registry write surface: the pipelines/dependencies/lanes
// shapes and the atomic write paths that populate them. Every write rides the
// single meta writer over a recording fake -- no live Postgres -- so a test
// asserts the exact statement set and its transaction grouping.

// stmtsContaining returns the recorded statements whose SQL contains sub.
func stmtsContaining(stmts []storetest.RecordedStatement, sub string) []storetest.RecordedStatement {
	var out []storetest.RecordedStatement
	for _, s := range stmts {
		if strings.Contains(s.SQL, sub) {
			out = append(out, s)
		}
	}
	return out
}

// anyStmtContains reports whether any recorded statement's SQL contains sub.
func anyStmtContains(stmts []storetest.RecordedStatement, sub string) bool {
	return len(stmtsContaining(stmts, sub)) > 0
}

// equalStrings reports whether a and b are the same length with equal elements.
func equalStrings(a, b []string) bool {
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

// TestPipelinesTableShape proves the pipelines registry root: the table shape
// (name PK, folder, run JSON argv, artifact and data_mode CHECK-constrained) and
// that the write path persists exactly those columns for a registered pipeline.
func TestPipelinesTableShape(t *testing.T) {
	t.Run("pipelines-table-shape", func(t *testing.T) {
		// The schema model: name PK; folder, run (json), artifact, data_mode columns;
		// artifact in (source, built); data_mode in (disposable, permanent).
		s := store.MetaSchema()
		pipelines := tableByName(t, s, "pipelines")

		if got := pipelines.PrimaryKey; len(got) != 1 || got[0] != "name" {
			t.Errorf("pipelines PK = %v, want [name]", got)
		}
		if c := columnByName(t, pipelines, "run"); c.Type != "json" {
			t.Errorf("pipelines.run type = %q, want json (JSON argv)", c.Type)
		}
		columnByName(t, pipelines, "folder")
		checks := map[string][]string{}
		for _, ck := range pipelines.Checks {
			checks[ck.Column] = ck.Values
		}
		if want := []string{"source", "built"}; !equalStrings(checks["artifact"], want) {
			t.Errorf("pipelines.artifact CHECK = %v, want %v", checks["artifact"], want)
		}
		if want := []string{"disposable", "permanent"}; !equalStrings(checks["data_mode"], want) {
			t.Errorf("pipelines.data_mode CHECK = %v, want %v", checks["data_mode"], want)
		}

		// The write path: RegisterPipeline persists exactly the pipelines columns.
		rec := storetest.NewWriteRecorder()
		w := store.NewWriter(rec)
		row := store.PipelineRow{
			Name:     "load_orders",
			Folder:   "pipelines/ingest/load_orders",
			Run:      []string{"python", "main.py"},
			Artifact: store.ArtifactSource,
			DataMode: store.DataDisposable,
		}
		if err := w.RegisterPipeline(context.Background(), row, nil); err != nil {
			t.Fatalf("RegisterPipeline: %v", err)
		}

		inserts := stmtsContaining(rec.Statements(), "INSERT INTO pipelines")
		if len(inserts) != 1 {
			t.Fatalf("RegisterPipeline issued %d pipelines inserts, want 1", len(inserts))
		}
		ins := inserts[0]
		for _, col := range []string{"name", "folder", "run", "artifact", "data_mode"} {
			if !strings.Contains(ins.SQL, col) {
				t.Errorf("pipelines insert omits the %q column: %q", col, ins.SQL)
			}
		}
		wantArgs := []any{"load_orders", "pipelines/ingest/load_orders", `["python","main.py"]`, "source", "disposable"}
		if len(ins.Args) != len(wantArgs) {
			t.Fatalf("pipelines insert args = %v, want %v", ins.Args, wantArgs)
		}
		for i, w := range wantArgs {
			if ins.Args[i] != w {
				t.Errorf("pipelines insert arg %d = %v, want %v", i, ins.Args[i], w)
			}
		}
	})
}

// TestDependenciesEdgeShape proves the dependencies table records the depends_on
// graph as one edge row per depends_on: from_pipeline/to_pipeline FKs to pipelines
// (from = dependent), composite PK on both, indexed in both directions; and the
// write path emits exactly one edge row per declared upstream.
func TestDependenciesEdgeShape(t *testing.T) {
	t.Run("dependencies-edge-shape", func(t *testing.T) {
		s := store.MetaSchema()
		deps := tableByName(t, s, "dependencies")

		if want := []string{"from_pipeline", "to_pipeline"}; !equalStrings(deps.PrimaryKey, want) {
			t.Errorf("dependencies PK = %v, want %v (composite)", deps.PrimaryKey, want)
		}
		fkTargets := map[string]string{}
		for _, fk := range deps.ForeignKeys {
			fkTargets[fk.Column] = fk.RefTable + "." + fk.RefColumn
		}
		if fkTargets["from_pipeline"] != "pipelines.name" {
			t.Errorf("dependencies.from_pipeline FK = %q, want pipelines.name", fkTargets["from_pipeline"])
		}
		if fkTargets["to_pipeline"] != "pipelines.name" {
			t.Errorf("dependencies.to_pipeline FK = %q, want pipelines.name", fkTargets["to_pipeline"])
		}
		// Both directions indexed: the composite PK leads with from_pipeline (forward
		// lookup) and a secondary index covers to_pipeline (the reverse lookup).
		if deps.PrimaryKey[0] != "from_pipeline" {
			t.Errorf("dependencies PK does not lead with from_pipeline (forward index): %v", deps.PrimaryKey)
		}
		hasReverse := false
		for _, idx := range deps.Indexes {
			if len(idx.Columns) == 1 && idx.Columns[0] == "to_pipeline" {
				hasReverse = true
			}
		}
		if !hasReverse {
			t.Errorf("dependencies has no reverse (to_pipeline) index: %+v", deps.Indexes)
		}

		// The write path: one edge row per depends_on, from = the dependent.
		rec := storetest.NewWriteRecorder()
		w := store.NewWriter(rec)
		row := store.PipelineRow{Name: "load_orders", Folder: "f", Run: []string{"python", "m.py"}, Artifact: store.ArtifactSource, DataMode: store.DataDisposable}
		if err := w.RegisterPipeline(context.Background(), row, []string{"extract_orders", "reset_counters"}); err != nil {
			t.Fatalf("RegisterPipeline: %v", err)
		}
		edges := stmtsContaining(rec.Statements(), "INSERT INTO dependencies")
		if len(edges) != 2 {
			t.Fatalf("RegisterPipeline issued %d dependency edge rows, want 2 (one per depends_on)", len(edges))
		}
		got := map[string]bool{}
		for _, e := range edges {
			if len(e.Args) != 2 {
				t.Fatalf("dependency edge args = %v, want [from, to]", e.Args)
			}
			if e.Args[0] != "load_orders" {
				t.Errorf("dependency edge from = %v, want load_orders (the dependent)", e.Args[0])
			}
			got[e.Args[1].(string)] = true
		}
		if !got["extract_orders"] || !got["reset_counters"] {
			t.Errorf("dependency edges to = %v, want extract_orders and reset_counters", got)
		}
	})
}

// TestDependenciesPersistRows proves an applied pipeline's depends_on relationships
// persist as rows in the dependencies table, separate from lanes: registering a
// pipeline with upstreams writes dependency rows and touches no lanes row.
func TestDependenciesPersistRows(t *testing.T) {
	t.Run("dependencies-persist-rows", func(t *testing.T) {
		rec := storetest.NewWriteRecorder()
		w := store.NewWriter(rec)
		row := store.PipelineRow{Name: "load_orders", Folder: "f", Run: []string{"python", "m.py"}, Artifact: store.ArtifactSource, DataMode: store.DataDisposable}
		if err := w.RegisterPipeline(context.Background(), row, []string{"extract_orders"}); err != nil {
			t.Fatalf("RegisterPipeline: %v", err)
		}
		if !anyStmtContains(rec.Statements(), "INSERT INTO dependencies") {
			t.Errorf("depends_on did not persist to the dependencies table: %v", rec.Statements())
		}
		if anyStmtContains(rec.Statements(), " lanes ") || anyStmtContains(rec.Statements(), "INTO lanes") || anyStmtContains(rec.Statements(), "FROM lanes") {
			t.Errorf("a pipeline apply touched the lanes table; dependencies persist separately from lanes: %v", rec.Statements())
		}
	})
}

// TestDependenciesReApplyReplaces proves a re-apply with a different depends_on set
// replaces the pipeline's edges wholesale rather than accumulating them: the
// production write path clears the pipeline's edges before inserting the current
// ones, and the registry-view fake mirrors that replace semantics, so the graph
// validation reads is the graph the writer persists (never a stale union).
func TestDependenciesReApplyReplaces(t *testing.T) {
	ctx := context.Background()

	// Production: every RegisterPipeline clears the pipeline's edges (a delete)
	// before inserting the current ones, so a re-apply persists exactly the new set.
	rec := storetest.NewWriteRecorder()
	w := store.NewWriter(rec)
	row := store.PipelineRow{Name: "p", Folder: "f", Run: []string{"python", "m.py"}, Artifact: store.ArtifactSource, DataMode: store.DataDisposable}
	if err := w.RegisterPipeline(ctx, row, []string{"b", "c"}); err != nil {
		t.Fatalf("RegisterPipeline: %v", err)
	}
	if len(stmtsContaining(rec.Statements(), "DELETE FROM dependencies")) != 1 {
		t.Errorf("re-apply does not clear the pipeline's edges before inserting: %v", rec.Statements())
	}
	prodTo := map[string]bool{}
	for _, e := range stmtsContaining(rec.Statements(), "INSERT INTO dependencies") {
		prodTo[e.Args[1].(string)] = true
	}

	// Fake: re-seeding a name with a different depends_on set replaces its edges,
	// matching what a re-apply persists -- never the stale union {a, b, c}.
	reg := storetest.NewRegistryFake()
	reg.Register("p", "a", "b") // first apply
	reg.Register("p", "b", "c") // re-apply with a different set
	edges, err := reg.DependencyEdges(ctx)
	if err != nil {
		t.Fatalf("DependencyEdges: %v", err)
	}
	fakeTo := map[string]bool{}
	for _, e := range edges {
		if e.From == "p" {
			fakeTo[e.To] = true
		}
	}
	if len(fakeTo) != 2 || !fakeTo["b"] || !fakeTo["c"] {
		t.Errorf("fake edges after re-seed = %v, want {b, c} (replace, not append)", fakeTo)
	}
	// The fake agrees with what production persists, so validation and the writer
	// never diverge on a re-apply.
	if len(fakeTo) != len(prodTo) {
		t.Errorf("fake edges %v and production persisted edges %v differ in count", fakeTo, prodTo)
	}
	for to := range fakeTo {
		if !prodTo[to] {
			t.Errorf("fake edge to %q is absent from the production write set %v", to, prodTo)
		}
	}
}

// TestLanesRowComposerWritten proves lane state persists in lanes as name-keyed
// rows (lane, pipeline name, pos) and is written only by the composer's own apply:
// RewriteLane emits those rows, and a pipeline apply (RegisterPipeline) emits none.
func TestLanesRowComposerWritten(t *testing.T) {
	t.Run("lanes-row-composer-written", func(t *testing.T) {
		rec := storetest.NewWriteRecorder()
		w := store.NewWriter(rec)
		if err := w.RewriteLane(context.Background(), "ingest", []string{"extract_orders", "reset_counters", "load_orders"}); err != nil {
			t.Fatalf("RewriteLane: %v", err)
		}
		rows := stmtsContaining(rec.Statements(), "INSERT INTO lanes")
		if len(rows) != 3 {
			t.Fatalf("RewriteLane wrote %d lane rows, want 3 (one per ordered member)", len(rows))
		}
		// Name-keyed rows: (lane, pipeline name, pos), pos in walk order.
		for i, r := range rows {
			if len(r.Args) != 3 {
				t.Fatalf("lane row args = %v, want [lane, pipeline, pos]", r.Args)
			}
			if r.Args[0] != "ingest" {
				t.Errorf("lane row %d lane = %v, want ingest", i, r.Args[0])
			}
			if r.Args[2] != int64(i) {
				t.Errorf("lane row %d pos = %v, want %d (walk order)", i, r.Args[2], i)
			}
		}
		if rows[0].Args[1] != "extract_orders" || rows[2].Args[1] != "load_orders" {
			t.Errorf("lane member order not preserved: %v", rows)
		}

		// A pipeline apply never writes lanes -- position comes only from the composer.
		rec2 := storetest.NewWriteRecorder()
		w2 := store.NewWriter(rec2)
		row := store.PipelineRow{Name: "load_orders", Folder: "f", Run: []string{"python", "m.py"}, Artifact: store.ArtifactSource, DataMode: store.DataDisposable}
		if err := w2.RegisterPipeline(context.Background(), row, nil); err != nil {
			t.Fatalf("RegisterPipeline: %v", err)
		}
		if anyStmtContains(rec2.Statements(), "lanes") {
			t.Errorf("pipeline apply wrote a lanes statement; only the composer writes lanes: %v", rec2.Statements())
		}
	})
}

// TestLanesComposerAtomicRewrite proves a composer apply replaces a lane's rows as
// one atomic full-lane rewrite -- the clearing DELETE and the ordered INSERTs are a
// single meta transaction -- and that pipeline applies never write the lanes table.
func TestLanesComposerAtomicRewrite(t *testing.T) {
	t.Run("lanes-composer-atomic-rewrite", func(t *testing.T) {
		rec := storetest.NewWriteRecorder()
		w := store.NewWriter(rec)
		if err := w.RewriteLane(context.Background(), "ingest", []string{"a", "b"}); err != nil {
			t.Fatalf("RewriteLane: %v", err)
		}
		// One atomic transaction carrying the whole rewrite.
		txns := rec.Transactions()
		if len(txns) != 1 {
			t.Fatalf("RewriteLane committed %d transactions, want 1 (atomic full-lane rewrite)", len(txns))
		}
		batch := txns[0]
		if len(batch) == 0 || !strings.Contains(batch[0].SQL, "DELETE FROM lanes") {
			t.Errorf("atomic rewrite does not begin by clearing the lane: %v", batch)
		}
		if len(stmtsContaining(batch, "INSERT INTO lanes")) != 2 {
			t.Errorf("atomic rewrite does not re-insert the new order in the same transaction: %v", batch)
		}

		// Pipeline applies never write lanes.
		rec2 := storetest.NewWriteRecorder()
		w2 := store.NewWriter(rec2)
		row := store.PipelineRow{Name: "load_orders", Folder: "f", Run: []string{"python", "m.py"}, Artifact: store.ArtifactSource, DataMode: store.DataDisposable}
		if err := w2.RegisterPipeline(context.Background(), row, []string{}); err != nil {
			t.Fatalf("RegisterPipeline: %v", err)
		}
		if anyStmtContains(rec2.Statements(), "lanes") {
			t.Errorf("pipeline apply wrote the lanes table: %v", rec2.Statements())
		}
	})
}
