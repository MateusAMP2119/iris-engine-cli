package store

import (
	"context"
	"fmt"
)

// This file is the pipeline-list read surface (iris pipeline list shows pipelines
// with a queued or running run, --all every registered pipeline). It is a
// plain-MVCC read like the runs reader: one query joins the pipelines registry to
// whether each pipeline has an active (queued or running) run, and the default
// and --all views are two filters over that one result. It never rides the single
// writer and is never busy-retried.

// PipelineListing is one row of iris pipeline list: a registered pipeline's name and
// whether it currently has a queued or running run (the active predicate the default
// view filters on).
type PipelineListing struct {
	// Name is the registered pipeline's name.
	Name string
	// Active reports whether the pipeline has a queued or running run.
	Active bool
}

// PipelineLister reads the pipeline-list surface: the default active view and the --all
// every-registered view. A pgx-pool-backed implementation and a fake both satisfy it;
// reads are plain MVCC, never serialized through the writer and never retried.
type PipelineLister interface {
	// ActivePipelines returns the registered pipelines with a queued or running run, in
	// name order -- the default iris pipeline list view.
	ActivePipelines(ctx context.Context) ([]PipelineListing, error)
	// AllPipelines returns every registered pipeline with its active flag, in name order
	// -- the iris pipeline list --all view.
	AllPipelines(ctx context.Context) ([]PipelineListing, error)
}

// selectPipelineListingSQL reads every registered pipeline with an EXISTS
// predicate over runs for whether it has a queued or running run. It is one plain
// SELECT: no locking clause, an MVCC snapshot, ordered by name (the pipelines
// collection key).
const selectPipelineListingSQL = `SELECT p.name,
    EXISTS (SELECT 1 FROM runs r WHERE r.pipeline = p.name AND r.state IN ('queued', 'running')) AS active
FROM pipelines p
ORDER BY p.name`

// pgxPipelineLister is the pgx-pool-backed PipelineLister: plain MVCC over the reader
// pool, no session pinning and no busy-retry.
type pgxPipelineLister struct {
	pool readPool
}

// compile-time proof the pgx adapter satisfies the pipeline-list read seam.
var _ PipelineLister = (*pgxPipelineLister)(nil)

// newPgxPipelineLister builds a pipeline-list reader over a pooled-query seam.
func newPgxPipelineLister(pool readPool) *pgxPipelineLister { return &pgxPipelineLister{pool: pool} }

// ActivePipelines returns only the pipelines whose active flag is set, filtered in Go
// over the one snapshot the base query returns (the same in-memory-filter pattern the
// runs reader uses), so the default and --all views share one query and one code path.
func (l *pgxPipelineLister) ActivePipelines(ctx context.Context) ([]PipelineListing, error) {
	all, err := l.list(ctx)
	if err != nil {
		return nil, err
	}
	active := make([]PipelineListing, 0, len(all))
	for _, p := range all {
		if p.Active {
			active = append(active, p)
		}
	}
	return active, nil
}

// AllPipelines returns every registered pipeline with its active flag.
func (l *pgxPipelineLister) AllPipelines(ctx context.Context) ([]PipelineListing, error) {
	return l.list(ctx)
}

// list issues the one plain MVCC query and scans the listing. A query error is returned
// immediately -- no retry, no backoff, no second attempt.
func (l *pgxPipelineLister) list(ctx context.Context) ([]PipelineListing, error) {
	rows, err := l.pool.query(ctx, selectPipelineListingSQL)
	if err != nil {
		return nil, fmt.Errorf("store: read pipeline listing: %w", err)
	}
	defer rows.Close()

	var out []PipelineListing
	for rows.Next() {
		var p PipelineListing
		if err := rows.Scan(&p.Name, &p.Active); err != nil {
			return nil, fmt.Errorf("store: scan pipeline listing: %w", err)
		}
		out = append(out, p)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: read pipeline listing: %w", err)
	}
	return out, nil
}
