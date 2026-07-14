package store

import (
	"context"
	"fmt"
)

// This file is the meta read path for count-based retention: the plain-MVCC reads
// the lane loop's post-pass pruner draws its decision inputs from. The decision
// itself is pure and lives in dispatch (SelectPrunable); the prune write is the
// single writer's (Writer.PruneRun). This reader supplies the three inputs those
// need: every run's (id, pipeline) for the newest-`retain` count, the run ids an
// outstanding dead_letters entry still holds (spared until replay, supersession,
// or drain releases them), and the full archival records of the runs selected for
// pruning.

// RetentionRunRef is one run as the count-based retention decision reads it: the
// run's meta identity and its pipeline, nothing else (retention is count-based
// and clockless -- the run id orders newest-first, the pipeline groups).
type RetentionRunRef struct {
	// RunID is the run's meta identity (runs.id).
	RunID int64
	// Pipeline is the run's pipeline (runs.pipeline).
	Pipeline string
}

// RetentionReader is the plain-MVCC read seam the post-pass pruner and the
// destroy teardown's archival step draw from. A pgx-pool-backed implementation
// and a fake both satisfy it.
type RetentionReader interface {
	// RetentionRuns returns every run's (id, pipeline), ascending by id: the
	// count-based decision's input.
	RetentionRuns(ctx context.Context) ([]RetentionRunRef, error)
	// OutstandingDeadLetterRuns returns the run ids held by outstanding
	// dead_letters entries, ascending: the runs retention spares.
	OutstandingDeadLetterRuns(ctx context.Context) ([]int64, error)
	// PrunableRunsByID returns the full archival records of the given runs, in
	// ascending id order, each carrying its consumed upstream run ids. Only
	// TERMINAL runs are returned: a queued or running run among ids is silently
	// omitted, so a prune can never delete a run that is still in flight.
	PrunableRunsByID(ctx context.Context, ids []int64) ([]PrunableRun, error)
	// PrunablePipelineRuns returns the full archival records of EVERY remaining
	// run of one pipeline, ascending, any state: the destroy teardown archives
	// whatever run history still exists before retiring it (the destructive-op
	// gate has already refused or cancelled in-flight runs by then).
	PrunablePipelineRuns(ctx context.Context, pipeline string) ([]PrunableRun, error)
	// ArtifactHashes returns the pipeline's content-addressed artifact hashes
	// (its artifacts index rows, including built-but-never-run artifacts): the
	// object bytes a teardown frees once the meta retirement commits.
	ArtifactHashes(ctx context.Context, pipeline string) ([]string, error)
}

// SQL the retention reader issues. Each is a plain SELECT over an MVCC snapshot.
const (
	// retentionRunsSQL reads every run's (id, pipeline), the count-based decision's
	// whole input.
	retentionRunsSQL = `SELECT id, pipeline FROM runs ORDER BY id`

	// outstandingDeadLetterRunsSQL reads the run ids the outstanding worklist holds:
	// dead_letters rows exist only while outstanding (replay, supersession, and
	// drain delete them), so the table IS the outstanding set.
	outstandingDeadLetterRunsSQL = `SELECT run_id FROM dead_letters ORDER BY run_id`

	// prunableRunsSQL reads the archival record of each selected run. The state
	// predicate pins the terminal-only guarantee (the literals are RunSucceeded and
	// RunDeadLettered): a run that raced back into flight between the decision read
	// and this read is dropped rather than pruned mid-run.
	prunableRunsSQL = `SELECT id, pipeline, state, artifact_hash, declaration_checksum, snapshot_lsn, journal_floor, journal_ceiling
FROM runs
WHERE id = ANY($1) AND state IN ('succeeded', 'dead_lettered')
ORDER BY id`

	// prunableRunInputsSQL reads the selected runs' own consumption ledger rows, the
	// consumed upstream ids the archival summary preserves as JSON.
	prunableRunInputsSQL = `SELECT run_id, upstream_run_id FROM run_inputs WHERE run_id = ANY($1) ORDER BY run_id, upstream_run_id`

	// prunablePipelineRunsSQL reads the archival record of every remaining run of
	// one pipeline, any state: the destroy teardown archives whatever exists.
	prunablePipelineRunsSQL = `SELECT id, pipeline, state, artifact_hash, declaration_checksum, snapshot_lsn, journal_floor, journal_ceiling
FROM runs
WHERE pipeline = $1
ORDER BY id`

	// pipelineRunInputsSQL reads the consumption ledger rows of one pipeline's runs.
	pipelineRunInputsSQL = `SELECT ri.run_id, ri.upstream_run_id
FROM run_inputs ri JOIN runs r ON r.id = ri.run_id
WHERE r.pipeline = $1
ORDER BY ri.run_id, ri.upstream_run_id`

	// artifactHashesSQL reads one pipeline's content-addressed artifact hashes.
	artifactHashesSQL = `SELECT hash FROM artifacts WHERE pipeline = $1 ORDER BY hash`
)

// pgxRetentionReader is the pgx-pool-backed RetentionReader.
type pgxRetentionReader struct {
	pool readPool
}

// compile-time proof the pgx adapter satisfies the retention read seam.
var _ RetentionReader = (*pgxRetentionReader)(nil)

// newPgxRetentionReader builds a retention reader over a pooled-query seam.
func newPgxRetentionReader(pool readPool) *pgxRetentionReader {
	return &pgxRetentionReader{pool: pool}
}

// RetentionRuns reads every run's (id, pipeline) in one plain MVCC query.
func (r *pgxRetentionReader) RetentionRuns(ctx context.Context) ([]RetentionRunRef, error) {
	rows, err := r.pool.query(ctx, retentionRunsSQL)
	if err != nil {
		return nil, fmt.Errorf("store: read retention runs: %w", err)
	}
	defer rows.Close()

	var out []RetentionRunRef
	for rows.Next() {
		var ref RetentionRunRef
		if err := rows.Scan(&ref.RunID, &ref.Pipeline); err != nil {
			return nil, fmt.Errorf("store: scan retention run row: %w", err)
		}
		out = append(out, ref)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: read retention runs: %w", err)
	}
	return out, nil
}

// OutstandingDeadLetterRuns reads the held run ids in one plain MVCC query.
func (r *pgxRetentionReader) OutstandingDeadLetterRuns(ctx context.Context) ([]int64, error) {
	rows, err := r.pool.query(ctx, outstandingDeadLetterRunsSQL)
	if err != nil {
		return nil, fmt.Errorf("store: read outstanding dead-letter runs: %w", err)
	}
	defer rows.Close()

	var out []int64
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			return nil, fmt.Errorf("store: scan outstanding dead-letter run row: %w", err)
		}
		out = append(out, id)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: read outstanding dead-letter runs: %w", err)
	}
	return out, nil
}

// PrunableRunsByID reads the full archival records of the given runs (terminal
// only) and stitches in each run's consumed upstream ids from run_inputs.
func (r *pgxRetentionReader) PrunableRunsByID(ctx context.Context, ids []int64) ([]PrunableRun, error) {
	if len(ids) == 0 {
		return nil, nil
	}
	return r.readPrunable(ctx, prunableRunsSQL, prunableRunInputsSQL, ids)
}

// PrunablePipelineRuns reads the archival records of every remaining run of one
// pipeline, any state, with each run's consumed upstream ids stitched in.
func (r *pgxRetentionReader) PrunablePipelineRuns(ctx context.Context, pipeline string) ([]PrunableRun, error) {
	return r.readPrunable(ctx, prunablePipelineRunsSQL, pipelineRunInputsSQL, pipeline)
}

// ArtifactHashes reads one pipeline's content-addressed artifact hashes.
func (r *pgxRetentionReader) ArtifactHashes(ctx context.Context, pipeline string) ([]string, error) {
	rows, err := r.pool.query(ctx, artifactHashesSQL, pipeline)
	if err != nil {
		return nil, fmt.Errorf("store: read artifact hashes: %w", err)
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var h string
		if err := rows.Scan(&h); err != nil {
			return nil, fmt.Errorf("store: scan artifact hash row: %w", err)
		}
		out = append(out, h)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: read artifact hashes: %w", err)
	}
	return out, nil
}

// readPrunable runs one archival-record read: the run rows under runsSQL, then
// their consumption ledger rows under inputsSQL, both parameterized by arg, with
// the consumed upstream ids stitched into each record.
func (r *pgxRetentionReader) readPrunable(ctx context.Context, runsSQL, inputsSQL string, arg any) ([]PrunableRun, error) {
	rows, err := r.pool.query(ctx, runsSQL, arg)
	if err != nil {
		return nil, fmt.Errorf("store: read prunable runs: %w", err)
	}
	defer rows.Close()

	var out []PrunableRun
	for rows.Next() {
		var run PrunableRun
		var state string
		if err := rows.Scan(&run.RunID, &run.Pipeline, &state, &run.ArtifactHash,
			&run.DeclarationChecksum, &run.SnapshotLSN, &run.JournalFloor, &run.JournalCeiling); err != nil {
			return nil, fmt.Errorf("store: scan prunable run row: %w", err)
		}
		run.State = RunState(state)
		out = append(out, run)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: read prunable runs: %w", err)
	}

	inRows, err := r.pool.query(ctx, inputsSQL, arg)
	if err != nil {
		return nil, fmt.Errorf("store: read prunable run inputs: %w", err)
	}
	defer inRows.Close()

	consumed := make(map[int64][]int64)
	for inRows.Next() {
		var runID, upstream int64
		if err := inRows.Scan(&runID, &upstream); err != nil {
			return nil, fmt.Errorf("store: scan prunable run input row: %w", err)
		}
		consumed[runID] = append(consumed[runID], upstream)
	}
	if err := inRows.Err(); err != nil {
		return nil, fmt.Errorf("store: read prunable run inputs: %w", err)
	}
	for i := range out {
		out[i].ConsumedUpstreamRunIDs = consumed[out[i].RunID]
	}
	return out, nil
}
