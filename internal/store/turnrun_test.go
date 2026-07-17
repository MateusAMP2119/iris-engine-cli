package store_test

import (
	"context"
	"reflect"
	"strings"
	"testing"

	"github.com/MateusAMP2119/iris-lakehouse/internal/store"
	"github.com/MateusAMP2119/iris-lakehouse/internal/store/storetest"
)

// The turn-run write surface (#206) mints a producing turn's run directly
// running, completes it with the turn's stamps in one guarded statement, and
// mints a failed turn's run directly dead-lettered with its worklist row in one
// atomic CTE -- so a quiet turn writes nothing and a failed turn always records.

// sampleTurnRecord is one turn-run record with the identity fields filled.
func sampleTurnRecord(upstreams []int64) store.TurnRunRecord {
	return store.TurnRunRecord{
		Pipeline:               "orders",
		Cause:                  store.CauseLoop,
		DeclarationChecksum:    "sha256:decl",
		Handle:                 4242,
		LogRef:                 "logs/7.log",
		ConsumedUpstreamRunIDs: upstreams,
	}
}

func TestCreateTurnRunMintsRunning(t *testing.T) {
	rec := storetest.NewWriteRecorder()
	w := store.NewWriter(rec)

	upstreams := []int64{11, 12}
	if err := w.CreateTurnRun(context.Background(), sampleTurnRecord(upstreams)); err != nil {
		t.Fatalf("CreateTurnRun: %v", err)
	}
	txns := rec.Transactions()
	if len(txns) != 1 || len(txns[0]) != 1 {
		t.Fatalf("want one atomic transaction of one statement, got %+v", txns)
	}
	stmt := txns[0][0]
	if !strings.Contains(stmt.SQL, "INSERT INTO runs") || !strings.Contains(stmt.SQL, "INSERT INTO run_inputs") {
		t.Errorf("turn-run mint is not the atomic runs+run_inputs CTE:\n%s", stmt.SQL)
	}
	if got := stmt.Args[1]; got != string(store.RunRunning) {
		t.Errorf("state arg = %v, want running (a producing turn's run is minted already started)", got)
	}
	if got := stmt.Args[5]; got != 4242 {
		t.Errorf("handle arg = %v, want the resident process group id", got)
	}
	if got := stmt.Args[7]; !reflect.DeepEqual(got, upstreams) {
		t.Errorf("consumed upstream run ids arg = %v, want %v", got, upstreams)
	}
}

func TestCompleteTurnRunStampsTerminalInOneWrite(t *testing.T) {
	rec := storetest.NewWriteRecorder()
	w := store.NewWriter(rec)

	if err := w.CompleteTurnRun(context.Background(), "7", "0/1A2B3C", 40, 44, "logs/run-7.log"); err != nil {
		t.Fatalf("CompleteTurnRun: %v", err)
	}
	stmts := rec.Statements()
	if len(stmts) != 1 {
		t.Fatalf("want one guarded UPDATE, got %d", len(stmts))
	}
	e := stmts[0]
	for _, want := range []string{"state = $1", "exit_code = 0", "snapshot_lsn", "journal_floor", "journal_ceiling", "log_ref", "AND state = $7"} {
		if !strings.Contains(e.SQL, want) {
			t.Errorf("terminal stamp misses %q:\n%s", want, e.SQL)
		}
	}
	if e.Args[0] != any(store.RunSucceeded) || e.Args[6] != any(store.RunRunning) {
		t.Errorf("terminal transition args = %v, want running -> succeeded", e.Args)
	}
	if e.Args[1] != "0/1A2B3C" || e.Args[2] != int64(40) || e.Args[3] != int64(44) || e.Args[4] != "logs/run-7.log" {
		t.Errorf("stamp args = %v", e.Args)
	}
}

func TestDeadLetterTurnRunMintsDeadWithWorklist(t *testing.T) {
	rec := storetest.NewWriteRecorder()
	w := store.NewWriter(rec)

	if err := w.DeadLetterTurnRun(context.Background(), sampleTurnRecord(nil), store.ReasonFailed, `protocol violation: "done 0"`); err != nil {
		t.Fatalf("DeadLetterTurnRun: %v", err)
	}
	txns := rec.Transactions()
	if len(txns) != 1 || len(txns[0]) != 1 {
		t.Fatalf("want one atomic transaction of one statement, got %+v", txns)
	}
	stmt := txns[0][0]
	for _, want := range []string{"INSERT INTO runs", "INSERT INTO dead_letters", "INSERT INTO run_inputs"} {
		if !strings.Contains(stmt.SQL, want) {
			t.Errorf("dead turn mint misses %q:\n%s", want, stmt.SQL)
		}
	}
	if got := stmt.Args[1]; got != string(store.RunDeadLettered) {
		t.Errorf("state arg = %v, want dead_lettered (a failed turn always records)", got)
	}
	if got := stmt.Args[7]; got != string(store.ReasonFailed) {
		t.Errorf("reason arg = %v, want failed", got)
	}
	if got := stmt.Args[8]; got != `protocol violation: "done 0"` {
		t.Errorf("detail arg = %v, want the quoted offending line", got)
	}
}
