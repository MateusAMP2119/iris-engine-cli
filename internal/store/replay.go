package store

import (
	"context"
	"fmt"
)

// This file is the replay write: the single-writer path that disposes of a
// dead-lettered run by minting its replacement (specification section 6.2). Replay
// is a fresh run on current data through the normal run path -- like CreateRun it
// stamps cause, checksum, the snapshot pin, and the consumed upstream runs -- with
// two additions that make it a replay: replayed_from points at the run it replaces
// (replay lineage, never parenthood), and the replaced run's dead_letters worklist
// row is removed in the same statement, so "the replacement mints" and "the worklist
// exits" are one atomic act. The dispatcher resolves which root causes to replay
// (internal/dispatch, ResolveReplayTargets) and hands each here to write.

// ReplayRecord is the input to ReplayRun: what the write path stamps onto the fresh
// replacement run and its consumption ledger, plus the dead-lettered run it replaces.
// The replacement's id is assigned by meta's identity generator (so it becomes the
// pipeline's most recent run), its state is always queued at mint, and its cause is
// always replay -- none of those are the caller's to set.
type ReplayRecord struct {
	// ReplacedRunID is the dead-lettered run this replay replaces: the replacement's
	// runs.replayed_from, and the dead_letters row removed when the replacement mints.
	ReplacedRunID int64
	// Pipeline is the declared pipeline the replacement executes (runs.pipeline).
	Pipeline string
	// DeclarationChecksum is the hash of the declaration the replacement executes
	// (runs.declaration_checksum), recorded on every run.
	DeclarationChecksum string
	// ArtifactHash is the built binary's content hash (runs.artifact_hash), or nil for
	// a dev run, which records SQL NULL.
	ArtifactHash *string
	// SnapshotLSN is the data-database LSN pinned at dispatch (runs.snapshot_lsn): the
	// replacement runs on CURRENT data, so this is read fresh, not copied from the
	// replaced run.
	SnapshotLSN string
	// JournalFloor is the journal high id at dispatch (runs.journal_floor), the low
	// edge of the replacement's journal window.
	JournalFloor int64
	// ConsumedUpstreamRunIDs are the upstream run ids the replacement consumes on
	// current data, one run_inputs row each (written once at run start).
	ConsumedUpstreamRunIDs []int64
}

// replayRunSQL mints the replacement run, removes the replaced run's worklist row,
// and writes the consumption ledger as ONE atomic CTE. new_run inserts the runs row
// -- state queued, cause replay, replayed_from the replaced run, the snapshot pin, no
// explicit id so meta assigns the identity (the replacement is the most recent run)
// -- and returns the id; removed (a data-modifying CTE, executed to completion
// whether or not the primary query reads it) deletes the replaced run's dead_letters
// entry; the primary INSERT records one run_inputs row per consumed upstream off the
// new id. All three commit together or not at all, so the replacement never exists
// beside the entry it should have cleared, nor the entry after the replacement mints.
// recorded_at is filled DB-side (now()::text); an empty upstream array unnests to
// zero run_inputs rows. The positional arg order is pinned: pipeline, state, cause,
// replayed_from, artifact_hash, declaration_checksum, snapshot_lsn, journal_floor,
// replaced run id (worklist key), consumed upstream ids.
const replayRunSQL = `WITH new_run AS (
    INSERT INTO runs
        (pipeline, state, cause, replayed_from, artifact_hash, declaration_checksum, snapshot_lsn, journal_floor, recorded_at)
    VALUES ($1, $2, $3, $4, $5, $6, $7, $8, now()::text)
    RETURNING id
),
removed AS (
    DELETE FROM dead_letters WHERE run_id = $9
)
INSERT INTO run_inputs (run_id, upstream_run_id)
SELECT new_run.id, upstream
FROM new_run, unnest($10::bigint[]) AS upstream`

// ReplayRun mints a dead-lettered run's replacement and removes the run's worklist
// row in one atomic meta transaction (specification section 6.2): the replacement is
// a fresh queued run on current data with cause replay and replayed_from the replaced
// run, its consumed upstreams recorded in run_inputs, and the replaced run's
// dead_letters entry deleted -- the replacement mints, the worklist exits, together.
// The replaced run row itself stays in runs (a worklist exit never deletes run
// history). It is a leader-only meta write, riding the single Writer through the
// atomic ExecTx path (a connection without it fails loudly rather than splitting the
// replay across un-atomic Execs).
func (w *Writer) ReplayRun(ctx context.Context, rec ReplayRecord) error {
	var artifactHash any // nil -> SQL NULL for a dev run.
	if rec.ArtifactHash != nil {
		artifactHash = *rec.ArtifactHash
	}
	stmts := []Statement{{
		SQL: replayRunSQL,
		Args: []any{
			rec.Pipeline,
			string(RunQueued),
			string(CauseReplay),
			rec.ReplacedRunID,
			artifactHash,
			rec.DeclarationChecksum,
			rec.SnapshotLSN,
			rec.JournalFloor,
			rec.ReplacedRunID,
			rec.ConsumedUpstreamRunIDs,
		},
	}}
	if err := w.execTx(ctx, stmts); err != nil {
		return fmt.Errorf("store: writer replay run replacing %d for pipeline %q: %w", rec.ReplacedRunID, rec.Pipeline, err)
	}
	return nil
}
