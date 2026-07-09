package store_test

import (
	"context"
	"reflect"
	"strings"
	"testing"

	"github.com/MateusAMP2119/iris-engine-cli/internal/store"
	"github.com/MateusAMP2119/iris-engine-cli/internal/store/storetest"
)

// TestRunInputsTableShape proves the run_inputs consumption ledger's DDL shape
// (specification section 4): run_id and upstream_run_id are both bigint foreign keys
// to runs, the primary key is the composite of both columns, and there is a secondary
// index on upstream_run_id alone -- the reverse lookup the downstream walk (run show
// --trace --down, the dead-letter blast radius) needs, which the composite primary key
// cannot serve. The model is the single source the DDL renders from, so a shape
// assertion here is a DDL-shape assertion; the rendered CREATE INDEX is checked too.
//
// spec: S04/run-inputs-table-shape
func TestRunInputsTableShape(t *testing.T) {
	s := store.MetaSchema()
	ri := tableByName(t, s, "run_inputs")

	// Columns: run_id and upstream_run_id, both bigint.
	for _, name := range []string{"run_id", "upstream_run_id"} {
		if col := columnByName(t, ri, name); col.Type != "bigint" {
			t.Errorf("run_inputs.%s type = %q, want bigint", name, col.Type)
		}
	}

	// Composite primary key on BOTH columns.
	if want := []string{"run_id", "upstream_run_id"}; !reflect.DeepEqual(ri.PrimaryKey, want) {
		t.Errorf("run_inputs primary key = %v, want composite %v", ri.PrimaryKey, want)
	}

	// Both columns are foreign keys to runs.id.
	wantFKs := map[string]string{
		"run_id":          "runs.id",
		"upstream_run_id": "runs.id",
	}
	gotFKs := map[string]string{}
	for _, fk := range ri.ForeignKeys {
		gotFKs[fk.Column] = fk.RefTable + "." + fk.RefColumn
	}
	if !reflect.DeepEqual(gotFKs, wantFKs) {
		t.Errorf("run_inputs foreign keys = %v, want %v", gotFKs, wantFKs)
	}

	// A secondary index on upstream_run_id ALONE (the reverse lookup the PK cannot
	// serve): exactly [upstream_run_id], not the composite.
	var haveReverse bool
	for _, idx := range ri.Indexes {
		if reflect.DeepEqual(idx.Columns, []string{"upstream_run_id"}) {
			haveReverse = true
		}
	}
	if !haveReverse {
		t.Errorf("run_inputs is missing the reverse index on upstream_run_id alone; indexes = %v", ri.Indexes)
	}

	// The rendered DDL carries the reverse index as a CREATE INDEX statement, so the
	// downstream walk never seq-scans run_inputs.
	var indexStmt string
	for _, stmt := range ri.IndexDDL() {
		if strings.Contains(stmt, "run_inputs") && strings.Contains(stmt, "(upstream_run_id)") {
			indexStmt = stmt
		}
	}
	if indexStmt == "" {
		t.Errorf("rendered run_inputs DDL has no CREATE INDEX on (upstream_run_id); statements = %v", ri.IndexDDL())
	} else if !strings.HasPrefix(indexStmt, "CREATE INDEX IF NOT EXISTS") {
		t.Errorf("run_inputs reverse index is not create-if-missing:\n%s", indexStmt)
	}
}

// TestRunInputsWriteOnce proves run start writes one run_inputs row per consumed
// upstream run -- several under fan-in -- and never mutates them afterward
// (specification section 4). CreateRun (the E05.3 run-create path this reuses) mints
// the runs row and its run_inputs rows as one atomic CTE: the consumed upstream ids
// flow 1:1 into the single INSERT ... unnest, committed as one transaction, and no
// statement the write path issues ever UPDATEs run_inputs -- the ledger is written
// once and is immutable.
//
// spec: S04/run-inputs-write-once
func TestRunInputsWriteOnce(t *testing.T) {
	rec := storetest.NewWriteRecorder()
	w := store.NewWriter(rec)

	// Fan-in: three consumed upstream runs.
	upstreams := []int64{39, 40, 41}
	rc := store.RunRecord{
		Pipeline:               "load_orders",
		Cause:                  store.CauseLoop,
		DeclarationChecksum:    "sha256:decl",
		SnapshotLSN:            "0/16A3D2F0",
		JournalFloor:           81,
		ConsumedUpstreamRunIDs: upstreams,
	}
	if err := w.CreateRun(context.Background(), rc); err != nil {
		t.Fatalf("CreateRun: %v", err)
	}

	// One atomic transaction of one CTE statement: the runs insert and the run_inputs
	// rows commit together or not at all.
	txns := rec.Transactions()
	if len(txns) != 1 || len(txns[0]) != 1 {
		t.Fatalf("run-create issued %d transactions / %d statements, want one atomic CTE", len(txns), lenFirst(txns))
	}
	stmt := txns[0][0]

	// The run_inputs write is an INSERT feeding off the consumed ids, one row per
	// upstream (unnest), never a per-id statement.
	if !strings.Contains(stmt.SQL, "INSERT INTO run_inputs") {
		t.Errorf("run-create does not INSERT run_inputs:\n%s", stmt.SQL)
	}
	if !strings.Contains(stmt.SQL, "unnest") {
		t.Errorf("run_inputs write is not the one-row-per-upstream unnest form:\n%s", stmt.SQL)
	}
	// The full fan-in list flows into the statement, 1:1 with the consumed upstreams.
	if got := stmt.Args[len(stmt.Args)-1]; !reflect.DeepEqual(got, upstreams) {
		t.Errorf("consumed upstream ids arg = %v, want the fan-in list %v (one row per edge)", got, upstreams)
	}

	// Write-once: no statement the write path issues ever mutates run_inputs. The
	// ledger is inserted at run start and never updated or deleted afterward.
	for _, s := range rec.Statements() {
		up := strings.ToUpper(s.SQL)
		if strings.Contains(up, "RUN_INPUTS") && (strings.Contains(up, "UPDATE") || strings.Contains(up, "DELETE")) {
			t.Errorf("run_inputs is mutated after write, violating write-once:\n%s", s.SQL)
		}
	}
}

// lenFirst returns the statement count of the first transaction batch, or 0 when there
// is none, for a clearer failure message.
func lenFirst(txns [][]storetest.RecordedStatement) int {
	if len(txns) == 0 {
		return 0
	}
	return len(txns[0])
}
