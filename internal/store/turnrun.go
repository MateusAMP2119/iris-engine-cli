package store

import (
	"context"
	"fmt"
)

// This file is the turn-protocol run-record write surface (#206). Under the turn
// protocol a loop run's record is minted only when its turn produces something to
// record: a producing turn mints a RUNNING run just before its data transaction
// (so a crash between the two leaves a running row for reconciliation, never data
// without a record) and completes it with the turn's stamps after; a failed turn
// mints its run directly dead-lettered with the worklist row in the same atomic
// statement; a quiet turn mints nothing at all. Every method rides the single
// Writer like the rest of the run-record path.

// TurnRunRecord is the input to the turn-run mints: the run identity fields a
// turn carries when it records.
type TurnRunRecord struct {
	// Pipeline is the pipeline the turn ran (runs.pipeline).
	Pipeline string
	// Cause is why the turn ran (runs.cause): loop for a lane turn.
	Cause RunCause
	// DeclarationChecksum is the declaration hash the turn executed under.
	DeclarationChecksum string
	// ArtifactHash is the built binary's content hash, nil for a dev run.
	ArtifactHash *string
	// Handle is the resident process's group id (runs.handle).
	Handle int
	// LogRef is the per-run log reference (runs.log_ref), empty for none.
	LogRef string
	// ConsumedUpstreamRunIDs are the upstream runs the turn's gate resolved,
	// one run_inputs row each.
	ConsumedUpstreamRunIDs []int64
}

// createTurnRunSQL mints a producing turn's run directly in the running state,
// with its handle and log reference, and its consumption ledger, in one atomic
// CTE (the createRunSQL shape with the state and handle fixed at mint).
const createTurnRunSQL = `WITH new_run AS (
    INSERT INTO runs
        (pipeline, state, cause, artifact_hash, declaration_checksum, handle, log_ref, recorded_at)
    VALUES ($1, $2, $3, $4, $5, NULLIF($6, 0), NULLIF($7, ''), now()::text)
    RETURNING id
)
INSERT INTO run_inputs (run_id, upstream_run_id)
SELECT new_run.id, upstream
FROM new_run, unnest($8::bigint[]) AS upstream`

// completeTurnRunSQL closes a producing turn's run: the guarded running ->
// succeeded transition stamping exit code zero and the turn's snapshot pin
// (LSN, journal floor and ceiling) in one statement.
const completeTurnRunSQL = `UPDATE runs
SET state = $1, exit_code = 0, snapshot_lsn = $2, journal_floor = $3, journal_ceiling = $4
WHERE id = $5 AND state = $6`

// deadLetterTurnRunSQL mints a failed turn's run directly dead-lettered with its
// worklist row, one atomic CTE (the DeadLetterPropagated shape with the cause and
// reason carried by the turn).
const deadLetterTurnRunSQL = `WITH new_run AS (
    INSERT INTO runs
        (pipeline, state, cause, artifact_hash, declaration_checksum, handle, log_ref, recorded_at)
    VALUES ($1, $2, $3, $4, $5, NULLIF($6, 0), NULLIF($7, ''), now()::text)
    RETURNING id
), letter AS (
    INSERT INTO dead_letters (run_id, reason, error)
    SELECT id, $8, $9 FROM new_run
)
INSERT INTO run_inputs (run_id, upstream_run_id)
SELECT new_run.id, upstream
FROM new_run, unnest($10::bigint[]) AS upstream`

// CreateTurnRun mints a producing turn's run row directly in the running state
// with its consumption ledger, one atomic meta transaction. The caller commits
// the turn's data transaction next and completes the run after, so a crash
// between the two leaves a running run for the next leader's reconciliation.
func (w *Writer) CreateTurnRun(ctx context.Context, rec TurnRunRecord) error {
	var artifactHash any
	if rec.ArtifactHash != nil {
		artifactHash = *rec.ArtifactHash
	}
	stmts := []Statement{{
		SQL: createTurnRunSQL,
		Args: []any{
			rec.Pipeline,
			string(RunRunning),
			string(rec.Cause),
			artifactHash,
			rec.DeclarationChecksum,
			rec.Handle,
			rec.LogRef,
			rec.ConsumedUpstreamRunIDs,
		},
	}}
	if err := w.execTx(ctx, stmts); err != nil {
		return fmt.Errorf("store: writer create turn run for pipeline %q: %w", rec.Pipeline, err)
	}
	return nil
}

// CompleteTurnRun records a producing turn's successful terminal transition and
// its snapshot pin in one guarded statement: running -> succeeded, exit code
// zero, and the turn's LSN and journal window, so the whole terminal state is a
// single meta write.
func (w *Writer) CompleteTurnRun(ctx context.Context, id string, snapshotLSN string, journalFloor, journalCeiling int64) error {
	if err := w.conn.Exec(ctx, completeTurnRunSQL, RunSucceeded, snapshotLSN, journalFloor, journalCeiling, id, RunRunning); err != nil {
		return fmt.Errorf("store: writer complete turn run %s: %w", id, err)
	}
	return nil
}

// DeadLetterTurnRun mints a failed turn's run directly dead-lettered with its
// worklist row and consumption ledger, one atomic meta transaction: a failed
// turn always records (the dead-letter worklist is the product), even though a
// quiet turn records nothing.
func (w *Writer) DeadLetterTurnRun(ctx context.Context, rec TurnRunRecord, reason DeadLetterReason, detail string) error {
	var artifactHash any
	if rec.ArtifactHash != nil {
		artifactHash = *rec.ArtifactHash
	}
	var errDetail any
	if detail != "" {
		errDetail = detail
	}
	stmts := []Statement{{
		SQL: deadLetterTurnRunSQL,
		Args: []any{
			rec.Pipeline,
			string(RunDeadLettered),
			string(rec.Cause),
			artifactHash,
			rec.DeclarationChecksum,
			rec.Handle,
			rec.LogRef,
			string(reason),
			errDetail,
			rec.ConsumedUpstreamRunIDs,
		},
	}}
	if err := w.execTx(ctx, stmts); err != nil {
		return fmt.Errorf("store: writer dead-letter turn run for pipeline %q: %w", rec.Pipeline, err)
	}
	return nil
}
