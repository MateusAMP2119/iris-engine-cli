package store

import (
	"context"
	"fmt"
)

// This file is the store half of `iris pipeline promote`: the meta write that
// flips one pipeline's per-pipeline data_mode from disposable to permanent -- the
// control truth the wipe scope consults -- plus the plain-MVCC reads the promote
// gate decides from. Promote owns the data_mode column exactly as build owns
// artifact (registry.go: apply never overwrites either); the flip is promotion's
// entire meta footprint, and the journal-side marker flip lives in internal/pg
// (promote.go there). The gate itself -- refuse while un-built, repeat the
// cross-mode warning -- is dispatch work; this file only supplies its write and
// its facts.

// promotePipelineSQL flips one pipeline's data_mode to permanent. It is a
// single-row guarded UPDATE -- the pipeline named by bound parameter, the target
// mode a literal because permanent is the only mode promote may ever set (promote
// flips it permanent; nothing flips it back).
const promotePipelineSQL = `UPDATE pipelines SET data_mode = 'permanent' WHERE name = $1`

// PromotePipeline flips the named pipeline's per-pipeline data_mode in meta from
// disposable to permanent. It is a leader-only meta write riding the single
// Writer, it touches exactly the one named row, and it is idempotent:
// re-promoting a permanent pipeline rewrites the same value. The caller (the
// dispatch promote op) enforces the built gate before this runs; the write itself
// is deliberately gate-free so the rule lives in one place.
func (w *Writer) PromotePipeline(ctx context.Context, name string) error {
	if err := w.conn.Exec(ctx, promotePipelineSQL, name); err != nil {
		return fmt.Errorf("store: writer promote pipeline %q: %w", name, err)
	}
	return nil
}

// UpstreamDataMode pairs one depends_on upstream pipeline with its current data
// mode, read from meta: the per-upstream fact the promote-time cross-mode read
// warning is computed from (promote repeats the warning while an upstream read
// dependency stays disposable).
type UpstreamDataMode struct {
	// Pipeline is the upstream pipeline's name (dependencies.to_pipeline).
	Pipeline string
	// Mode is that upstream's current data mode (pipelines.data_mode).
	Mode DataMode
}

// PromoteStateReader reads the meta facts the promote gate decides from: the
// target's registration and data mode, its built state, and its upstreams' data
// modes. It is a plain-MVCC read seam, never serialized through the single
// writer; the pgx-pool-backed implementation and a canned fake both satisfy it.
type PromoteStateReader interface {
	// PipelineDataMode returns the pipeline's current data mode and whether the
	// pipeline is registered at all.
	PipelineDataMode(ctx context.Context, name string) (DataMode, bool, error)
	// PipelineBuilt reports whether the pipeline is in built state: it has a
	// recorded artifact. The artifacts table is insert-only and the current
	// artifact is the pipeline's newest row, so any row at all means a built,
	// content-addressed binary exists for the pipeline.
	PipelineBuilt(ctx context.Context, name string) (bool, error)
	// UpstreamDataModes returns the data mode of every registered upstream the
	// pipeline declares depends_on edges to, in upstream-name order.
	UpstreamDataModes(ctx context.Context, name string) ([]UpstreamDataMode, error)
}

// The promote-gate read statements.
const (
	// selectPipelineDataModeSQL reads one pipeline's data mode; no row means the
	// pipeline is not registered.
	selectPipelineDataModeSQL = `SELECT data_mode FROM pipelines WHERE name = $1`

	// selectPipelineBuiltSQL reads the built state: whether any artifacts row
	// exists for the pipeline (rows are immutable and insert-only, so existence
	// is exactly "a built binary was recorded").
	selectPipelineBuiltSQL = `SELECT EXISTS (SELECT 1 FROM artifacts WHERE pipeline = $1)`

	// selectUpstreamDataModesSQL reads each depends_on upstream's name and data
	// mode by joining the pipeline's dependency edges onto the registry.
	selectUpstreamDataModesSQL = `SELECT p.name, p.data_mode FROM dependencies d
JOIN pipelines p ON p.name = d.to_pipeline
WHERE d.from_pipeline = $1 ORDER BY p.name`
)

// pgxPromoteReader is the pgx-pool-backed PromoteStateReader: plain MVCC reads
// over the reader pool, no session pinning and no busy-retry, so the promote
// gate's reads never contend with the leader's single-writer path.
type pgxPromoteReader struct {
	pool readPool
}

// compile-time proof the pgx adapter satisfies the promote-state read seam.
var _ PromoteStateReader = (*pgxPromoteReader)(nil)

// PipelineDataMode reads one pipeline's data mode in one plain MVCC query. A
// missing row reports found = false, never an error.
func (r *pgxPromoteReader) PipelineDataMode(ctx context.Context, name string) (DataMode, bool, error) {
	rows, err := r.pool.query(ctx, selectPipelineDataModeSQL, name)
	if err != nil {
		return "", false, fmt.Errorf("store: read pipeline %q data mode: %w", name, err)
	}
	defer rows.Close()

	if !rows.Next() {
		if err := rows.Err(); err != nil {
			return "", false, fmt.Errorf("store: read pipeline %q data mode: %w", name, err)
		}
		return "", false, nil
	}
	var mode string
	if err := rows.Scan(&mode); err != nil {
		return "", false, fmt.Errorf("store: scan pipeline %q data mode: %w", name, err)
	}
	return DataMode(mode), true, nil
}

// PipelineBuilt reads the built state in one plain MVCC existence query.
func (r *pgxPromoteReader) PipelineBuilt(ctx context.Context, name string) (bool, error) {
	rows, err := r.pool.query(ctx, selectPipelineBuiltSQL, name)
	if err != nil {
		return false, fmt.Errorf("store: read pipeline %q built state: %w", name, err)
	}
	defer rows.Close()

	var built bool
	if rows.Next() {
		if err := rows.Scan(&built); err != nil {
			return false, fmt.Errorf("store: scan pipeline %q built state: %w", name, err)
		}
	}
	if err := rows.Err(); err != nil {
		return false, fmt.Errorf("store: read pipeline %q built state: %w", name, err)
	}
	return built, nil
}

// UpstreamDataModes reads the depends_on upstreams' data modes in one plain MVCC
// query.
func (r *pgxPromoteReader) UpstreamDataModes(ctx context.Context, name string) ([]UpstreamDataMode, error) {
	rows, err := r.pool.query(ctx, selectUpstreamDataModesSQL, name)
	if err != nil {
		return nil, fmt.Errorf("store: read pipeline %q upstream data modes: %w", name, err)
	}
	defer rows.Close()

	var out []UpstreamDataMode
	for rows.Next() {
		var up, mode string
		if err := rows.Scan(&up, &mode); err != nil {
			return nil, fmt.Errorf("store: scan pipeline %q upstream data mode: %w", name, err)
		}
		out = append(out, UpstreamDataMode{Pipeline: up, Mode: DataMode(mode)})
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: read pipeline %q upstream data modes: %w", name, err)
	}
	return out, nil
}
