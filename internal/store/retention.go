package store

import (
	"context"
	"fmt"
	"strconv"
	"strings"
)

// This file is the retention write surface: the leader-only path that archives a
// run into run_summaries and then prunes it. A pruned run first leaves a compact,
// FK-free archival summary -- run_summaries outlives everything it summarizes --
// written in the SAME meta transaction that deletes the run and cascades its
// run_inputs rows, so a surviving reference resolves to a run row or its summary,
// never to a hole. run_summaries is insert-only and never touched by a delete;
// the data journal is never touched (capture rows are bounded by the journal's
// own lifecycle, not by run pruning). The run's per-run log file is deleted after
// the meta transaction commits, through a caller-supplied hook so this leaf
// package never reaches into the daemon's log layout.

// PrunableRun is the full run record the pruner archives before deleting it:
// exactly the fields run_summaries preserves. It is the pruner's input, read from
// the run row being pruned; BuildRunSummary copies it into the archival shape.
type PrunableRun struct {
	// RunID is the run's meta identity (runs.id), the summary's primary key.
	RunID int64
	// Pipeline is the run's pipeline name, copied into the summary (not an FK: the
	// summary outlives the pipeline row).
	Pipeline string
	// State is the run's terminal state (runs.state).
	State RunState
	// ArtifactHash is the built binary's content hash, or nil for a dev run (SQL NULL).
	ArtifactHash *string
	// DeclarationChecksum is the hash of the declaration the run executed.
	DeclarationChecksum string
	// ConsumedUpstreamRunIDs are the upstream run ids the run consumed (its run_inputs
	// rows), preserved in the summary as a JSON array.
	ConsumedUpstreamRunIDs []int64
	// SnapshotLSN is the snapshot pin's data-database LSN, or nil (SQL NULL).
	SnapshotLSN *string
	// JournalFloor is the snapshot pin's journal low edge, or nil (SQL NULL).
	JournalFloor *int64
	// JournalCeiling is the pin's journal high edge at terminal transition, or nil.
	JournalCeiling *int64
}

// RunSummary is the archival-tier row the pruner writes before deleting a run
// (run_summaries): the pin and provenance that outlive pruning.
// consumed_upstream_run_ids is JSON, so it survives without a run_inputs FK.
// recorded_at is not carried here: it is stamped DB-side (now()::text) at insert,
// so no clock is read in the engine.
type RunSummary struct {
	// RunID is the summarized run's identity (run_summaries.run_id PK).
	RunID int64
	// Pipeline is the run's pipeline name, copied (not an FK).
	Pipeline string
	// State is the run's terminal state.
	State RunState
	// ArtifactHash is the built binary's content hash, or nil (SQL NULL).
	ArtifactHash *string
	// DeclarationChecksum is the declaration hash the run executed.
	DeclarationChecksum string
	// ConsumedUpstreamRunIDsJSON is the consumed upstream run ids as a JSON array
	// literal ("[]" when none), the value written into the json column.
	ConsumedUpstreamRunIDsJSON string
	// SnapshotLSN is the snapshot pin's LSN, or nil (SQL NULL).
	SnapshotLSN *string
	// JournalFloor is the pin's journal low edge, or nil (SQL NULL).
	JournalFloor *int64
	// JournalCeiling is the pin's journal high edge, or nil (SQL NULL).
	JournalCeiling *int64
}

// BuildRunSummary copies a run being pruned into its archival summary: run_id,
// pipeline (copied, never an FK), state, artifact_hash, declaration_checksum, the
// consumed upstream run ids as a JSON array, and the snapshot pin (snapshot_lsn /
// journal_floor / journal_ceiling -- the input state that survives pruning). It
// is pure: no reads, no writes, no clock. An empty or nil consumed-upstreams set
// becomes the JSON empty array "[]" (never "null"), so the column is always a
// well-formed array a provenance query can read.
func BuildRunSummary(run PrunableRun) RunSummary {
	return RunSummary{
		RunID:                      run.RunID,
		Pipeline:                   run.Pipeline,
		State:                      run.State,
		ArtifactHash:               run.ArtifactHash,
		DeclarationChecksum:        run.DeclarationChecksum,
		ConsumedUpstreamRunIDsJSON: marshalRunIDs(run.ConsumedUpstreamRunIDs),
		SnapshotLSN:                run.SnapshotLSN,
		JournalFloor:               run.JournalFloor,
		JournalCeiling:             run.JournalCeiling,
	}
}

// The retention write statements. Each is a single parameterized statement;
// pruneStatements groups them into the one atomic batch PruneRun submits.
const (
	// insertRunSummarySQL archives one run into the FK-free, insert-only
	// run_summaries tier. consumed_upstream_run_ids is a JSON array literal (cast to
	// json); recorded_at is filled DB-side (now()::text) so no clock is read in the
	// engine. The positional arg order is pinned: run_id, pipeline, state,
	// artifact_hash, declaration_checksum, consumed ids JSON, snapshot_lsn,
	// journal_floor, journal_ceiling.
	insertRunSummarySQL = `INSERT INTO run_summaries
    (run_id, pipeline, state, artifact_hash, declaration_checksum, consumed_upstream_run_ids, snapshot_lsn, journal_floor, journal_ceiling, recorded_at)
VALUES ($1, $2, $3, $4, $5, $6::json, $7, $8, $9, now()::text)`
	// deleteRunInputsSQL cascades to the pruned run's OWN consumption ledger rows (the
	// upstreams it consumed). Run pruning removes the run's own inputs; it never
	// deletes a surviving downstream's ledger row (that would erase a live run's
	// provenance and re-open its gate).
	deleteRunInputsSQL = `DELETE FROM run_inputs WHERE run_id = $1`
	// deleteRunSQL removes the run row itself -- last, after its summary is written
	// and its run_inputs cascaded.
	deleteRunSQL = `DELETE FROM runs WHERE id = $1`
)

// pruneStatements builds the atomic batch that prunes one run: the archival
// summary INSERT first, then the run's run_inputs cascade, then the run DELETE.
// The order is load-bearing -- the summary is written before the run is deleted
// (so a surviving reference never dangles), and run_inputs is deleted before runs
// (the ledger rows reference the run row). It is pure and closed: the batch names
// only run_summaries, run_inputs, and runs -- never data_journal (capture rows
// are bounded by the journal's own lifecycle) and never a DELETE against the
// insert-only run_summaries. PruneRun submits the whole batch as one meta
// transaction.
func pruneStatements(s RunSummary) []Statement {
	return []Statement{
		{SQL: insertRunSummarySQL, Args: []any{
			s.RunID, s.Pipeline, string(s.State), s.ArtifactHash, s.DeclarationChecksum,
			s.ConsumedUpstreamRunIDsJSON, s.SnapshotLSN, s.JournalFloor, s.JournalCeiling,
		}},
		{SQL: deleteRunInputsSQL, Args: []any{s.RunID}},
		{SQL: deleteRunSQL, Args: []any{s.RunID}},
	}
}

// PruneRun archives a run and prunes it: it writes the run's compact archival
// summary into run_summaries and, in the SAME meta transaction, deletes the run's
// run_inputs rows and the run row itself -- so a pruned run always leaves its
// summary and a surviving reference resolves to a run or its summary, never a
// hole. run_summaries is insert-only and never touched by a delete, and the data
// journal is never touched. After the meta transaction commits, the run's per-run
// log file is deleted through deleteLog (the daemon's RunLogWriter.DeleteOnPrune,
// passed in so this leaf package never imports the daemon; an already-absent log
// is not an error, so the delete is idempotent). deleteLog may be nil, in which
// case no log deletion is attempted.
//
// It is a leader-only meta write, riding the single Writer through the atomic ExecTx
// path (a connection without it fails loudly rather than splitting the prune across
// un-atomic Execs). On a meta failure nothing is deleted -- the run and its log
// survive intact for the next pass; the log is removed only once the run row is
// durably gone.
func (w *Writer) PruneRun(ctx context.Context, run PrunableRun, deleteLog func(runID string) error) error {
	if err := w.execTx(ctx, pruneStatements(BuildRunSummary(run))); err != nil {
		return fmt.Errorf("store: writer prune run %d: %w", run.RunID, err)
	}
	if deleteLog != nil {
		if err := deleteLog(runIDString(run.RunID)); err != nil {
			return fmt.Errorf("store: writer prune run %d: delete log: %w", run.RunID, err)
		}
	}
	return nil
}

// marshalRunIDs renders run ids as a JSON array literal, normalizing nil/empty to
// "[]" so a root run (no consumed upstreams) still records a well-formed empty array.
// It builds the array directly from decimal integers, so it is total -- there is no
// marshaling error to swallow.
func marshalRunIDs(ids []int64) string {
	var b strings.Builder
	b.WriteByte('[')
	for i, id := range ids {
		if i > 0 {
			b.WriteByte(',')
		}
		b.WriteString(strconv.FormatInt(id, 10))
	}
	b.WriteByte(']')
	return b.String()
}

// runIDString renders a run id in its canonical decimal form, the per-run log key
// RunLogWriter records and DeleteOnPrune removes.
func runIDString(id int64) string {
	return strconv.FormatInt(id, 10)
}
