package store_test

import (
	"context"
	"reflect"
	"strings"
	"testing"

	"github.com/MateusAMP2119/iris-lakehouse/internal/store"
	"github.com/MateusAMP2119/iris-lakehouse/internal/store/storetest"
)

// argsContain reports whether any of args deep-equals want, so a test can assert a
// closed-enum value (or a poisoned-run list) flows into a write without pinning its
// positional index.
func argsContain(args []any, want any) bool {
	for _, a := range args {
		if reflect.DeepEqual(a, want) {
			return true
		}
	}
	return false
}

// TestDeadLetterPropagatedWritesDeadRun proves the propagation write: when a
// downstream's awaited upstream run is dead-lettered, the single writer mints the
// downstream a NEVER-EXECUTED dead-lettered run (state dead_lettered, cause
// propagated, no exit_code or handle), its dead_letters worklist row (reason
// upstream_dead_lettered, failed_upstream the immediate upstream), and the
// poisoned upstream run recorded in run_inputs -- all as ONE atomic transaction
// (a single CTE), so the three rows commit together or not at all and the
// downstream's dead-letter can never be left without its worklist row or its
// lineage.
func TestDeadLetterPropagatedWritesDeadRun(t *testing.T) {
	rec := storetest.NewWriteRecorder()
	w := store.NewWriter(rec)

	err := w.DeadLetterPropagated(context.Background(), store.PropagatedRun{
		Pipeline:               "load_orders",
		DeclarationChecksum:    "sha256:decl",
		FailedUpstream:         "extract_orders",
		PoisonedUpstreamRunIDs: []int64{7},
	})
	if err != nil {
		t.Fatalf("DeadLetterPropagated: %v", err)
	}

	// One atomic transaction of one CTE statement: the run row, its dead_letters
	// worklist row, and its run_inputs lineage commit together or not at all.
	txns := rec.Transactions()
	if len(txns) != 1 || len(txns[0]) != 1 {
		t.Fatalf("DeadLetterPropagated issued %d transactions / %d statements, want one atomic CTE", len(txns), lenFirst(txns))
	}
	stmt := txns[0][0]
	sql := stmt.SQL

	// The three rows, one statement.
	if !strings.Contains(sql, "INSERT INTO runs") {
		t.Errorf("propagation write does not INSERT the downstream's run row:\n%s", sql)
	}
	if !strings.Contains(sql, "INSERT INTO dead_letters") {
		t.Errorf("propagation write does not INSERT the dead_letters worklist row:\n%s", sql)
	}
	if !strings.Contains(sql, "INSERT INTO run_inputs") {
		t.Errorf("propagation write does not INSERT the run_inputs lineage:\n%s", sql)
	}

	// Never executed: the run stamps no exit_code and no process handle.
	if strings.Contains(sql, "exit_code") {
		t.Errorf("a propagated run stamps an exit_code, but it never executed:\n%s", sql)
	}
	if strings.Contains(sql, "handle") {
		t.Errorf("a propagated run stamps a process handle, but it never executed:\n%s", sql)
	}

	// The closed-enum values the spec pins ride the args.
	if !argsContain(stmt.Args, string(store.RunDeadLettered)) {
		t.Errorf("propagated run state is not dead_lettered; args = %v", stmt.Args)
	}
	if !argsContain(stmt.Args, string(store.CausePropagated)) {
		t.Errorf("propagated run cause is not propagated; args = %v", stmt.Args)
	}
	if !argsContain(stmt.Args, string(store.ReasonUpstreamDeadLettered)) {
		t.Errorf("dead-letter reason is not upstream_dead_lettered; args = %v", stmt.Args)
	}
	if !argsContain(stmt.Args, "extract_orders") {
		t.Errorf("failed_upstream is not the immediate upstream; args = %v", stmt.Args)
	}

	// The poisoned upstream run flows into run_inputs (complete lineage), one row per
	// upstream via the unnest form -- the last positional arg.
	if !strings.Contains(sql, "unnest") {
		t.Errorf("run_inputs write is not the one-row-per-upstream unnest form:\n%s", sql)
	}
	if got := stmt.Args[len(stmt.Args)-1]; !reflect.DeepEqual(got, []int64{7}) {
		t.Errorf("poisoned upstream ids arg = %v, want the awaited dead-lettered run [7]", got)
	}
}

// TestFailedUpstreamAttribution proves the attribution asymmetry the dead_letters
// table encodes: a PROPAGATED dead-lettering records the immediate upstream in
// failed_upstream, while a DIRECT failure (a run that failed on its own) leaves
// failed_upstream null. The propagation write names the upstream; the
// direct-failure write (DeadLetterRun) writes no failed_upstream column at all,
// so it defaults to SQL NULL.
func TestFailedUpstreamAttribution(t *testing.T) {
	ctx := context.Background()

	// Propagated: failed_upstream names the immediate upstream.
	prop := storetest.NewWriteRecorder()
	wp := store.NewWriter(prop)
	if err := wp.DeadLetterPropagated(ctx, store.PropagatedRun{
		Pipeline:               "load_orders",
		DeclarationChecksum:    "sha256:decl",
		FailedUpstream:         "extract_orders",
		PoisonedUpstreamRunIDs: []int64{7},
	}); err != nil {
		t.Fatalf("DeadLetterPropagated: %v", err)
	}
	ptx := prop.Transactions()
	if len(ptx) != 1 || len(ptx[0]) != 1 {
		t.Fatalf("propagation issued %d transactions / %d statements, want one atomic CTE", len(ptx), lenFirst(ptx))
	}
	pstmt := ptx[0][0]
	if !strings.Contains(pstmt.SQL, "failed_upstream") {
		t.Errorf("propagated dead-letter does not write failed_upstream:\n%s", pstmt.SQL)
	}
	if !argsContain(pstmt.Args, "extract_orders") {
		t.Errorf("propagated dead-letter did not record the immediate upstream in failed_upstream; args = %v", pstmt.Args)
	}

	// Direct failure: DeadLetterRun writes no failed_upstream column, so it stays null.
	direct := storetest.NewWriteRecorder()
	wd := store.NewWriter(direct)
	if err := wd.DeadLetterRun(ctx, "run-9", store.ReasonFailed, "exited 1"); err != nil {
		t.Fatalf("DeadLetterRun: %v", err)
	}
	stmts := direct.Statements()
	if len(stmts) != 1 {
		t.Fatalf("DeadLetterRun issued %d statements, want exactly 1", len(stmts))
	}
	if strings.Contains(stmts[0].SQL, "failed_upstream") {
		t.Errorf("a direct failure names a failed_upstream; a direct failure must leave it null:\n%s", stmts[0].SQL)
	}
}
