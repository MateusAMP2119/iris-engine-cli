package store

import (
	"context"
	"encoding/json"
	"fmt"
)

// This file is the pipeline-show read surface: the plain-MVCC meta reads `iris
// pipeline show <name>` composes into a single-pipeline readout -- the resolved
// declaration (folder, run argv, artifact and data modes), the pipeline role's
// field-level grants, its recent runs, and the inputs the depends_on gate ledger
// is computed from (the dependency edges, each upstream's latest run, and the
// already-consumed check). Every read is a plain pooled MVCC query, never
// serialized through the single writer and never busy-retried: viewing a pipeline
// never blocks the leader's writes.
//
// The ledger itself is not composed here -- the gate is dispatch's, and store must
// not import it. This seam returns the raw meta facts; the daemon feeds them to
// dispatch's pure gate to produce the per-edge verdict.

// PipelineDetail is a pipeline's resolved declaration as the show readout consumes
// it: the workspace-relative folder, the run argv, and the artifact/data modes.
// The depends_on edges are read separately (DependencyEdges), so this carries only
// the pipelines-row columns.
type PipelineDetail struct {
	// Folder is the pipeline's workspace-relative folder (pipelines.folder).
	Folder string
	// Run is the declared run argv (pipelines.run, stored as JSON).
	Run []string
	// Artifact is the pipeline's artifact mode (pipelines.artifact).
	Artifact Artifact
	// DataMode is the pipeline's data mode (pipelines.data_mode).
	DataMode DataMode
}

// ShowReader is the plain-MVCC read seam the pipeline-show op composes: a
// pipeline's declaration detail, its role's grants, its runs, the dependency
// edges, each upstream's latest run, and the already-consumed check. A
// pgx-pool-backed implementation and a fake both satisfy it; reads are never
// serialized through the writer and never retried.
type ShowReader interface {
	// PipelineDetail returns a pipeline's resolved declaration detail, and whether
	// it is registered.
	PipelineDetail(ctx context.Context, name string) (PipelineDetail, bool, error)
	// GrantsForRole returns a role's field-level grants, in stable order.
	GrantsForRole(ctx context.Context, pgRole string) ([]Grant, error)
	// Runs returns the runs matching filter, in ordering-identity order.
	Runs(ctx context.Context, filter RunFilter) ([]Run, error)
	// DependencyEdges returns every persisted depends_on edge (from = dependent).
	DependencyEdges(ctx context.Context) ([]DependencyEdge, error)
	// LatestRun returns a pipeline's most recent run (highest id), and whether it
	// has any run at all -- the single run the depends_on gate reads for an upstream.
	LatestRun(ctx context.Context, pipeline string) (LatestRunInfo, bool, error)
	// Consumed reports whether dependent has a run_inputs row recording
	// upstreamRunID among the upstream runs any of its runs consumed.
	Consumed(ctx context.Context, dependent string, upstreamRunID int64) (bool, error)
	// LaneRows returns every persisted (lane, pipeline, pos) row, in (lane, pos) order.
	LaneRows(ctx context.Context) ([]LaneEntry, error)
	// RegisteredPipelines returns the names of all registered pipelines in name order.
	RegisteredPipelines(ctx context.Context) ([]string, error)
}

// The pipeline-show read statements new to this file. Each is a single plain
// SELECT: an MVCC snapshot, no locking clause. The dependency-edge, latest-run,
// consumed, and runs reads reuse the readers this type embeds.
const (
	// selectPipelineDetailSQL reads a pipeline's declaration columns.
	selectPipelineDetailSQL = `SELECT folder, run, artifact, data_mode FROM pipelines WHERE name = $1`

	// selectRoleGrantsSQL reads a role's field-level grants in a stable order. The
	// reserved column names schema and table are double-quoted.
	selectRoleGrantsSQL = `SELECT "schema", "table", field, access FROM grants WHERE pg_role = $1
ORDER BY "schema", "table", field, access`
)

// pgxShowReader is the pgx-pool-backed ShowReader. It embeds the runs, registry,
// and manual-run readers to reuse their plain-MVCC reads (Runs, DependencyEdges,
// LatestRun, Consumed) and adds the two declaration/grant reads over the same
// pool, so the show readout draws one consistent set of MVCC snapshots.
type pgxShowReader struct {
	*pgxReader
	*pgxRegistryReader
	*pgxManualReader
	pool readPool
}

// compile-time proof the pgx adapter satisfies the pipeline-show read seam.
var _ ShowReader = (*pgxShowReader)(nil)

// newPgxShowReader builds a pipeline-show reader over a shared pooled-query seam.
func newPgxShowReader(pool readPool) *pgxShowReader {
	return &pgxShowReader{
		pgxReader:         newPgxReader(pool),
		pgxRegistryReader: &pgxRegistryReader{pool: pool},
		pgxManualReader:   newPgxManualReader(pool),
		pool:              pool,
	}
}

// PipelineDetail reads a pipeline's declaration columns in one plain MVCC query.
// The run argv is stored as JSON (registry.pipelineUpsertSQL), so it is
// unmarshaled here.
func (r *pgxShowReader) PipelineDetail(ctx context.Context, name string) (PipelineDetail, bool, error) {
	rows, err := r.pool.query(ctx, selectPipelineDetailSQL, name)
	if err != nil {
		return PipelineDetail{}, false, fmt.Errorf("store: read pipeline detail %q: %w", name, err)
	}
	defer rows.Close()

	if !rows.Next() {
		if err := rows.Err(); err != nil {
			return PipelineDetail{}, false, fmt.Errorf("store: read pipeline detail %q: %w", name, err)
		}
		return PipelineDetail{}, false, nil
	}
	var folder, runJSON, artifact, dataMode string
	if err := rows.Scan(&folder, &runJSON, &artifact, &dataMode); err != nil {
		return PipelineDetail{}, false, fmt.Errorf("store: scan pipeline detail %q: %w", name, err)
	}
	var argv []string
	if err := json.Unmarshal([]byte(runJSON), &argv); err != nil {
		return PipelineDetail{}, false, fmt.Errorf("store: decode run argv for %q: %w", name, err)
	}
	return PipelineDetail{Folder: folder, Run: argv, Artifact: Artifact(artifact), DataMode: DataMode(dataMode)}, true, nil
}

// GrantsForRole reads a role's field-level grants in one plain MVCC query, in a
// stable order.
func (r *pgxShowReader) GrantsForRole(ctx context.Context, pgRole string) ([]Grant, error) {
	rows, err := r.pool.query(ctx, selectRoleGrantsSQL, pgRole)
	if err != nil {
		return nil, fmt.Errorf("store: read grants for role %q: %w", pgRole, err)
	}
	defer rows.Close()

	var out []Grant
	for rows.Next() {
		var g Grant
		var access string
		if err := rows.Scan(&g.Schema, &g.Table, &g.Field, &access); err != nil {
			return nil, fmt.Errorf("store: scan grant for role %q: %w", pgRole, err)
		}
		g.Access = GrantAccess(access)
		out = append(out, g)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: read grants for role %q: %w", pgRole, err)
	}
	return out, nil
}

// LaneRows delegates to the embedded manual reader (reuses its MVCC query).
func (r *pgxShowReader) LaneRows(ctx context.Context) ([]LaneEntry, error) {
	return r.pgxManualReader.LaneRows(ctx)
}

// RegisteredPipelines delegates to the embedded registry reader.
func (r *pgxShowReader) RegisteredPipelines(ctx context.Context) ([]string, error) {
	return r.pgxRegistryReader.RegisteredPipelines(ctx)
}
