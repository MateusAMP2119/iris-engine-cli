package store

import (
	"context"
	"encoding/json"
	"fmt"
)

// This file is the read and terminal-write surface the manual `iris pipeline run`
// op composes beyond the registry apply. The reads are plain MVCC (never the
// single writer, never busy-retried): a pipeline's run target (folder + argv) to
// execute, its upstreams' latest runs to resolve the depends_on gate, the
// run_inputs already-consumed check, and the persisted lane rows for the
// queue-or-run-now routing. The one write here, MarkRunSucceeded, is the guarded
// running -> succeeded terminal transition the immediate own-lane run records,
// riding the single Writer like every other run-record write.

// LaneEntry is one persisted (lane, pipeline, pos) row, the manual router's basis
// for deciding whether a pipeline is a lane member (queued at the run boundary)
// or its own lane (run immediately). It mirrors the lanes table.
type LaneEntry struct {
	// Lane is the lane the row places its pipeline in.
	Lane string
	// Pipeline is the placed pipeline's name (a name, never an FK).
	Pipeline string
	// Pos is the row's walk position.
	Pos int64
}

// PipelineRunTarget is a pipeline's dev-run execution target: the workspace-relative
// folder it runs in and the direct-exec argv, read from the registry so the manual run
// can start the subprocess.
type PipelineRunTarget struct {
	// Folder is the pipeline's workspace-relative folder (pipelines.folder).
	Folder string
	// Argv is the declared direct-exec command (pipelines.run).
	Argv []string
}

// LatestRunInfo is a pipeline's most recent run: its id and lifecycle state, the
// single run the depends_on gate reads for an upstream, and the handle the
// immediate runner reads back after minting a manual run. DeadLetterReason is
// the run's OUTSTANDING dead-letter worklist reason, empty when the run is not
// dead-lettered or its worklist row was drained or replayed away -- the lane
// gate's no-retry brake reads it (an outstanding failure parks the pipeline; a
// stopped run or a drained failure does not).
type LatestRunInfo struct {
	// ID is the run's meta id.
	ID int64
	// State is the run's lifecycle state.
	State RunState
	// DeadLetterReason is the outstanding dead_letters.reason, empty when none.
	DeadLetterReason DeadLetterReason
	// DeadLetterDetail is the outstanding dead_letters.error text; the lane gate reads it to tell an operator cancel (parks) from a crash-reconciliation stop (never parks).
	DeadLetterDetail string
}

// QueuedManualRun is one enqueued lane-member manual run awaiting its lane's run
// boundary: the queued run's id and the artifact hash it was minted against (nil
// for a dev run), everything the lane loop needs to start it.
type QueuedManualRun struct {
	// ID is the queued run's meta id.
	ID int64
	// ArtifactHash is the built artifact the run was minted for, nil for dev.
	ArtifactHash *string
}

// QueuedManualReader is the plain-MVCC read seam the lane loop's queued-manual
// pickup composes: the queued cause=manual runs of a pipeline, oldest first, that
// the lane runner starts in turn at the member's boundary. The pgx manual reader
// satisfies it; a fake satisfies it in tests.
type QueuedManualReader interface {
	// QueuedManualRuns returns pipeline's queued cause=manual runs, ascending by id.
	QueuedManualRuns(ctx context.Context, pipeline string) ([]QueuedManualRun, error)
}

// ManualReader is the plain-MVCC read seam the manual-run op composes: pipeline run
// targets, latest-run lookups, the run_inputs consumed check, and the lane roster. A
// pgx-pool-backed implementation and a fake both satisfy it; reads are never serialized
// through the writer and never retried.
type ManualReader interface {
	// PipelineRunTarget returns a pipeline's folder and run argv, and whether it is
	// registered.
	PipelineRunTarget(ctx context.Context, name string) (PipelineRunTarget, bool, error)
	// LatestRun returns a pipeline's most recent run (highest id), and whether it has
	// any run at all.
	LatestRun(ctx context.Context, pipeline string) (LatestRunInfo, bool, error)
	// Consumed reports whether dependent has a run_inputs row recording
	// upstreamRunID among the upstream runs any of its runs consumed (the gate's
	// already-consumed check).
	Consumed(ctx context.Context, dependent string, upstreamRunID int64) (bool, error)
	// LaneRows returns every persisted (lane, pipeline, pos) row, in (lane, pos) order.
	LaneRows(ctx context.Context) ([]LaneEntry, error)
}

// The manual-run read statements. Each is a single plain SELECT: an MVCC snapshot, no
// locking clause, no advisory-lock interplay.
const (
	selectPipelineRunTargetSQL = `SELECT folder, run FROM pipelines WHERE name = $1`
	selectLatestRunSQL = `SELECT r.id, r.state, coalesce(d.reason, ''), coalesce(d.error, '')
    FROM runs r LEFT JOIN dead_letters d ON d.run_id = r.id
    WHERE r.pipeline = $1 ORDER BY r.id DESC LIMIT 1`
	selectQueuedManualRunsSQL  = `SELECT id, artifact_hash FROM runs WHERE pipeline = $1 AND state = 'queued' AND cause = 'manual' ORDER BY id`
	selectConsumedSQL          = `SELECT EXISTS (
    SELECT 1 FROM run_inputs ri JOIN runs r ON r.id = ri.run_id
    WHERE r.pipeline = $1 AND ri.upstream_run_id = $2)`
	selectLaneRowsSQL = `SELECT lane, pipeline, pos FROM lanes ORDER BY lane, pos`
)

// markRunSucceededSQL transitions a run from running to succeeded, recording exit code
// zero. It is guarded on the running state, so it can only ever act on a run actually in
// flight -- never one already terminal -- and is one atomic Exec.
const markRunSucceededSQL = `UPDATE runs SET state = $1, exit_code = 0 WHERE id = $2 AND state = $3`

// pgxManualReader is the pgx-pool-backed ManualReader.
type pgxManualReader struct {
	pool readPool
}

// compile-time proof the pgx adapter satisfies the manual-run read seam.
var _ ManualReader = (*pgxManualReader)(nil)

// newPgxManualReader builds a manual-run reader over a pooled-query seam.
func newPgxManualReader(pool readPool) *pgxManualReader { return &pgxManualReader{pool: pool} }

// PipelineRunTarget reads a pipeline's folder and run argv in one plain MVCC query. The
// run argv is stored as JSON (registry.pipelineUpsertSQL), so it is unmarshaled here.
func (r *pgxManualReader) PipelineRunTarget(ctx context.Context, name string) (PipelineRunTarget, bool, error) {
	rows, err := r.pool.query(ctx, selectPipelineRunTargetSQL, name)
	if err != nil {
		return PipelineRunTarget{}, false, fmt.Errorf("store: read pipeline run target %q: %w", name, err)
	}
	defer rows.Close()

	if !rows.Next() {
		if err := rows.Err(); err != nil {
			return PipelineRunTarget{}, false, fmt.Errorf("store: read pipeline run target %q: %w", name, err)
		}
		return PipelineRunTarget{}, false, nil
	}
	var folder, runJSON string
	if err := rows.Scan(&folder, &runJSON); err != nil {
		return PipelineRunTarget{}, false, fmt.Errorf("store: scan pipeline run target %q: %w", name, err)
	}
	var argv []string
	if err := json.Unmarshal([]byte(runJSON), &argv); err != nil {
		return PipelineRunTarget{}, false, fmt.Errorf("store: decode run argv for %q: %w", name, err)
	}
	return PipelineRunTarget{Folder: folder, Argv: argv}, true, nil
}

// LatestRun reads a pipeline's most recent run (highest id) in one plain MVCC query.
func (r *pgxManualReader) LatestRun(ctx context.Context, pipeline string) (LatestRunInfo, bool, error) {
	rows, err := r.pool.query(ctx, selectLatestRunSQL, pipeline)
	if err != nil {
		return LatestRunInfo{}, false, fmt.Errorf("store: read latest run for %q: %w", pipeline, err)
	}
	defer rows.Close()

	if !rows.Next() {
		if err := rows.Err(); err != nil {
			return LatestRunInfo{}, false, fmt.Errorf("store: read latest run for %q: %w", pipeline, err)
		}
		return LatestRunInfo{}, false, nil
	}
	var info LatestRunInfo
	var state, reason string
	if err := rows.Scan(&info.ID, &state, &reason, &info.DeadLetterDetail); err != nil {
		return LatestRunInfo{}, false, fmt.Errorf("store: scan latest run for %q: %w", pipeline, err)
	}
	info.State = RunState(state)
	info.DeadLetterReason = DeadLetterReason(reason)
	return info, true, nil
}

// QueuedManualRuns reads pipeline's queued cause=manual runs (oldest first) in one
// plain MVCC query, satisfying the lane loop's queued-manual pickup seam.
func (r *pgxManualReader) QueuedManualRuns(ctx context.Context, pipeline string) ([]QueuedManualRun, error) {
	rows, err := r.pool.query(ctx, selectQueuedManualRunsSQL, pipeline)
	if err != nil {
		return nil, fmt.Errorf("store: read queued manual runs for %q: %w", pipeline, err)
	}
	defer rows.Close()

	var out []QueuedManualRun
	for rows.Next() {
		var q QueuedManualRun
		if err := rows.Scan(&q.ID, &q.ArtifactHash); err != nil {
			return nil, fmt.Errorf("store: scan queued manual run for %q: %w", pipeline, err)
		}
		out = append(out, q)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: iterate queued manual runs for %q: %w", pipeline, err)
	}
	return out, nil
}

// Consumed answers the run_inputs already-consumed check in one plain MVCC query,
// with no mutable cursor.
func (r *pgxManualReader) Consumed(ctx context.Context, dependent string, upstreamRunID int64) (bool, error) {
	rows, err := r.pool.query(ctx, selectConsumedSQL, dependent, upstreamRunID)
	if err != nil {
		return false, fmt.Errorf("store: read consumed check for %q upstream run %d: %w", dependent, upstreamRunID, err)
	}
	defer rows.Close()

	if !rows.Next() {
		if err := rows.Err(); err != nil {
			return false, fmt.Errorf("store: read consumed check for %q: %w", dependent, err)
		}
		return false, nil
	}
	var consumed bool
	if err := rows.Scan(&consumed); err != nil {
		return false, fmt.Errorf("store: scan consumed check for %q: %w", dependent, err)
	}
	return consumed, nil
}

// LaneRows reads every persisted lane row in one plain MVCC query, ordered by (lane,
// pos).
func (r *pgxManualReader) LaneRows(ctx context.Context) ([]LaneEntry, error) {
	rows, err := r.pool.query(ctx, selectLaneRowsSQL)
	if err != nil {
		return nil, fmt.Errorf("store: read lane rows: %w", err)
	}
	defer rows.Close()

	var out []LaneEntry
	for rows.Next() {
		var e LaneEntry
		if err := rows.Scan(&e.Lane, &e.Pipeline, &e.Pos); err != nil {
			return nil, fmt.Errorf("store: scan lane row: %w", err)
		}
		out = append(out, e)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: read lane rows: %w", err)
	}
	return out, nil
}

// MarkRunSucceeded records a run's successful terminal transition: in one guarded atomic
// statement it moves the run from running to succeeded and stamps exit code zero. The
// UPDATE is guarded on the running state, so it can only ever act on a run in flight,
// never one already terminal. It is a leader-only meta write, riding the single Writer.
func (w *Writer) MarkRunSucceeded(ctx context.Context, id string) error {
	if err := w.conn.Exec(ctx, markRunSucceededSQL, RunSucceeded, id, RunRunning); err != nil {
		return fmt.Errorf("store: writer mark run succeeded %s: %w", id, err)
	}
	return nil
}
