package store

import (
	"context"
	"fmt"
)

// This file is the meta read path for the run-history lineage view: the
// plain-MVCC reads GET /runs[?include=inputs] and GET /runs/{id} draw from, and
// therefore what `iris run list` renders as the lineage rail. Like every reader
// here it runs over the pool with no session pinning and no busy-retry, so
// listing run history never contends with the leader's single-writer path.
//
// Each row carries the run and, for the include=inputs view, its consumed
// upstream run ids and replayed_from as plain attributes (parents-per-row, never
// a separate edge array). The consumed upstream ids come straight off run_inputs,
// which is FK-free (PR #101): an upstream id may name a run since pruned, so it
// is returned verbatim -- the rail renderer's gap to draw -- never joined away
// against a live run.

// RunLineage is one run of the history view as the runs collection consumes it:
// the run, its pipeline and state, its consumed upstream run ids (the run_inputs
// ledger, each a solid rail edge), and the run it replaced (replayed_from, an
// annotation never an edge). ReplayedFrom is nil when the run is not a replay.
type RunLineage struct {
	// ID is the run's meta id (runs.id).
	ID int64
	// Pipeline is the run's pipeline (runs.pipeline).
	Pipeline string
	// State is the run's lifecycle state (runs.state).
	State RunState
	// ReplayedFrom is the id of the dead-lettered run this run replaced
	// (runs.replayed_from), or nil when the run is not a replay.
	ReplayedFrom *int64
	// Inputs are the consumed upstream run ids (run_inputs.upstream_run_id for this
	// run), ascending. FK-free: an id may name a pruned run, carried verbatim.
	Inputs []int64
}

// RunLineageReader is the plain-MVCC read seam the runs history plane draws from:
// the whole run history newest-first, or a single run by id, each with its consumed
// upstream ids and replayed_from. A pgx-pool-backed implementation and a fake both
// satisfy it; reads are never serialized through the writer and never retried.
type RunLineageReader interface {
	// RunLineages returns the whole run history, newest first (descending id), each
	// with its consumed upstream ids and replayed_from.
	RunLineages(ctx context.Context) ([]RunLineage, error)
	// RunLineage returns a single run by id and whether it exists, with the same
	// consumed-upstream and replayed_from attributes.
	RunLineage(ctx context.Context, id int64) (RunLineage, bool, error)
}

// SQL the run-lineage reader issues. Each is a plain SELECT over an MVCC snapshot,
// no locking clause. The consumed upstream ids ride as a correlated array aggregate
// (empty array, never null, when the run consumed nothing), so one query returns a
// run and its whole parents-per-row lineage.
const (
	// runLineageColumns is the shared projection: the run row plus its ascending
	// consumed-upstream array. run_inputs is FK-free, so the array is read straight
	// off the ledger without joining the upstream ids to live runs.
	runLineageColumns = `SELECT r.id, r.pipeline, r.state, r.replayed_from,
    COALESCE((SELECT array_agg(ri.upstream_run_id ORDER BY ri.upstream_run_id)
              FROM run_inputs ri WHERE ri.run_id = r.id), '{}'::bigint[]) AS inputs
FROM runs r`

	// runLineagesSQL reads the whole history newest first: the rail renderer draws
	// newest-at-top, and run-id gaps stay visible (ids never renumber).
	runLineagesSQL = runLineageColumns + `
ORDER BY r.id DESC`

	// runLineageByIDSQL reads one run by id.
	runLineageByIDSQL = runLineageColumns + `
WHERE r.id = $1`
)

// pgxRunLineageReader is the pgx-pool-backed RunLineageReader.
type pgxRunLineageReader struct {
	pool readPool
}

// compile-time proof the pgx adapter satisfies the run-lineage read seam.
var _ RunLineageReader = (*pgxRunLineageReader)(nil)

// newPgxRunLineageReader builds a run-lineage reader over a pooled-query seam.
func newPgxRunLineageReader(pool readPool) *pgxRunLineageReader {
	return &pgxRunLineageReader{pool: pool}
}

// RunLineages reads the whole run history in one plain MVCC query, newest first.
func (r *pgxRunLineageReader) RunLineages(ctx context.Context) ([]RunLineage, error) {
	rows, err := r.pool.query(ctx, runLineagesSQL)
	if err != nil {
		return nil, fmt.Errorf("store: read run lineages: %w", err)
	}
	defer rows.Close()

	var out []RunLineage
	for rows.Next() {
		rl, err := scanRunLineage(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, rl)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: read run lineages: %w", err)
	}
	return out, nil
}

// RunLineage reads a single run by id in one plain MVCC query, reporting whether
// it exists.
func (r *pgxRunLineageReader) RunLineage(ctx context.Context, id int64) (RunLineage, bool, error) {
	rows, err := r.pool.query(ctx, runLineageByIDSQL, id)
	if err != nil {
		return RunLineage{}, false, fmt.Errorf("store: read run lineage %d: %w", id, err)
	}
	defer rows.Close()

	if !rows.Next() {
		if err := rows.Err(); err != nil {
			return RunLineage{}, false, fmt.Errorf("store: read run lineage %d: %w", id, err)
		}
		return RunLineage{}, false, nil
	}
	rl, err := scanRunLineage(rows)
	if err != nil {
		return RunLineage{}, false, err
	}
	return rl, true, nil
}

// scanRunLineage maps one row of the shared projection into a RunLineage: id,
// pipeline, state, the nullable replayed_from, and the consumed-upstream array.
func scanRunLineage(rows poolRows) (RunLineage, error) {
	var rl RunLineage
	var state string
	if err := rows.Scan(&rl.ID, &rl.Pipeline, &state, &rl.ReplayedFrom, &rl.Inputs); err != nil {
		return RunLineage{}, fmt.Errorf("store: scan run lineage row: %w", err)
	}
	rl.State = RunState(state)
	return rl, nil
}
