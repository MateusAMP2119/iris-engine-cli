package store

import (
	"context"
	"fmt"
)

// This file is the run-record write surface: the leader-only path that mints a
// run row and stamps its journal window. Every method here rides the single
// Writer, so run records are serialized onto the one meta connection like every
// other meta write. It also carries the pure dead-letter classification: the
// mapping from a non-success ending to the single dead_lettered terminal state,
// which the dispatcher consults but which reads and writes nothing.

// RunCause is why a run was minted (runs.cause): a loop pass, an
// operator-requested run, a replay, or a propagated (never-executed) rejection.
// It is the closed enum the meta CHECK constraint pins.
type RunCause string

// The run causes (the runs.cause CHECK value set).
const (
	// CauseManual is an operator-requested run (iris pipeline run).
	CauseManual RunCause = "manual"
	// CauseLoop is a run started by a lane's ordinary loop pass.
	CauseLoop RunCause = "loop"
	// CauseReplay is a fresh run minted to replace a dead-lettered one.
	CauseReplay RunCause = "replay"
	// CausePropagated is a never-executed run written when an awaited upstream was
	// dead-lettered: the rejection propagates as a dead-lettered run of this cause.
	CausePropagated RunCause = "propagated"
)

// RunEnding is a run's non-success ending kind: the three ways a run can finish
// without succeeding. ClassifyEnding folds all three onto the single
// dead_lettered terminal state, each with its own worklist reason.
type RunEnding string

// The non-success endings.
const (
	// EndingFailed is a run that exited non-zero on its own.
	EndingFailed RunEnding = "failed"
	// EndingCancelled is a run the engine stopped rather than one that failed on its
	// own: an operator cancel, or a run left in flight when the daemon terminated.
	EndingCancelled RunEnding = "cancelled"
	// EndingUpstreamDeadLettered is a run rejected because an upstream it awaited was
	// dead-lettered (failure propagation along a depends_on edge).
	EndingUpstreamDeadLettered RunEnding = "upstream_dead_lettered"
)

// ClassifyEnding maps a non-success ending to the single dead_lettered terminal
// state and the dead-letter worklist reason it parks under (a failed,
// stopped/cancelled, or upstream-dead-lettered run all land in the one
// non-success terminal state). It is pure -- no reads, no writes -- and closed:
// an ending outside the three kinds is rejected rather than mapped to a default,
// so a run can never reach the terminal state by an unclassified path.
func ClassifyEnding(e RunEnding) (RunState, DeadLetterReason, error) {
	switch e {
	case EndingFailed:
		return RunDeadLettered, ReasonFailed, nil
	case EndingCancelled:
		return RunDeadLettered, ReasonStopped, nil
	case EndingUpstreamDeadLettered:
		return RunDeadLettered, ReasonUpstreamDeadLettered, nil
	default:
		return "", "", fmt.Errorf("store: classify ending: unknown non-success ending %q", e)
	}
}

// RunRecord is the input to CreateRun: everything the write path stamps onto a
// new run row and its consumption ledger. The run's id is assigned by meta's
// identity generator, never carried here; state is always queued at create.
type RunRecord struct {
	// Pipeline is the declared pipeline the run executes (runs.pipeline).
	Pipeline string
	// Cause is why the run was minted (runs.cause).
	Cause RunCause
	// DeclarationChecksum is the hash of the declaration the run executes
	// (runs.declaration_checksum), recorded on every run.
	DeclarationChecksum string
	// ArtifactHash is the built binary's content hash (runs.artifact_hash), or nil
	// for a dev run, which records SQL NULL.
	ArtifactHash *string
	// SnapshotLSN is the data-database LSN pinned at dispatch (runs.snapshot_lsn).
	SnapshotLSN string
	// JournalFloor is the journal high id at dispatch (runs.journal_floor), the low
	// edge of the run's journal window.
	JournalFloor int64
	// ReplayedFrom is the id of the dead-lettered run this run replaces
	// (runs.replayed_from), or nil when the run is not a replay.
	ReplayedFrom *int64
	// ConsumedUpstreamRunIDs are the upstream run ids this run consumes, one
	// run_inputs row each (written once at run start).
	ConsumedUpstreamRunIDs []int64
}

// createRunSQL mints a queued run and its consumption ledger as one atomic CTE: the
// runs insert returns the meta-assigned id, and the run_inputs insert feeds off that
// id, so the two commit together or not at all -- a run row can never exist without
// its consumed-upstream rows, nor those rows without their run. recorded_at is filled
// DB-side (now()::text) so no clock is read in the engine; it is an opaque audit
// string, never an ordering key. An empty upstream array unnests to zero rows, so a
// root run (no consumed upstreams) still creates its runs row. The positional arg
// order is pinned: pipeline, state, cause, replayed_from, artifact_hash,
// declaration_checksum, snapshot_lsn, journal_floor, upstream ids.
const createRunSQL = `WITH new_run AS (
    INSERT INTO runs
        (pipeline, state, cause, replayed_from, artifact_hash, declaration_checksum, snapshot_lsn, journal_floor, recorded_at)
    VALUES ($1, $2, $3, $4, $5, $6, $7, $8, now()::text)
    RETURNING id
)
INSERT INTO run_inputs (run_id, upstream_run_id)
SELECT new_run.id, upstream
FROM new_run, unnest($9::bigint[]) AS upstream`

// stampJournalCeilingSQL stamps the terminal edge of a run's journal window. It is a
// single guarded UPDATE: the WHERE journal_ceiling IS NULL clause makes it stamp
// once, so a re-issued terminal transition cannot overwrite an already-closed window.
const stampJournalCeilingSQL = `UPDATE runs SET journal_ceiling = $1 WHERE id = $2 AND journal_ceiling IS NULL`

// CreateRun mints a new queued run and its consumption ledger in one atomic meta
// transaction: the runs row records the run's cause, declaration checksum, binary
// hash (SQL NULL for a dev run), the snapshot pin (snapshot_lsn, journal_floor),
// and an optional replayed_from; the run_inputs rows record the consumed upstream
// run ids. Both commit together or not at all, so a run is never left
// half-recorded. It is a leader-only meta write, riding the single Writer through
// the atomic ExecTx path (a connection without it fails loudly rather than
// splitting the create across un-atomic Execs).
func (w *Writer) CreateRun(ctx context.Context, rec RunRecord) error {
	var replayedFrom any // nil -> SQL NULL for a non-replay run.
	if rec.ReplayedFrom != nil {
		replayedFrom = *rec.ReplayedFrom
	}
	var artifactHash any // nil -> SQL NULL for a dev run.
	if rec.ArtifactHash != nil {
		artifactHash = *rec.ArtifactHash
	}
	stmts := []Statement{{
		SQL: createRunSQL,
		Args: []any{
			rec.Pipeline,
			string(RunQueued),
			string(rec.Cause),
			replayedFrom,
			artifactHash,
			rec.DeclarationChecksum,
			rec.SnapshotLSN,
			rec.JournalFloor,
			rec.ConsumedUpstreamRunIDs,
		},
	}}
	if err := w.execTx(ctx, stmts); err != nil {
		return fmt.Errorf("store: writer create run for pipeline %q: %w", rec.Pipeline, err)
	}
	return nil
}

// StampJournalCeiling records the terminal edge of a run's journal window: the
// journal high id read at the run's terminal transition. The UPDATE is guarded on
// journal_ceiling IS NULL so the window is closed exactly once. It is a
// leader-only meta write, riding the single Writer.
func (w *Writer) StampJournalCeiling(ctx context.Context, runID string, ceiling int64) error {
	if err := w.conn.Exec(ctx, stampJournalCeilingSQL, ceiling, runID); err != nil {
		return fmt.Errorf("store: writer stamp journal ceiling for run %s: %w", runID, err)
	}
	return nil
}
