package store_test

import (
	"context"
	"reflect"
	"strings"
	"testing"

	"github.com/MateusAMP2119/iris-lakehouse/internal/store"
	"github.com/MateusAMP2119/iris-lakehouse/internal/store/storetest"
)

// TestRunInputsTableShape proves the run_inputs consumption ledger's DDL shape:
// run_id and upstream_run_id are both bigint columns, the primary key is the
// composite of both columns, and there is a secondary index on upstream_run_id
// alone -- the reverse lookup the downstream walk (run show --trace --down, the
// dead-letter blast radius) needs, which the composite primary key cannot serve.
// run_id is a foreign key to runs.id; upstream_run_id is deliberately FK-free
// (see TestRunInputsUpstreamNotFK). The model is the single source the DDL
// renders from, so a shape assertion here is a DDL-shape assertion; the rendered
// CREATE INDEX is checked too.
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

	// run_id -- the downstream's OWN run -- is a foreign key to runs.id; it is the sole
	// FK on the table (upstream_run_id is FK-free, proven separately).
	wantFKs := map[string]string{
		"run_id": "runs.id",
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

// TestRunInputsUpstreamNotFK proves run_inputs.upstream_run_id is deliberately
// FK-free (the precedent is data_journal.run_id): it references a run logically
// only, resolving to a live run OR its archival summary. Count-based retention
// prunes an upstream run while a cross- pipeline downstream still holds a
// run_inputs row naming it; a hard FK there could only block the prune (RESTRICT,
// a live violation) or cascade-delete the surviving downstream's consumption
// record (erasing a live run's lineage and re-opening its gate), and the
// composite NOT NULL primary key forbids SET NULL. So the column carries no FK
// and no reference-to-runs appears for it in the rendered CREATE TABLE, exactly
// like data_journal.run_id. run_id (the downstream's own run) keeps its FK.
func TestRunInputsUpstreamNotFK(t *testing.T) {
	s := store.MetaSchema()
	ri := tableByName(t, s, "run_inputs")

	// No foreign key names upstream_run_id.
	for _, fk := range ri.ForeignKeys {
		if fk.Column == "upstream_run_id" {
			t.Errorf("run_inputs.upstream_run_id must be FK-free (resolves to a run or its summary), but it has FK -> %s.%s", fk.RefTable, fk.RefColumn)
		}
	}

	// The rendered CREATE TABLE carries no FOREIGN KEY clause for upstream_run_id: a
	// stray reference would re-enforce the constraint the doctrine removes.
	ddl := ri.CreateTableDDL()
	if strings.Contains(ddl, "FOREIGN KEY (upstream_run_id)") {
		t.Errorf("rendered run_inputs DDL still declares a FOREIGN KEY on upstream_run_id:\n%s", ddl)
	}

	// run_id, by contrast, is still a FOREIGN KEY -- the doctrine drops only the
	// upstream edge, not the downstream's own-run edge.
	if !strings.Contains(ddl, "FOREIGN KEY (run_id) REFERENCES runs (id)") {
		t.Errorf("rendered run_inputs DDL dropped the run_id -> runs.id FK, which must stay:\n%s", ddl)
	}
}

// TestRunInputsWriteOnce proves run start writes one run_inputs row per consumed
// upstream run -- several under fan-in -- and never mutates them afterward.
// CreateRun (the run-create write path this reuses, in runs.go) mints the runs row
// and its run_inputs rows as one atomic CTE: the consumed upstream ids flow 1:1
// into the single INSERT ... unnest, committed as one transaction, and no
// statement the write path issues ever UPDATEs run_inputs -- the ledger is written
// once and is immutable.
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
