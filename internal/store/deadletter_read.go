package store

import (
	"context"
	"fmt"
)

// This file is the meta read path for the dead-letter worklist: the plain-MVCC
// reads replay resolution and the blast-radius readout draw from. Like every
// reader here it runs over the pool with no session pinning and no busy-retry, so
// `iris deadletter show` (a read served on any node) and the leader's replay
// resolution both read a consistent snapshot without contending with the
// single-writer path.
//
// The worklist row the dispatcher's pure resolution needs carries more than the stats
// projection: besides the run, pipeline, and reason it needs -- for a propagated entry
// -- the immediate poisoned upstream run recorded in run_inputs, so the failed_upstream
// root walk (dispatch.ResolveReplayTargets, dispatch.ClassifyBlastRadius) can resolve a
// propagated entry to its root cause.

// DeadLetterWorklistEntry is one outstanding worklist row as replay and blast-radius
// resolution read it: the run, its pipeline, its reason, and -- for a propagated entry
// -- the immediate poisoned upstream run recorded in run_inputs (zero for a root cause,
// which propagated from nothing).
type DeadLetterWorklistEntry struct {
	// RunID is the dead-lettered run this entry parks.
	RunID int64
	// Pipeline is the run's pipeline.
	Pipeline string
	// Reason is the worklist entry's reason (the dead_letters.reason set).
	Reason DeadLetterReason
	// FailedUpstreamRunID is the immediate upstream dead-lettered run a propagated
	// entry propagated from (the poisoned run recorded in run_inputs); zero for a root
	// cause.
	FailedUpstreamRunID int64
}

// ConsumptionFact is one recorded consumption edge with both endpoints' pipelines: a
// dependent run consumed an upstream run. The blast radius reads these to decide
// shielding -- a dependent that has consumed an upstream run later than a poisoned one
// is no longer poisoned by it.
type ConsumptionFact struct {
	// Dependent is the pipeline of the consuming (downstream) run.
	Dependent string
	// UpstreamPipeline is the pipeline of the consumed (upstream) run.
	UpstreamPipeline string
	// UpstreamRunID is the consumed upstream run's id.
	UpstreamRunID int64
}

// DeadLetterReader is the plain-MVCC read seam the dead-letter plane draws from: the
// outstanding worklist (with each propagated entry's poisoned upstream run), the
// consumption edges the shielded check needs, and lane membership for the untouched
// composer neighbors. A pgx-pool-backed implementation and a fake both satisfy it.
type DeadLetterReader interface {
	// Worklist returns the outstanding dead-letter worklist entries.
	Worklist(ctx context.Context) ([]DeadLetterWorklistEntry, error)
	// Consumptions returns the recorded consumption edges with both pipelines named.
	Consumptions(ctx context.Context) ([]ConsumptionFact, error)
	// LaneMembers returns the persisted composer rows (lane membership).
	LaneMembers(ctx context.Context) ([]LaneMember, error)
}

// SQL the dead-letter reader issues. Each is a plain SELECT over an MVCC snapshot.
const (
	// deadLetterWorklistSQL reads the worklist joined to runs for the pipeline, and
	// resolves each propagated entry's immediate poisoned upstream run from run_inputs
	// (the min upstream run id it recorded -- a propagated run records only poisoned
	// upstreams). A root cause records zero: its failed_upstream is never consulted.
	deadLetterWorklistSQL = `SELECT dl.run_id, r.pipeline, dl.reason,
    CASE WHEN dl.reason = 'upstream_dead_lettered'
         THEN COALESCE((SELECT min(upstream_run_id) FROM run_inputs WHERE run_id = dl.run_id), 0)
         ELSE 0 END
FROM dead_letters dl
JOIN runs r ON r.id = dl.run_id
ORDER BY dl.run_id`

	// deadLetterConsumptionsSQL reads consumption edges with both endpoints' pipelines.
	// An upstream run that has been pruned to its summary is dropped by the inner join;
	// the shielded check only needs live consumption edges.
	deadLetterConsumptionsSQL = `SELECT dr.pipeline, ur.pipeline, ri.upstream_run_id
FROM run_inputs ri
JOIN runs dr ON dr.id = ri.run_id
JOIN runs ur ON ur.id = ri.upstream_run_id`
)

// pgxDeadLetterReader is the pgx-pool-backed DeadLetterReader.
type pgxDeadLetterReader struct {
	pool readPool
}

// compile-time proof the pgx adapter satisfies the dead-letter read seam.
var _ DeadLetterReader = (*pgxDeadLetterReader)(nil)

// newPgxDeadLetterReader builds a dead-letter reader over a pooled-query seam.
func newPgxDeadLetterReader(pool readPool) *pgxDeadLetterReader {
	return &pgxDeadLetterReader{pool: pool}
}

// Worklist reads the outstanding worklist in one plain MVCC query.
func (r *pgxDeadLetterReader) Worklist(ctx context.Context) ([]DeadLetterWorklistEntry, error) {
	rows, err := r.pool.query(ctx, deadLetterWorklistSQL)
	if err != nil {
		return nil, fmt.Errorf("store: read dead-letter worklist: %w", err)
	}
	defer rows.Close()

	var out []DeadLetterWorklistEntry
	for rows.Next() {
		var e DeadLetterWorklistEntry
		var reason string
		if err := rows.Scan(&e.RunID, &e.Pipeline, &reason, &e.FailedUpstreamRunID); err != nil {
			return nil, fmt.Errorf("store: scan dead-letter worklist row: %w", err)
		}
		e.Reason = DeadLetterReason(reason)
		out = append(out, e)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: read dead-letter worklist: %w", err)
	}
	return out, nil
}

// Consumptions reads the consumption edges in one plain MVCC query.
func (r *pgxDeadLetterReader) Consumptions(ctx context.Context) ([]ConsumptionFact, error) {
	rows, err := r.pool.query(ctx, deadLetterConsumptionsSQL)
	if err != nil {
		return nil, fmt.Errorf("store: read consumption edges: %w", err)
	}
	defer rows.Close()

	var out []ConsumptionFact
	for rows.Next() {
		var f ConsumptionFact
		if err := rows.Scan(&f.Dependent, &f.UpstreamPipeline, &f.UpstreamRunID); err != nil {
			return nil, fmt.Errorf("store: scan consumption edge: %w", err)
		}
		out = append(out, f)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: read consumption edges: %w", err)
	}
	return out, nil
}

// LaneMembers reads the persisted composer rows in lane then walk (pos) order,
// reusing the stats source's lane query.
func (r *pgxDeadLetterReader) LaneMembers(ctx context.Context) ([]LaneMember, error) {
	rows, err := r.pool.query(ctx, statsLaneMembersSQL)
	if err != nil {
		return nil, fmt.Errorf("store: read lane members: %w", err)
	}
	defer rows.Close()

	var out []LaneMember
	for rows.Next() {
		var m LaneMember
		if err := rows.Scan(&m.Lane, &m.Pipeline); err != nil {
			return nil, fmt.Errorf("store: scan lane member: %w", err)
		}
		out = append(out, m)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: read lane members: %w", err)
	}
	return out, nil
}
