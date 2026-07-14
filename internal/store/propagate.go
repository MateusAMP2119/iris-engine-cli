package store

import (
	"context"
	"fmt"
)

// This file is the failure-propagation write: the single-writer path that
// dead-letters a downstream whose awaited upstream run was itself dead-lettered.
// Unlike DeadLetterRun (writer.go), which transitions an existing run that failed
// or was stopped, propagation MINTS the downstream a fresh run it never executed:
// the rejection along a depends_on edge is recorded as a dead-lettered run of
// cause=propagated. The dispatcher computes the plan lazily at the downstream's
// consumption time (internal/dispatch, PlanPropagation) and hands it here to
// write.

// PropagatedRun is the input to DeadLetterPropagated: everything the write path
// stamps onto a downstream's never-executed, propagation-dead-lettered run and
// its worklist and lineage rows. The run's id is assigned by meta's identity
// generator, never carried here; its state (dead_lettered), cause (propagated),
// and dead-letter reason (upstream_dead_lettered) are fixed by the write, not the
// caller.
type PropagatedRun struct {
	// Pipeline is the downstream pipeline being dead-lettered by propagation
	// (runs.pipeline).
	Pipeline string
	// DeclarationChecksum is the hash of the declaration the run would have executed
	// (runs.declaration_checksum), recorded on every run including a never-executed one.
	DeclarationChecksum string
	// FailedUpstream is the immediate upstream pipeline whose dead-lettered run
	// propagated (dead_letters.failed_upstream): the immediate upstream, never the root
	// of a transitive chain.
	FailedUpstream string
	// PoisonedUpstreamRunIDs are the dead-lettered upstream run ids the propagation
	// records in run_inputs, one row each (complete lineage): the run(s) the rejection
	// propagated from.
	PoisonedUpstreamRunIDs []int64
	// Detail is an optional human-readable error for dead_letters.error; empty records
	// SQL NULL (the machine attribution rides reason and failed_upstream, not this).
	Detail string
}

// deadLetterPropagatedSQL mints the downstream's never-executed dead-lettered run and
// its worklist and lineage rows as ONE atomic CTE. new_run inserts the runs row --
// state dead_lettered, cause propagated, no exit_code/handle/snapshot pin, since the
// run never executed -- and returns the meta-assigned id; dead (a data-modifying CTE,
// executed to completion whether or not the primary query reads it) inserts the
// dead_letters worklist row against that id with reason upstream_dead_lettered and the
// immediate failed_upstream; the primary INSERT records one run_inputs row per poisoned
// upstream run off the same id. All three commit together or not at all, so a
// propagated dead-letter can never be left without its worklist row or its lineage.
// recorded_at is filled DB-side (now()::text); an empty upstream array unnests to zero
// run_inputs rows. The positional arg order is pinned: pipeline, state, cause,
// declaration_checksum, reason, failed_upstream, error, poisoned upstream ids.
const deadLetterPropagatedSQL = `WITH new_run AS (
    INSERT INTO runs
        (pipeline, state, cause, declaration_checksum, recorded_at)
    VALUES ($1, $2, $3, $4, now()::text)
    RETURNING id
),
dead AS (
    INSERT INTO dead_letters (run_id, reason, failed_upstream, error)
    SELECT id, $5, $6, $7 FROM new_run
)
INSERT INTO run_inputs (run_id, upstream_run_id)
SELECT new_run.id, upstream
FROM new_run, unnest($8::bigint[]) AS upstream`

// DeadLetterPropagated mints a downstream a never-executed dead-lettered run and
// its dead_letters and run_inputs rows in one atomic meta transaction: the run
// records cause propagated and state dead_lettered with no exit code (it never
// ran); the dead_letters row records reason upstream_dead_lettered and the
// immediate failed_upstream; and run_inputs records the poisoned upstream run(s)
// for complete lineage. All three commit together or not at all, so the
// propagated dead-letter is never left half-recorded. It is a leader-only meta
// write, riding the single Writer through the atomic ExecTx path (a connection
// without it fails loudly rather than splitting the write across un-atomic
// Execs).
func (w *Writer) DeadLetterPropagated(ctx context.Context, rec PropagatedRun) error {
	var errDetail any // nil -> SQL NULL when no human detail is supplied.
	if rec.Detail != "" {
		errDetail = rec.Detail
	}
	stmts := []Statement{{
		SQL: deadLetterPropagatedSQL,
		Args: []any{
			rec.Pipeline,
			string(RunDeadLettered),
			string(CausePropagated),
			rec.DeclarationChecksum,
			string(ReasonUpstreamDeadLettered),
			rec.FailedUpstream,
			errDetail,
			rec.PoisonedUpstreamRunIDs,
		},
	}}
	if err := w.execTx(ctx, stmts); err != nil {
		return fmt.Errorf("store: writer dead-letter propagated run for pipeline %q: %w", rec.Pipeline, err)
	}
	return nil
}
