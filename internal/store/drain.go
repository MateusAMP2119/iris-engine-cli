package store

import (
	"context"
	"fmt"
)

// This file is the drain write: the single-writer path that disposes of scoped
// dead-lettered runs by pure discard. Unlike replay, drain mints nothing and
// touches no other table: it deletes exactly the scoped runs' dead_letters
// worklist rows, one atomic statement, and nothing else -- no re-run, no
// downstream alteration. The run rows themselves stay in runs (a worklist exit
// never deletes run history); with no outstanding dead_letters entry left,
// retention (E05.9) reads the run as prunable, and -- because the removed entry
// was the run's only replay ticket -- the run can never be replayed again
// (ResolveReplayTargets finds no worklist entry for it and errors). The
// dispatcher resolves which run ids the operator's scope names
// (internal/dispatch, ResolveDrainTargets) and hands the resolved list here to
// write.

// drainDeadLettersSQL discards the dead_letters worklist rows for exactly the given
// run ids, scoped-only: no other row, and no other table, is touched by this
// statement.
const drainDeadLettersSQL = `DELETE FROM dead_letters WHERE run_id = ANY($1::bigint[])`

// DrainDeadLetters discards the dead_letters worklist rows for runIDs in one
// atomic statement (drain discards the outstanding entries within the given scope
// and no others). It is a pure discard: nothing re-runs (no runs insert), nothing
// downstream is altered (no run_inputs insert) -- the runs rows stay exactly as
// they were, so a drained run becomes prunable and can never be replayed
// afterward (its dead_letters entry, the only replay ticket, is gone). An empty
// runIDs issues no statement: draining nothing changes nothing. It is a
// leader-only meta write, riding the single Writer through the atomic ExecTx path
// (a connection without it fails loudly rather than splitting the drain across
// un-atomic Execs).
func (w *Writer) DrainDeadLetters(ctx context.Context, runIDs []int64) error {
	if len(runIDs) == 0 {
		return nil
	}
	stmts := []Statement{{SQL: drainDeadLettersSQL, Args: []any{runIDs}}}
	if err := w.execTx(ctx, stmts); err != nil {
		return fmt.Errorf("store: writer drain dead letters %v: %w", runIDs, err)
	}
	return nil
}
