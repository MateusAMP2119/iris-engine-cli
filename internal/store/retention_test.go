package store_test

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"testing"

	"github.com/MateusAMP2119/iris-engine-cli/internal/store"
	"github.com/MateusAMP2119/iris-engine-cli/internal/store/storetest"
)

// ptrStr and ptrI64 build pointers for the nullable summary fields (artifact_hash,
// snapshot_lsn, journal_floor/ceiling): a set value is a pointer, nil is SQL NULL.
func ptrStr(s string) *string { return &s }
func ptrI64(n int64) *int64   { return &n }

// fullPrunableRun is a run with every field populated: a succeeded run that consumed
// two upstreams and carries the full snapshot pin. Tests override the fields a case
// exercises (e.g. a dev run's nil artifact hash).
func fullPrunableRun() store.PrunableRun {
	return store.PrunableRun{
		RunID:                  42,
		Pipeline:               "load_orders",
		State:                  store.RunSucceeded,
		ArtifactHash:           ptrStr("sha256:abc"),
		DeclarationChecksum:    "decl-9f",
		ConsumedUpstreamRunIDs: []int64{7, 11},
		SnapshotLSN:            ptrStr("0/16B3F00"),
		JournalFloor:           ptrI64(1000),
		JournalCeiling:         ptrI64(1200),
	}
}

// TestBuildRunSummaryCopiesRunFields proves the archival summary copies exactly
// the run fields that must outlive pruning: run_id, pipeline (copied, not an FK),
// state, artifact_hash, declaration_checksum, the consumed upstream run ids as a
// JSON array, and the snapshot_lsn / journal_floor / journal_ceiling pin. It is
// pure. A dev run's nil artifact hash and nil pin survive as nil (SQL NULL), and
// a root run's empty consumed set becomes the JSON empty array "[]", never
// "null".
func TestBuildRunSummaryCopiesRunFields(t *testing.T) {
	tests := []struct {
		name string
		run  store.PrunableRun
		want store.RunSummary
	}{
		{
			name: "full run copies every field and the pin",
			run:  fullPrunableRun(),
			want: store.RunSummary{
				RunID:                      42,
				Pipeline:                   "load_orders",
				State:                      store.RunSucceeded,
				ArtifactHash:               ptrStr("sha256:abc"),
				DeclarationChecksum:        "decl-9f",
				ConsumedUpstreamRunIDsJSON: "[7,11]",
				SnapshotLSN:                ptrStr("0/16B3F00"),
				JournalFloor:               ptrI64(1000),
				JournalCeiling:             ptrI64(1200),
			},
		},
		{
			name: "dev run keeps nil artifact hash and nil pin as SQL NULL",
			run: store.PrunableRun{
				RunID:                  9,
				Pipeline:               "extract",
				State:                  store.RunDeadLettered,
				ArtifactHash:           nil,
				DeclarationChecksum:    "decl-00",
				ConsumedUpstreamRunIDs: []int64{3},
				SnapshotLSN:            nil,
				JournalFloor:           nil,
				JournalCeiling:         nil,
			},
			want: store.RunSummary{
				RunID:                      9,
				Pipeline:                   "extract",
				State:                      store.RunDeadLettered,
				ArtifactHash:               nil,
				DeclarationChecksum:        "decl-00",
				ConsumedUpstreamRunIDsJSON: "[3]",
				SnapshotLSN:                nil,
				JournalFloor:               nil,
				JournalCeiling:             nil,
			},
		},
		{
			name: "root run's empty consumed set becomes the JSON empty array",
			run: store.PrunableRun{
				RunID:                  1,
				Pipeline:               "seed",
				State:                  store.RunSucceeded,
				DeclarationChecksum:    "decl-seed",
				ConsumedUpstreamRunIDs: nil,
			},
			want: store.RunSummary{
				RunID:                      1,
				Pipeline:                   "seed",
				State:                      store.RunSucceeded,
				DeclarationChecksum:        "decl-seed",
				ConsumedUpstreamRunIDsJSON: "[]",
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := store.BuildRunSummary(tt.run)
			if !reflect.DeepEqual(got, tt.want) {
				t.Fatalf("BuildRunSummary =\n  %+v\nwant\n  %+v", got, tt.want)
			}
		})
	}
}

// pruneTxn runs PruneRun through a fresh recorder and returns it, failing the test on
// error. deleteLog is passed straight through.
func pruneTxn(t *testing.T, run store.PrunableRun, deleteLog func(string) error) *storetest.WriteRecorder {
	t.Helper()
	rec := storetest.NewWriteRecorder()
	w := store.NewWriter(rec)
	if err := w.PruneRun(context.Background(), run, deleteLog); err != nil {
		t.Fatalf("PruneRun: %v", err)
	}
	return rec
}

// stmtIndex returns the index of the first recorded statement whose SQL contains
// fragment, or -1. Statements is the flat issue-order record.
func stmtIndex(stmts []storetest.RecordedStatement, fragment string) int {
	for i, s := range stmts {
		if strings.Contains(s.SQL, fragment) {
			return i
		}
	}
	return -1
}

// TestPruneRunSummaryOutlivesPruning proves the pruner writes the archival
// summary BEFORE it deletes the run, and that run_summaries is insert-only --
// never the target of a delete (run_summaries is insert-only, never pruned, so it
// outlives all it summarizes). The summary INSERT is the first statement issued
// and precedes the run DELETE; no statement anywhere deletes from run_summaries.
func TestPruneRunSummaryOutlivesPruning(t *testing.T) {
	rec := pruneTxn(t, fullPrunableRun(), nil)
	stmts := rec.Statements()

	insertAt := stmtIndex(stmts, "INSERT INTO run_summaries")
	deleteRunAt := stmtIndex(stmts, "DELETE FROM runs WHERE")
	if insertAt < 0 {
		t.Fatalf("no INSERT INTO run_summaries issued; a pruned run must leave its archival summary:\n%v", stmts)
	}
	if deleteRunAt < 0 {
		t.Fatalf("no DELETE FROM runs issued; the pruner must delete the run row:\n%v", stmts)
	}
	if insertAt >= deleteRunAt {
		t.Fatalf("summary INSERT at %d does not precede run DELETE at %d; the summary must be written before the run is pruned", insertAt, deleteRunAt)
	}
	if insertAt != 0 {
		t.Errorf("summary INSERT is statement %d, want the first statement (written before anything is deleted)", insertAt)
	}

	// Insert-only: no statement ever deletes (or updates) run_summaries.
	for _, s := range stmts {
		if !strings.Contains(s.SQL, "run_summaries") {
			continue
		}
		if !strings.Contains(s.SQL, "INSERT INTO run_summaries") {
			t.Errorf("run_summaries touched by a non-INSERT statement (it is insert-only, never pruned):\n%s", s.SQL)
		}
		if strings.Contains(s.SQL, "DELETE") || strings.Contains(s.SQL, "UPDATE") {
			t.Errorf("run_summaries is the target of a DELETE/UPDATE; it must be insert-only:\n%s", s.SQL)
		}
	}

	// The archived summary carries the run's exact fields and pin (its provenance
	// outlives the deleted run row).
	ins := stmts[insertAt]
	wantArgs := []any{
		int64(42), "load_orders", string(store.RunSucceeded), ptrStr("sha256:abc"),
		"decl-9f", "[7,11]", ptrStr("0/16B3F00"), ptrI64(1000), ptrI64(1200),
	}
	if !reflect.DeepEqual(ins.Args, wantArgs) {
		t.Errorf("summary INSERT args =\n  %#v\nwant\n  %#v", ins.Args, wantArgs)
	}
}

// TestPruneRunSummarySameTxn proves the archival summary is written inside the
// SAME meta transaction that deletes the run, so surviving references never
// dangle. The whole prune commits as exactly one atomic ExecTx batch that carries
// both the summary INSERT and the run DELETE; and an injected meta failure rolls
// the whole thing back -- recording nothing and deleting no log file -- so a
// failed prune leaves the run and its log intact for the next pass.
func TestPruneRunSummarySameTxn(t *testing.T) {
	t.Run("summary and delete commit as one transaction", func(t *testing.T) {
		rec := pruneTxn(t, fullPrunableRun(), nil)

		txns := rec.Transactions()
		if len(txns) != 1 {
			t.Fatalf("PruneRun issued %d transactions, want exactly one atomic meta transaction", len(txns))
		}
		batch := txns[0]
		if stmtIndex(batch, "INSERT INTO run_summaries") < 0 {
			t.Errorf("the atomic transaction does not write the archival summary:\n%v", batch)
		}
		if stmtIndex(batch, "DELETE FROM runs WHERE") < 0 {
			t.Errorf("the atomic transaction does not delete the run:\n%v", batch)
		}
		// Same txn = summary and delete are in the one batch (not split across two).
		if stmtIndex(batch, "INSERT INTO run_summaries") >= stmtIndex(batch, "DELETE FROM runs WHERE") {
			t.Errorf("within the transaction the summary INSERT must precede the run DELETE:\n%v", batch)
		}
	})

	t.Run("meta failure rolls back and deletes no log", func(t *testing.T) {
		rec := storetest.NewWriteRecorder()
		sentinel := errors.New("meta write failed")
		rec.FailTx(sentinel)
		w := store.NewWriter(rec)

		logDeleted := false
		err := w.PruneRun(context.Background(), fullPrunableRun(), func(string) error {
			logDeleted = true
			return nil
		})
		if !errors.Is(err, sentinel) {
			t.Fatalf("PruneRun error = %v, want the injected meta failure", err)
		}
		if len(rec.Statements()) != 0 {
			t.Errorf("a rolled-back prune recorded %d statements, want none (all-or-nothing)", len(rec.Statements()))
		}
		if logDeleted {
			t.Error("the run's log was deleted despite the meta transaction failing; the run still exists, so its log must survive")
		}
	})
}

// TestPruneRunCascadesInputsAndLog proves pruning a run cascades to its
// run_inputs rows and deletes the run's per-run log file. The atomic prune
// deletes the run's OWN consumption ledger rows (run_inputs.run_id = the pruned
// run); and, after the meta transaction commits, the run's log file is deleted
// through the supplied hook -- keyed by the run's decimal id, idempotent on an
// absent file.
func TestPruneRunCascadesInputsAndLog(t *testing.T) {
	run := fullPrunableRun()

	// A real per-run log file on disk, keyed by the run's decimal id, mirroring
	// the daemon's .iris/logs layout. The delete hook removes exactly that file.
	dir := t.TempDir()
	logPath := filepath.Join(dir, "run-"+strconv.FormatInt(run.RunID, 10)+".log")
	if err := os.WriteFile(logPath, []byte("run output\n"), 0o600); err != nil {
		t.Fatalf("seed log file: %v", err)
	}

	var gotRunID string
	rec := storetest.NewWriteRecorder()
	w := store.NewWriter(rec)
	deleteLog := func(runID string) error {
		gotRunID = runID
		// The log is deleted only AFTER the meta transaction has committed: by the
		// time the hook runs, the atomic prune batch is already recorded.
		if len(rec.Transactions()) != 1 {
			t.Errorf("log hook ran with %d committed transactions, want the prune committed first", len(rec.Transactions()))
		}
		p := filepath.Join(dir, "run-"+runID+".log")
		if err := os.Remove(p); err != nil && !errors.Is(err, os.ErrNotExist) {
			return err
		}
		return nil
	}
	if err := w.PruneRun(context.Background(), run, deleteLog); err != nil {
		t.Fatalf("PruneRun: %v", err)
	}

	// Cascade: the run's own run_inputs rows are deleted, scoped to the pruned run.
	batch := rec.Transactions()[0]
	inputsAt := stmtIndex(batch, "DELETE FROM run_inputs WHERE")
	if inputsAt < 0 {
		t.Fatalf("prune does not cascade to run_inputs:\n%v", batch)
	}
	if !reflect.DeepEqual(batch[inputsAt].Args, []any{run.RunID}) {
		t.Errorf("run_inputs cascade args = %v, want the pruned run id [%d] only", batch[inputsAt].Args, run.RunID)
	}

	// Log: the file is gone, and the hook was called with the run's decimal id.
	if gotRunID != strconv.FormatInt(run.RunID, 10) {
		t.Errorf("log hook run id = %q, want %q", gotRunID, strconv.FormatInt(run.RunID, 10))
	}
	if _, err := os.Stat(logPath); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("run log %s still present after prune (stat err = %v), want it deleted", logPath, err)
	}
}

// TestPinSurvivesPruning proves that BuildRunSummary copies all three snapshot
// pin values (snapshot_lsn, journal_floor, journal_ceiling) into the archival
// run_summary. This keeps the pin queryable after the run row is pruned
// (the provenance walk's summary fallback depends on it). It is pure logic,
// no I/O: the unit tier for the pin-survives contract.
func TestPinSurvivesPruning(t *testing.T) {
	t.Run("pin-survives-pruning", func(t *testing.T) {
		run := fullPrunableRun()
		// Ensure the input carries the pin (the test fixture does).
		if run.SnapshotLSN == nil || run.JournalFloor == nil || run.JournalCeiling == nil {
			t.Fatal("fixture PrunableRun must carry all three pin values for this contract")
		}
		sum := store.BuildRunSummary(run)
		if sum.SnapshotLSN == nil || *sum.SnapshotLSN != *run.SnapshotLSN {
			t.Errorf("summary snapshot_lsn = %v, want %v (pin must survive into summary)", sum.SnapshotLSN, run.SnapshotLSN)
		}
		if sum.JournalFloor == nil || *sum.JournalFloor != *run.JournalFloor {
			t.Errorf("summary journal_floor = %v, want %v (pin must survive into summary)", sum.JournalFloor, run.JournalFloor)
		}
		if sum.JournalCeiling == nil || *sum.JournalCeiling != *run.JournalCeiling {
			t.Errorf("summary journal_ceiling = %v, want %v (pin must survive into summary)", sum.JournalCeiling, run.JournalCeiling)
		}
	})
}
