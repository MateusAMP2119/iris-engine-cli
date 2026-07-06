package store_test

import (
	"path/filepath"
	"sort"
	"strings"
	"testing"

	"github.com/MateusAMP2119/iris-engine-cli/internal/golden"
	"github.com/MateusAMP2119/iris-engine-cli/internal/store"
)

// metaRoster is the exact seventeen-table roster of the meta control-plane
// database (specification section 4). The order is the spec's own listing order,
// which is also the create-if-missing emission order.
var metaRoster = []string{
	"pipelines",
	"dependencies",
	"lanes",
	"runs",
	"run_inputs",
	"dead_letters",
	"artifacts",
	"run_summaries",
	"journal_checkpoints",
	"pats",
	"pat_scopes",
	"endpoints",
	"endpoint_filters",
	"roles",
	"grants",
	"credentials",
	"migrations",
}

// specFKEdges is the exact foreign-key edge list of the meta schema, transcribed
// from the erDiagram at specification section 4 (doc line ~212). Every edge is
// "child.column->parent.column"; the set must match the model's edges exactly,
// no more and no fewer.
var specFKEdges = []string{
	"runs.pipeline->pipelines.name",
	"dependencies.from_pipeline->pipelines.name",
	"dependencies.to_pipeline->pipelines.name",
	"artifacts.pipeline->pipelines.name",
	"runs.artifact_hash->artifacts.hash",
	"run_inputs.run_id->runs.id",
	"run_inputs.upstream_run_id->runs.id",
	"dead_letters.run_id->runs.id",
	"dead_letters.failed_upstream->pipelines.name",
	"runs.replayed_from->runs.id",
	"roles.pipeline->pipelines.name",
	"pat_scopes.pat_id->pats.id",
	"roles.pat->pats.id",
	"grants.pg_role->roles.pg_role",
	"credentials.pg_role->roles.pg_role",
	"endpoint_filters.endpoint->endpoints.name",
}

// edgeSet folds a schema's foreign keys into the "child.column->parent.column"
// edge set the FK-graph contract compares against the spec.
func edgeSet(s store.Schema) map[string]bool {
	edges := map[string]bool{}
	for _, t := range s.Tables {
		for _, fk := range t.ForeignKeys {
			edges[t.Name+"."+fk.Column+"->"+fk.RefTable+"."+fk.RefColumn] = true
		}
	}
	return edges
}

// TestMetaFKGraphMatchesSpec proves the bootstrap DDL's foreign-key edges exactly
// match the specified graph: the sixteen erDiagram edges and no others, with
// pipelines as a zero-out-degree root, runs as the history root, and lanes,
// migrations, run_summaries, and journal_checkpoints carrying no FKs to the rest.
//
// spec: S04/fk-graph-matches-spec
func TestMetaFKGraphMatchesSpec(t *testing.T) {
	s := store.MetaSchema()
	got := edgeSet(s)

	want := map[string]bool{}
	for _, e := range specFKEdges {
		want[e] = true
	}

	for e := range want {
		if !got[e] {
			t.Errorf("meta FK graph is missing the spec edge %q", e)
		}
	}
	for e := range got {
		if !want[e] {
			t.Errorf("meta FK graph has the edge %q, which is not in the spec graph", e)
		}
	}

	// The named standalone tables carry no FKs (specification section 4:
	// migrations, run_summaries, journal_checkpoints, and lanes stand apart;
	// lanes references pipelines by name, never FK).
	for _, name := range []string{"lanes", "migrations", "run_summaries", "journal_checkpoints"} {
		tbl := tableByName(t, s, name)
		if len(tbl.ForeignKeys) != 0 {
			t.Errorf("%s carries %d FK(s), want none: %+v", name, len(tbl.ForeignKeys), tbl.ForeignKeys)
		}
	}

	// pipelines is the registry root: referenced by many, itself referencing none.
	if fks := tableByName(t, s, "pipelines").ForeignKeys; len(fks) != 0 {
		t.Errorf("pipelines (registry root) carries %d FK(s), want none: %+v", len(fks), fks)
	}

	// runs is the history root: referenced by run_inputs, dead_letters, and itself
	// (replay lineage). It must be the target of at least those edges.
	if !got["run_inputs.run_id->runs.id"] || !got["dead_letters.run_id->runs.id"] || !got["runs.replayed_from->runs.id"] {
		t.Error("runs is not referenced as the history root by run_inputs/dead_letters/replayed_from")
	}
}

// TestMetaOrderingIdentityNeverClock proves every meta ordering key is a monotonic
// bigint identity column, recorded_at is an opaque non-ordering text audit string,
// and no meta column is a clock type used for ordering.
//
// spec: S04/ordering-identity-never-clock
func TestMetaOrderingIdentityNeverClock(t *testing.T) {
	s := store.MetaSchema()

	// The identity ordering keys of the meta schema and their owning tables.
	identityCols := map[string]string{
		"runs":                "id",
		"journal_checkpoints": "seq",
		"migrations":          "applied_seq",
	}
	for table, col := range identityCols {
		c := columnByName(t, tableByName(t, s, table), col)
		if !c.Identity {
			t.Errorf("%s.%s is not declared as an identity ordering key", table, col)
		}
		if c.Type != "bigint" {
			t.Errorf("%s.%s identity type = %q, want bigint (monotonic bigint identity)", table, col, c.Type)
		}
	}

	for _, tbl := range s.Tables {
		for _, c := range tbl.Columns {
			// recorded_at is an opaque audit string, never a clock.
			if c.Name == "recorded_at" && c.Type != "text" {
				t.Errorf("%s.recorded_at type = %q, want text (opaque non-ordering audit string)", tbl.Name, c.Type)
			}
			// No meta column is a timestamp/timestamptz: ordering is identity, never
			// a clock (specification section 4).
			if strings.Contains(c.Type, "timestamp") {
				t.Errorf("%s.%s is a clock type %q; meta ordering is bigint identity, never a clock", tbl.Name, c.Name, c.Type)
			}
			// Every identity column is a bigint.
			if c.Identity && c.Type != "bigint" {
				t.Errorf("%s.%s is an identity column of type %q, want bigint", tbl.Name, c.Name, c.Type)
			}
		}
	}
}

// TestMetaRenderedDDLGolden pins the rendered meta DDL byte-for-byte: the embedded
// create-if-missing schema is a deterministic artifact, so a golden diff is a
// contract diff. Every table renders CREATE TABLE IF NOT EXISTS.
//
// spec: S04/meta-ddl-create-if-missing
func TestMetaRenderedDDLGolden(t *testing.T) {
	s := store.MetaSchema()
	stmts := s.DDL()

	for _, stmt := range stmts {
		if strings.HasPrefix(stmt, "CREATE TABLE") && !strings.Contains(stmt, "IF NOT EXISTS") {
			t.Errorf("meta DDL statement is not create-if-missing:\n%s", stmt)
		}
	}

	golden.Assert(t, []byte(strings.Join(stmts, "\n\n")+"\n"), filepath.Join("testdata", "meta_schema.sql"))
}

// tableByName returns the named table from the schema, failing the test when it
// is absent.
func tableByName(t *testing.T, s store.Schema, name string) store.Table {
	t.Helper()
	for _, tbl := range s.Tables {
		if tbl.Name == name {
			return tbl
		}
	}
	t.Fatalf("meta schema has no table %q", name)
	return store.Table{}
}

// columnByName returns the named column from the table, failing the test when it
// is absent.
func columnByName(t *testing.T, tbl store.Table, name string) store.Column {
	t.Helper()
	for _, c := range tbl.Columns {
		if c.Name == name {
			return c
		}
	}
	t.Fatalf("table %q has no column %q", tbl.Name, name)
	return store.Column{}
}

// sortedNames returns the table names of a schema, sorted, for set comparison.
func sortedNames(s store.Schema) []string {
	names := make([]string, 0, len(s.Tables))
	for _, t := range s.Tables {
		names = append(names, t.Name)
	}
	sort.Strings(names)
	return names
}
