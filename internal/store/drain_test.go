package store_test

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"testing"

	"github.com/MateusAMP2119/iris-engine-cli/internal/store"
	"github.com/MateusAMP2119/iris-engine-cli/internal/store/storetest"
)

// TestDrainDeadLettersPureDiscard proves the drain write is a PURE DISCARD: it
// removes exactly the scoped dead_letters worklist rows in one atomic statement,
// re-runs nothing (no INSERT INTO runs), and alters no downstream (no INSERT INTO
// run_inputs): the ONLY statement issued is the dead_letters delete, scoped to
// exactly the given run ids and no others. The runs rows themselves are never
// named in the write, so a drained run's row is left exactly as it was in runs --
// untouched, never deleted, never re-minted -- the disposition retention (E05.9)
// later reads as prunable.
func TestDrainDeadLettersPureDiscard(t *testing.T) {
	rec := storetest.NewWriteRecorder()
	w := store.NewWriter(rec)

	if err := w.DrainDeadLetters(context.Background(), []int64{10, 20}); err != nil {
		t.Fatalf("DrainDeadLetters: %v", err)
	}

	// One atomic transaction of one statement: the discard commits whole or not at
	// all, never split across un-atomic Execs.
	txns := rec.Transactions()
	if len(txns) != 1 || len(txns[0]) != 1 {
		t.Fatalf("DrainDeadLetters issued %d transactions / %d statements, want one atomic delete", len(txns), lenFirst(txns))
	}
	stmt := txns[0][0]

	if !strings.Contains(stmt.SQL, "DELETE FROM dead_letters") {
		t.Errorf("drain write does not DELETE FROM dead_letters:\n%s", stmt.SQL)
	}

	// Pure discard: the write's SQL text names no other table. Nothing re-runs (no
	// runs insert), nothing downstream is altered (no run_inputs insert), and the
	// runs table is never even referenced -- the run row stays exactly as it was.
	if strings.Contains(stmt.SQL, "INSERT INTO runs") || strings.Contains(stmt.SQL, "UPDATE runs") || strings.Contains(stmt.SQL, "DELETE FROM runs") {
		t.Errorf("drain write touches runs; it must be a pure discard of the worklist entry alone:\n%s", stmt.SQL)
	}
	if strings.Contains(stmt.SQL, "run_inputs") {
		t.Errorf("drain write touches run_inputs; nothing downstream may be altered:\n%s", stmt.SQL)
	}

	// Across the WHOLE write (not just this one statement), only the one delete was
	// issued: draining mints nothing and re-runs nothing.
	if all := rec.Statements(); len(all) != 1 {
		t.Fatalf("DrainDeadLetters issued %d statements total, want exactly 1 (the discard alone)", len(all))
	}

	// Scoped-only: the exact run ids given, no others.
	if !reflect.DeepEqual(stmt.Args, []any{[]int64{10, 20}}) {
		t.Errorf("drain write args = %v, want exactly the scoped run ids [10 20]", stmt.Args)
	}
}

// TestDrainDeadLettersAtomicRollback proves the drain write is all-or-nothing: an
// injected meta failure rolls the delete back, so a failed drain leaves no worklist
// row half-removed. The single writer fails loudly rather than leaving the worklist
// in an inconsistent state.
func TestDrainDeadLettersAtomicRollback(t *testing.T) {
	rec := storetest.NewWriteRecorder()
	sentinel := errors.New("meta write failed")
	rec.FailTx(sentinel)
	w := store.NewWriter(rec)

	err := w.DrainDeadLetters(context.Background(), []int64{10})
	if err == nil {
		t.Fatal("DrainDeadLetters did not report the injected meta failure")
	}
	if !errors.Is(err, sentinel) {
		t.Errorf("DrainDeadLetters error = %v, want it to wrap the injected failure", err)
	}
	if txns := rec.Transactions(); len(txns) != 0 {
		t.Errorf("a failed drain recorded %d transactions, want none (atomic rollback)", len(txns))
	}
}

// TestDrainDeadLettersEmptyIsNoOp proves draining an empty scope issues no
// statement at all: nothing to discard writes nothing, rather than an empty-array
// DELETE that would still be a spurious meta write.
func TestDrainDeadLettersEmptyIsNoOp(t *testing.T) {
	rec := storetest.NewWriteRecorder()
	w := store.NewWriter(rec)

	if err := w.DrainDeadLetters(context.Background(), nil); err != nil {
		t.Fatalf("DrainDeadLetters(nil): %v", err)
	}
	if stmts := rec.Statements(); len(stmts) != 0 {
		t.Errorf("draining an empty scope issued %d statements, want none", len(stmts))
	}
}
