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

// TestReplayRunMintsFreshRunAndExitsWorklist proves the replay write (specification
// section 6.2): a replay mints a FRESH run on current data through the normal run
// path -- state queued, cause replay, replayed_from set to the replaced dead-lettered
// run -- and REMOVES the replaced run's dead_letters worklist row, both in ONE atomic
// transaction (a single CTE). The replacement mints, so the worklist exits together
// with the mint; there is no window where the replaced entry lingers beside its fresh
// replacement, and none where a fresh run exists without the replaced entry cleared.
// The fresh run carries a new meta-assigned identity id, so it becomes the pipeline's
// most recent run.
//
// spec: S06.2/replay-fresh-run-record
func TestReplayRunMintsFreshRunAndExitsWorklist(t *testing.T) {
	rec := storetest.NewWriteRecorder()
	w := store.NewWriter(rec)

	hash := "sha256:built"
	err := w.ReplayRun(context.Background(), store.ReplayRecord{
		ReplacedRunID:          10,
		Pipeline:               "extract_orders",
		DeclarationChecksum:    "sha256:decl",
		ArtifactHash:           &hash,
		SnapshotLSN:            "0/16B3748",
		JournalFloor:           4242,
		ConsumedUpstreamRunIDs: []int64{7, 8},
	})
	if err != nil {
		t.Fatalf("ReplayRun: %v", err)
	}

	// One atomic transaction of one CTE: the mint, the worklist removal, and the
	// consumption ledger commit together or not at all.
	txns := rec.Transactions()
	if len(txns) != 1 || len(txns[0]) != 1 {
		t.Fatalf("ReplayRun issued %d transactions / %d statements, want one atomic CTE", len(txns), lenFirst(txns))
	}
	stmt := txns[0][0]
	sql := stmt.SQL

	// The fresh run is minted, the replaced entry removed, and the consumption ledger
	// written -- one statement.
	if !strings.Contains(sql, "INSERT INTO runs") {
		t.Errorf("replay write does not INSERT the fresh run:\n%s", sql)
	}
	if !strings.Contains(sql, "DELETE FROM dead_letters") {
		t.Errorf("replay write does not DELETE the replaced worklist entry (the replacement mints -> worklist exit):\n%s", sql)
	}
	if !strings.Contains(sql, "INSERT INTO run_inputs") {
		t.Errorf("replay write does not INSERT the run_inputs consumption ledger:\n%s", sql)
	}

	// A fresh run through the normal path: minted queued, cause replay, no explicit id
	// (meta assigns the identity, so the run becomes the most recent).
	if strings.Contains(sql, "(id,") || strings.Contains(sql, "(id ") {
		t.Errorf("replay write sets an explicit run id; the identity must be meta-assigned so the run is the most recent:\n%s", sql)
	}
	if !argsContain(stmt.Args, string(store.RunQueued)) {
		t.Errorf("replay run state is not queued; args = %v", stmt.Args)
	}
	if !argsContain(stmt.Args, string(store.CauseReplay)) {
		t.Errorf("replay run cause is not replay; args = %v", stmt.Args)
	}

	// replayed_from is the replaced run: replay lineage, so the fresh run points back
	// at the dead-lettered run it replaces (never parenthood).
	if !argsContain(stmt.Args, int64(10)) {
		t.Errorf("replay write does not carry the replaced run id 10 (replayed_from + worklist key); args = %v", stmt.Args)
	}

	// The pin and checksum ride the mint; the consumed upstream ids flow as the
	// one-row-per-upstream unnest, the last positional arg (current-data consumption).
	if !argsContain(stmt.Args, "sha256:decl") {
		t.Errorf("replay write omits the declaration checksum; args = %v", stmt.Args)
	}
	if !strings.Contains(sql, "unnest") {
		t.Errorf("run_inputs write is not the one-row-per-upstream unnest form:\n%s", sql)
	}
	if got := stmt.Args[len(stmt.Args)-1]; !reflect.DeepEqual(got, []int64{7, 8}) {
		t.Errorf("consumed upstream ids arg = %v, want the current-data consumption [7 8]", got)
	}
}

// TestReplayRunAtomicRollback proves the replay write is all-or-nothing: an injected
// meta failure rolls the whole CTE back, so a failed replay leaves neither a fresh run
// without its worklist removal nor a removed entry without its replacement. The single
// writer fails loudly rather than splitting the change across un-atomic Execs.
//
// spec: S06.2/replay-fresh-run-record
func TestReplayRunAtomicRollback(t *testing.T) {
	rec := storetest.NewWriteRecorder()
	sentinel := errors.New("meta write failed")
	rec.FailTx(sentinel)
	w := store.NewWriter(rec)

	err := w.ReplayRun(context.Background(), store.ReplayRecord{
		ReplacedRunID:       10,
		Pipeline:            "extract_orders",
		DeclarationChecksum: "sha256:decl",
		SnapshotLSN:         "0/16B3748",
		JournalFloor:        4242,
	})
	if err == nil {
		t.Fatal("ReplayRun did not report the injected meta failure")
	}
	if !errors.Is(err, sentinel) {
		t.Errorf("ReplayRun error = %v, want it to wrap the injected failure", err)
	}
	// Atomic rollback: nothing was recorded.
	if txns := rec.Transactions(); len(txns) != 0 {
		t.Errorf("a failed replay recorded %d transactions, want none (atomic rollback)", len(txns))
	}
	if stmts := rec.Statements(); len(stmts) != 0 {
		t.Errorf("a failed replay recorded %d statements, want none (atomic rollback)", len(stmts))
	}
}

// TestReplayRunDevRunNullArtifact proves a dev-run replay records a null artifact
// hash (nil ArtifactHash -> SQL NULL), mirroring CreateRun: replay is a fresh run
// through the normal path, so a dev pipeline's replacement carries no binary hash.
//
// spec: S06.2/replay-fresh-run-record
func TestReplayRunDevRunNullArtifact(t *testing.T) {
	rec := storetest.NewWriteRecorder()
	w := store.NewWriter(rec)

	if err := w.ReplayRun(context.Background(), store.ReplayRecord{
		ReplacedRunID:       10,
		Pipeline:            "extract_orders",
		DeclarationChecksum: "sha256:decl",
		SnapshotLSN:         "0/16B3748",
		JournalFloor:        4242,
	}); err != nil {
		t.Fatalf("ReplayRun: %v", err)
	}
	stmt := rec.Transactions()[0][0]
	// A nil artifact hash is recorded as a SQL NULL arg, never the empty string.
	if argsContain(stmt.Args, "") {
		t.Errorf("dev-run replay recorded an empty-string artifact hash; want SQL NULL; args = %v", stmt.Args)
	}
}
