package pg_test

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"

	"github.com/MateusAMP2119/iris-engine-cli/internal/pg"
)

// The provenance tests exercise the `iris data provenance` three-lookup walk as
// pure query logic over in-memory fixtures (specification sections 4 and 14):
// row -> stamps via the journal's provenance key, run id -> run facts with the
// archival-summary fallback, and recursive ancestry via run_inputs with the
// summary's consumed-upstream list standing in once a run's own ledger rows are
// pruned. The walk returns lineage only -- stamps, run facts, ancestry edges --
// and never a row image. No SQL runs here; the fixtures are the relational
// shapes the live wiring reads (journal rows from the data database, runs /
// run_summaries / run_inputs from meta).

// strPtr and i64Ptr build the nullable fixture fields (SQL NULL modeled as nil).
func strPtr(s string) *string { return &s }
func i64Ptr(v int64) *int64   { return &v }

// orderedRowKey is the contested row every journal fixture centers on.
var orderKey = pg.RowKey{Schema: "analytics", Table: "orders", RowPK: "9f3c"}

// TestProvenanceRowToRun proves lookup one of the walk: the provenance key
// (schema, table, row_pk) returns exactly that row's stamps, newest first, the
// latest surviving stamp names the current authoring run, and the full list is
// the layered write history. Stamps for other rows, tables, and schemas never
// leak into the result.
//
// spec: S14/provenance-row-to-run
func TestProvenanceRowToRun(t *testing.T) {
	journal := []pg.JournalEntry{
		{ID: 81, RunID: 39, Schema: "analytics", Table: "orders", RowPK: "9f3c", Op: pg.OpInsert, Undo: pg.UndoPromoted},
		{ID: 88, RunID: 42, Schema: "analytics", Table: "orders", RowPK: "9f3c", Op: pg.OpUpdate, Undo: pg.UndoOpen},
		// Noise the key must exclude: same table other row, other table, other schema.
		{ID: 82, RunID: 39, Schema: "analytics", Table: "orders", RowPK: "zz10", Op: pg.OpInsert, Undo: pg.UndoPromoted},
		{ID: 83, RunID: 40, Schema: "analytics", Table: "refunds", RowPK: "9f3c", Op: pg.OpInsert, Undo: pg.UndoOpen},
		{ID: 84, RunID: 41, Schema: "staging", Table: "orders", RowPK: "9f3c", Op: pg.OpInsert, Undo: pg.UndoOpen},
	}

	stamps := pg.RowStamps(journal, orderKey)
	want := []pg.Stamp{
		{EntryID: 88, RunID: 42, Op: pg.OpUpdate, Undo: pg.UndoOpen},
		{EntryID: 81, RunID: 39, Op: pg.OpInsert, Undo: pg.UndoPromoted},
	}
	if !reflect.DeepEqual(stamps, want) {
		t.Fatalf("RowStamps(%v) = %+v, want the row's layered history newest first %+v", orderKey, stamps, want)
	}

	author, ok := pg.CurrentAuthor(stamps)
	if !ok {
		t.Fatalf("CurrentAuthor found no surviving stamp; want entry 88")
	}
	if author.EntryID != 88 || author.RunID != 42 {
		t.Errorf("current author = entry %d run %d, want the latest surviving stamp entry 88 run 42", author.EntryID, author.RunID)
	}

	// The top-level walk carries the same answers.
	report, found := pg.WalkProvenance(journal, pg.Lineage{}, orderKey, 0)
	if !found {
		t.Fatalf("WalkProvenance did not find the stamped row %v", orderKey)
	}
	if report.Row != orderKey {
		t.Errorf("report row = %v, want %v", report.Row, orderKey)
	}
	if !reflect.DeepEqual(report.Stamps, want) {
		t.Errorf("report stamps = %+v, want %+v", report.Stamps, want)
	}
	if !report.Authored || report.Author.RunID != 42 {
		t.Errorf("report author = %+v (authored=%v), want run 42", report.Author, report.Authored)
	}

	// An unstamped row is simply not found: nothing speculative.
	if _, found := pg.WalkProvenance(journal, pg.Lineage{}, pg.RowKey{Schema: "analytics", Table: "orders", RowPK: "absent"}, 0); found {
		t.Errorf("WalkProvenance found a row with no stamps; want found = false")
	}
}

// TestProvenanceCurrentAuthorSurviving proves authorship resolves to the latest
// SURVIVING stamp: a row whose newest entry is wiped resolves to the latest
// non-wiped layer, a conflict-skipped write still counts as surviving (its
// write is still in the row's value), and wiped layers stay listed in the
// output, never hidden.
//
// spec: S14/provenance-current-author-surviving
func TestProvenanceCurrentAuthorSurviving(t *testing.T) {
	t.Run("newest wiped resolves to latest non-wiped layer", func(t *testing.T) {
		journal := []pg.JournalEntry{
			{ID: 81, RunID: 39, Schema: "analytics", Table: "orders", RowPK: "9f3c", Op: pg.OpInsert, Undo: pg.UndoPromoted},
			{ID: 88, RunID: 42, Schema: "analytics", Table: "orders", RowPK: "9f3c", Op: pg.OpUpdate, Undo: pg.UndoPromoted},
			{ID: 95, RunID: 57, Schema: "analytics", Table: "orders", RowPK: "9f3c", Op: pg.OpUpdate, Undo: pg.UndoWiped},
		}
		stamps := pg.RowStamps(journal, orderKey)
		if len(stamps) != 3 {
			t.Fatalf("got %d stamps, want all 3 layers listed (wiped included)", len(stamps))
		}
		if stamps[0].EntryID != 95 || stamps[0].Undo != pg.UndoWiped {
			t.Errorf("newest listed stamp = %+v, want the wiped entry 95 still listed first", stamps[0])
		}
		author, ok := pg.CurrentAuthor(stamps)
		if !ok || author.EntryID != 88 || author.RunID != 42 {
			t.Errorf("current author = %+v (ok=%v), want the latest non-wiped layer entry 88 run 42", author, ok)
		}
	})

	t.Run("skipped write survives as author", func(t *testing.T) {
		journal := []pg.JournalEntry{
			{ID: 10, RunID: 5, Schema: "analytics", Table: "orders", RowPK: "9f3c", Op: pg.OpInsert, Undo: pg.UndoPromoted},
			{ID: 12, RunID: 7, Schema: "analytics", Table: "orders", RowPK: "9f3c", Op: pg.OpUpdate, Undo: pg.UndoSkipped},
			{ID: 15, RunID: 9, Schema: "analytics", Table: "orders", RowPK: "9f3c", Op: pg.OpUpdate, Undo: pg.UndoWiped},
		}
		author, ok := pg.CurrentAuthor(pg.RowStamps(journal, orderKey))
		if !ok || author.EntryID != 12 || author.RunID != 7 {
			t.Errorf("current author = %+v (ok=%v), want the skipped-but-in-value entry 12 run 7", author, ok)
		}
	})

	t.Run("all layers wiped leaves history listed with no current author", func(t *testing.T) {
		journal := []pg.JournalEntry{
			{ID: 20, RunID: 11, Schema: "analytics", Table: "orders", RowPK: "9f3c", Op: pg.OpInsert, Undo: pg.UndoWiped},
		}
		report, found := pg.WalkProvenance(journal, pg.Lineage{}, orderKey, 0)
		if !found {
			t.Fatalf("a fully wiped row still has listed history; want found = true")
		}
		if len(report.Stamps) != 1 || report.Stamps[0].Undo != pg.UndoWiped {
			t.Errorf("report stamps = %+v, want the wiped layer still listed", report.Stamps)
		}
		if report.Authored {
			t.Errorf("report names author %+v for a fully wiped row; want no current author", report.Author)
		}
	})
}

// TestProvenanceLineageNeverImages proves the walk returns lineage only. The
// report's type graph carries no pre-image or row-image field at any depth, and
// a walk over journal entries holding captured pre-images never echoes the
// payload bytes anywhere in its output.
//
// spec: S14/provenance-lineage-never-images
func TestProvenanceLineageNeverImages(t *testing.T) {
	// Structural: no field named like an image anywhere in the report graph.
	seen := map[reflect.Type]bool{}
	var walk func(t *testing.T, typ reflect.Type, path string)
	walk = func(t *testing.T, typ reflect.Type, path string) {
		switch typ.Kind() {
		case reflect.Pointer, reflect.Slice, reflect.Array:
			walk(t, typ.Elem(), path)
		case reflect.Map:
			walk(t, typ.Key(), path)
			walk(t, typ.Elem(), path)
		case reflect.Struct:
			if seen[typ] {
				return
			}
			seen[typ] = true
			for i := 0; i < typ.NumField(); i++ {
				f := typ.Field(i)
				if strings.Contains(strings.ToLower(f.Name), "image") {
					t.Errorf("provenance report carries image field %s.%s: the walk must return lineage only", path, f.Name)
				}
				walk(t, f.Type, path+"."+f.Name)
			}
		}
	}
	walk(t, reflect.TypeOf(pg.ProvenanceReport{}), "ProvenanceReport")

	// Behavioral: captured pre-image payloads never surface in the walk output.
	const payload = `{"id":"9f3c","amount":100,"marker":"PRE-IMAGE-PAYLOAD-NEVER-RETURNED"}`
	journal := []pg.JournalEntry{
		{ID: 81, RunID: 39, Schema: "analytics", Table: "orders", RowPK: "9f3c", Op: pg.OpInsert, Undo: pg.UndoPromoted},
		{ID: 90, RunID: 57, Schema: "analytics", Table: "orders", RowPK: "9f3c", Op: pg.OpUpdate, PreImage: payload, Undo: pg.UndoOpen},
	}
	lineage := pg.Lineage{
		Runs:   []pg.RunRecord{{RunID: 57, Pipeline: "load_orders", State: "succeeded", DeclarationChecksum: "sha-decl"}},
		Inputs: []pg.RunInput{{RunID: 57, UpstreamRunID: 39}},
	}
	report, found := pg.WalkProvenance(journal, lineage, orderKey, pg.FullAncestry)
	if !found {
		t.Fatalf("WalkProvenance did not find the stamped row")
	}
	rendered, err := json.Marshal(report)
	if err != nil {
		t.Fatalf("marshaling the report: %v", err)
	}
	if strings.Contains(string(rendered), "PRE-IMAGE-PAYLOAD-NEVER-RETURNED") {
		t.Errorf("walk output echoes a captured pre-image payload:\n%s", rendered)
	}
	if strings.Contains(string(rendered), `"amount":100`) {
		t.Errorf("walk output echoes row-image content:\n%s", rendered)
	}
}

// TestProvenanceRunFactsSummaryFallback proves lookup two of the walk: a run id
// resolves to its pipeline, state, artifact hash, declaration checksum, and
// snapshot pin from the live run row, and falls back to the archival summary --
// same facts, FromSummary set -- once the run row has been pruned. A run id
// known to neither tier resolves to nothing.
//
// spec: S14/provenance-run-facts-summary-fallback
func TestProvenanceRunFactsSummaryFallback(t *testing.T) {
	lineage := pg.Lineage{
		Runs: []pg.RunRecord{
			{
				RunID: 42, Pipeline: "load_orders", State: "succeeded",
				ArtifactHash: strPtr("sha-bin-42"), DeclarationChecksum: "sha-decl-42",
				Pin: pg.SnapshotPin{SnapshotLSN: strPtr("0/16A3D2F0"), JournalFloor: i64Ptr(81), JournalCeiling: i64Ptr(95)},
			},
			// A live row shadows any stale summary: the live tier wins.
			{RunID: 50, Pipeline: "load_orders", State: "running", DeclarationChecksum: "sha-decl-50"},
		},
		Summaries: []pg.ArchivalSummary{
			{
				RunID: 39, Pipeline: "extract_orders", State: "succeeded",
				ArtifactHash: strPtr("sha-bin-39"), DeclarationChecksum: "sha-decl-39",
				ConsumedUpstreamRunIDs: []int64{30},
				Pin:                    pg.SnapshotPin{SnapshotLSN: strPtr("0/16A00000"), JournalFloor: i64Ptr(60), JournalCeiling: i64Ptr(80)},
			},
			{RunID: 50, Pipeline: "load_orders", State: "succeeded", DeclarationChecksum: "stale"},
		},
	}

	t.Run("live run row resolves directly", func(t *testing.T) {
		facts, ok := lineage.RunFacts(42)
		if !ok {
			t.Fatalf("RunFacts(42) not resolved; want the live run row")
		}
		want := pg.RunFacts{
			RunID: 42, Pipeline: "load_orders", State: "succeeded",
			ArtifactHash: strPtr("sha-bin-42"), DeclarationChecksum: "sha-decl-42",
			Pin:         pg.SnapshotPin{SnapshotLSN: strPtr("0/16A3D2F0"), JournalFloor: i64Ptr(81), JournalCeiling: i64Ptr(95)},
			FromSummary: false,
		}
		if !reflect.DeepEqual(facts, want) {
			t.Errorf("RunFacts(42) = %+v, want %+v", facts, want)
		}
	})

	t.Run("pruned run falls back to the archival summary", func(t *testing.T) {
		facts, ok := lineage.RunFacts(39)
		if !ok {
			t.Fatalf("RunFacts(39) not resolved; want the archival summary fallback")
		}
		want := pg.RunFacts{
			RunID: 39, Pipeline: "extract_orders", State: "succeeded",
			ArtifactHash: strPtr("sha-bin-39"), DeclarationChecksum: "sha-decl-39",
			Pin:         pg.SnapshotPin{SnapshotLSN: strPtr("0/16A00000"), JournalFloor: i64Ptr(60), JournalCeiling: i64Ptr(80)},
			FromSummary: true,
		}
		if !reflect.DeepEqual(facts, want) {
			t.Errorf("RunFacts(39) = %+v, want %+v", facts, want)
		}
	})

	t.Run("live row wins over a summary", func(t *testing.T) {
		facts, ok := lineage.RunFacts(50)
		if !ok || facts.FromSummary || facts.State != "running" {
			t.Errorf("RunFacts(50) = %+v (ok=%v), want the live row (running, not from summary)", facts, ok)
		}
	})

	t.Run("unknown run resolves to nothing", func(t *testing.T) {
		if facts, ok := lineage.RunFacts(7); ok {
			t.Errorf("RunFacts(7) = %+v, want ok = false for a run in neither tier", facts)
		}
	})
}

// TestProvenanceAncestryRecursive proves lookup three of the walk: run ancestry
// climbs upward via run_inputs, one edge per consumed upstream (fan-in = several
// edges), at depth 1 by default, with full recursive ancestry available from a
// single walk call and as a single WITH RECURSIVE statement over run_inputs. A
// diamond ancestor is expanded once but stays listed once per consumer.
//
// spec: S14/provenance-ancestry-recursive
func TestProvenanceAncestryRecursive(t *testing.T) {
	lineage := pg.Lineage{
		Inputs: []pg.RunInput{
			{RunID: 42, UpstreamRunID: 39},
			{RunID: 42, UpstreamRunID: 40},
			{RunID: 39, UpstreamRunID: 30},
			{RunID: 40, UpstreamRunID: 30}, // diamond: 30 consumed via both parents
			{RunID: 30, UpstreamRunID: 20},
			{RunID: 99, UpstreamRunID: 98}, // unrelated lineage, never visited
		},
	}

	t.Run("depth 1 by default, one edge per consumed upstream", func(t *testing.T) {
		got := lineage.Ancestry(42, 0)
		want := []pg.AncestryEdge{
			{RunID: 42, UpstreamRunID: 39, Depth: 1},
			{RunID: 42, UpstreamRunID: 40, Depth: 1},
		}
		if !reflect.DeepEqual(got, want) {
			t.Errorf("Ancestry(42, default) = %+v, want depth-1 fan-in %+v", got, want)
		}
	})

	t.Run("full recursive ancestry from one call", func(t *testing.T) {
		got := lineage.Ancestry(42, pg.FullAncestry)
		want := []pg.AncestryEdge{
			{RunID: 42, UpstreamRunID: 39, Depth: 1},
			{RunID: 42, UpstreamRunID: 40, Depth: 1},
			{RunID: 39, UpstreamRunID: 30, Depth: 2},
			{RunID: 40, UpstreamRunID: 30, Depth: 2},
			{RunID: 30, UpstreamRunID: 20, Depth: 3},
		}
		if !reflect.DeepEqual(got, want) {
			t.Errorf("Ancestry(42, full) = %+v, want the whole upward DAG %+v", got, want)
		}
	})

	t.Run("bounded depth stops the climb", func(t *testing.T) {
		got := lineage.Ancestry(42, 2)
		for _, e := range got {
			if e.Depth > 2 {
				t.Errorf("Ancestry(42, 2) climbed past its bound: %+v", e)
			}
		}
		if len(got) != 4 {
			t.Errorf("Ancestry(42, 2) = %+v, want the 4 edges within depth 2", got)
		}
	})

	t.Run("full ancestry is one recursive statement", func(t *testing.T) {
		sql := pg.RenderAncestryTrace()
		if !strings.Contains(sql, "WITH RECURSIVE") {
			t.Errorf("ancestry trace is not a recursive query:\n%s", sql)
		}
		if !strings.Contains(sql, "run_inputs") || !strings.Contains(sql, "upstream_run_id") {
			t.Errorf("ancestry trace does not walk run_inputs upward:\n%s", sql)
		}
		if !strings.Contains(sql, "$1") {
			t.Errorf("ancestry trace is not parameterized on the root run id:\n%s", sql)
		}
		if strings.Contains(strings.TrimRight(strings.TrimSpace(sql), ";"), ";") {
			t.Errorf("ancestry trace is more than a single statement:\n%s", sql)
		}
	})
}

// TestProvenanceSurvivesPruning proves the walk keeps naming the exact
// declaration checksum and binary hash after the run rows are pruned: the
// archival summary supplies the same facts, and ancestry falls back to the
// summary's consumed-upstream list once the run's own run_inputs rows are gone.
//
// spec: S03/provenance-survives-pruning
func TestProvenanceSurvivesPruning(t *testing.T) {
	journal := []pg.JournalEntry{
		{ID: 88, RunID: 42, Schema: "analytics", Table: "orders", RowPK: "9f3c", Op: pg.OpUpdate, Undo: pg.UndoPromoted},
	}
	const (
		binHash  = "sha256-bin-exact"
		declHash = "sha256-decl-exact"
	)

	live := pg.Lineage{
		Runs: []pg.RunRecord{{
			RunID: 42, Pipeline: "load_orders", State: "succeeded",
			ArtifactHash: strPtr(binHash), DeclarationChecksum: declHash,
			Pin: pg.SnapshotPin{SnapshotLSN: strPtr("0/16A3D2F0"), JournalFloor: i64Ptr(81), JournalCeiling: i64Ptr(95)},
		}},
		Inputs: []pg.RunInput{{RunID: 42, UpstreamRunID: 39}},
	}
	before, found := pg.WalkProvenance(journal, live, orderKey, 0)
	if !found || !before.FactsResolved {
		t.Fatalf("pre-prune walk did not resolve run facts (found=%v report=%+v)", found, before)
	}

	// Prune run 42: the run row and its run_inputs rows are gone; only the
	// FK-free archival summary remains, carrying the same hashes and the
	// consumed-upstream list.
	pruned := pg.Lineage{
		Summaries: []pg.ArchivalSummary{{
			RunID: 42, Pipeline: "load_orders", State: "succeeded",
			ArtifactHash: strPtr(binHash), DeclarationChecksum: declHash,
			ConsumedUpstreamRunIDs: []int64{39},
			Pin:                    pg.SnapshotPin{SnapshotLSN: strPtr("0/16A3D2F0"), JournalFloor: i64Ptr(81), JournalCeiling: i64Ptr(95)},
		}},
	}
	after, found := pg.WalkProvenance(journal, pruned, orderKey, 0)
	if !found || !after.FactsResolved {
		t.Fatalf("post-prune walk did not resolve run facts (found=%v report=%+v)", found, after)
	}
	if !after.Facts.FromSummary {
		t.Errorf("post-prune facts not marked as the archival fallback: %+v", after.Facts)
	}
	if after.Facts.DeclarationChecksum != declHash {
		t.Errorf("post-prune declaration checksum = %q, want the exact pre-prune value %q", after.Facts.DeclarationChecksum, declHash)
	}
	if after.Facts.ArtifactHash == nil || *after.Facts.ArtifactHash != binHash {
		t.Errorf("post-prune artifact hash = %v, want the exact pre-prune value %q", after.Facts.ArtifactHash, binHash)
	}
	if before.Facts.DeclarationChecksum != after.Facts.DeclarationChecksum {
		t.Errorf("declaration checksum changed across pruning: %q -> %q", before.Facts.DeclarationChecksum, after.Facts.DeclarationChecksum)
	}
	wantEdges := []pg.AncestryEdge{{RunID: 42, UpstreamRunID: 39, Depth: 1}}
	if !reflect.DeepEqual(after.Ancestry, wantEdges) {
		t.Errorf("post-prune ancestry = %+v, want the summary's consumed-upstream list %+v", after.Ancestry, wantEdges)
	}
	if !reflect.DeepEqual(before.Ancestry, after.Ancestry) {
		t.Errorf("ancestry changed across pruning: %+v -> %+v", before.Ancestry, after.Ancestry)
	}
}
