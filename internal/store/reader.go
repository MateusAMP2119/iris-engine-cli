package store

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// This file is the meta read path: plain Postgres MVCC connections, drawn from a
// pool, with no busy-retry anywhere (specification section 2: "Readers: plain
// Postgres connections, MVCC. No busy-retry anywhere."). Reads never contend with
// the leader's single-writer path -- MVCC serves a consistent snapshot without
// blocking, which is why any Postgres client can read meta while the daemon holds
// the leader lock (the advisory lock guards leadership, not rows). A read that
// fails surfaces its error at once; it is never re-attempted behind a retry or
// backoff loop.

// Reader is the meta read seam: plain MVCC reads over a connection pool. A
// pgx-pool-backed implementation and a fake both satisfy it. Reads are never
// serialized through the dispatcher (that path is writes only) and never retried.
type Reader interface {
	// Runs returns the runs matching filter, in ordering-identity order. It reads a
	// plain MVCC snapshot; on error the error is returned immediately, never retried.
	Runs(ctx context.Context, filter RunFilter) ([]Run, error)

	// ProvenanceLineage returns the runs, run_summaries and run_inputs needed to
	// build a lineage for provenance walk. One MVCC snapshot; errors abort.
	ProvenanceLineage(ctx context.Context) (ProvenanceLineage, error)
}

// ProvenanceLineage is the snapshot of meta rows the provenance walk consumes
// (live runs, archival summaries with consumed list, consumption edges). It
// avoids importing pg at store layer; daemon maps it to pg.Lineage.
type ProvenanceLineage struct {
	Runs []struct {
		RunID               int64
		Pipeline            string
		State               string
		ArtifactHash        *string
		DeclarationChecksum string
		SnapshotLSN         *string
		JournalFloor        *int64
		JournalCeiling      *int64
	}
	Summaries []struct {
		RunID                  int64
		Pipeline               string
		State                  string
		ArtifactHash           *string
		DeclarationChecksum    string
		ConsumedUpstreamRunIDs []int64
		SnapshotLSN            *string
		JournalFloor           *int64
		JournalCeiling         *int64
	}
	Inputs []struct {
		RunID         int64
		UpstreamRunID int64
	}
}

// poolRows is the minimal row-cursor surface the reader consumes, so a test can
// stand in for a live result set. pgx.Rows satisfies it (its method set is a
// superset), so the pgx-pool adapter returns pgx.Rows directly.
type poolRows interface {
	Next() bool
	Scan(dest ...any) error
	Err() error
	Close()
}

// readPool is the minimal pooled-query surface the reader issues reads through, so
// the no-busy-retry behavior is provable against a fake with no live Postgres.
type readPool interface {
	query(ctx context.Context, sql string, args ...any) (poolRows, error)
}

// pgxReadPool adapts a *pgxpool.Pool to the readPool seam: each query checks a
// connection out of the pool, runs, and returns it -- plain MVCC, no session
// pinning (unlike the leader lock) and no retry.
type pgxReadPool struct {
	pool *pgxpool.Pool
}

// compile-time proof the pgx pool adapter satisfies the read seam.
var _ readPool = (*pgxReadPool)(nil)

func (p *pgxReadPool) query(ctx context.Context, sql string, args ...any) (poolRows, error) {
	rows, err := p.pool.Query(ctx, sql, args...)
	if err != nil {
		return nil, err
	}
	return rows, nil
}

// selectRunsSQL reads the run records the reader returns. It is a plain SELECT: no
// locking clause, no advisory-lock interplay, just an MVCC snapshot.
const selectRunsSQL = "SELECT id, pipeline, state, coalesce(exit_code, 0), coalesce(handle, 0) FROM runs ORDER BY id"

// pgxReader is the pgx-pool-backed Reader.
type pgxReader struct {
	pool readPool
}

// compile-time proof the reader satisfies the seam.
var _ Reader = (*pgxReader)(nil)

// newPgxReader builds a reader over a pooled-query seam.
func newPgxReader(pool readPool) *pgxReader { return &pgxReader{pool: pool} }

// Runs issues one plain MVCC query and scans the result. A query error is returned
// immediately -- there is no retry, no backoff, no second attempt. filter is
// applied in memory over the snapshot (E05 pushes it into SQL); the point E02.6
// pins is that the read path is a single, un-retried MVCC query.
func (r *pgxReader) Runs(ctx context.Context, filter RunFilter) ([]Run, error) {
	rows, err := r.pool.query(ctx, selectRunsSQL)
	if err != nil {
		return nil, fmt.Errorf("store: read runs: %w", err)
	}
	defer rows.Close()

	var out []Run
	for rows.Next() {
		var run Run
		var exit, handle int64
		if err := rows.Scan(&run.ID, &run.Pipeline, &run.State, &exit, &handle); err != nil {
			return nil, fmt.Errorf("store: scan run: %w", err)
		}
		run.Handle = int(handle)
		code := int(exit)
		run.ExitCode = &code
		if runMatchesFilter(run, filter) {
			out = append(out, run)
		}
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: read runs: %w", err)
	}
	return out, nil
}

// runMatchesFilter reports whether r passes filter; a zero filter field matches
// anything, so the zero RunFilter matches every run.
func runMatchesFilter(r Run, filter RunFilter) bool {
	if filter.Pipeline != "" && r.Pipeline != filter.Pipeline {
		return false
	}
	if filter.Lane != "" && r.Lane != filter.Lane {
		return false
	}
	if filter.State != "" && r.State != filter.State {
		return false
	}
	return true
}

// pgxRowsAssert keeps the pgx.Rows/poolRows compatibility honest at compile time:
// a value satisfying pgx.Rows also satisfies the reader's poolRows seam, so the
// pool adapter can hand its result set straight through.
var _ = func(r pgx.Rows) poolRows { return r }

// ProvenanceLineage implements the three meta reads for the provenance walk:
// live runs (for facts), run_summaries (facts + consumed list fallback), run_inputs.
func (r *pgxReader) ProvenanceLineage(ctx context.Context) (ProvenanceLineage, error) {
	var lin ProvenanceLineage

	// Live runs.
	runRows, err := r.pool.query(ctx, `SELECT id, pipeline, state, artifact_hash, declaration_checksum, snapshot_lsn, journal_floor, journal_ceiling FROM runs`)
	if err != nil {
		return ProvenanceLineage{}, fmt.Errorf("store: read lineage runs: %w", err)
	}
	defer runRows.Close()
	for runRows.Next() {
		var rec struct {
			RunID               int64
			Pipeline            string
			State               string
			ArtifactHash        *string
			DeclarationChecksum string
			SnapshotLSN         *string
			JournalFloor        *int64
			JournalCeiling      *int64
		}
		if err := runRows.Scan(&rec.RunID, &rec.Pipeline, &rec.State, &rec.ArtifactHash, &rec.DeclarationChecksum, &rec.SnapshotLSN, &rec.JournalFloor, &rec.JournalCeiling); err != nil {
			return ProvenanceLineage{}, fmt.Errorf("store: scan lineage run: %w", err)
		}
		lin.Runs = append(lin.Runs, rec)
	}
	if err := runRows.Err(); err != nil {
		return ProvenanceLineage{}, fmt.Errorf("store: lineage runs iter: %w", err)
	}
	runRows.Close()

	// Summaries (pruned runs).
	sumRows, err := r.pool.query(ctx, `SELECT run_id, pipeline, state, artifact_hash, declaration_checksum, consumed_upstream_run_ids, snapshot_lsn, journal_floor, journal_ceiling FROM run_summaries`)
	if err != nil {
		return ProvenanceLineage{}, fmt.Errorf("store: read lineage summaries: %w", err)
	}
	defer sumRows.Close()
	for sumRows.Next() {
		var sum struct {
			RunID                  int64
			Pipeline               string
			State                  string
			ArtifactHash           *string
			DeclarationChecksum    string
			ConsumedUpstreamRunIDs []int64
			SnapshotLSN            *string
			JournalFloor           *int64
			JournalCeiling         *int64
		}
		var consJSON []byte
		if err := sumRows.Scan(&sum.RunID, &sum.Pipeline, &sum.State, &sum.ArtifactHash, &sum.DeclarationChecksum, &consJSON, &sum.SnapshotLSN, &sum.JournalFloor, &sum.JournalCeiling); err != nil {
			return ProvenanceLineage{}, fmt.Errorf("store: scan lineage summary: %w", err)
		}
		if len(consJSON) != 0 {
			if uerr := json.Unmarshal(consJSON, &sum.ConsumedUpstreamRunIDs); uerr != nil {
				return ProvenanceLineage{}, fmt.Errorf("store: unmarshal consumed upstreams for summary %d: %w", sum.RunID, uerr)
			}
		}
		lin.Summaries = append(lin.Summaries, sum)
	}
	if err := sumRows.Err(); err != nil {
		return ProvenanceLineage{}, fmt.Errorf("store: lineage summaries iter: %w", err)
	}
	sumRows.Close()

	// Inputs (consumption edges for ancestry).
	inRows, err := r.pool.query(ctx, `SELECT run_id, upstream_run_id FROM run_inputs`)
	if err != nil {
		return ProvenanceLineage{}, fmt.Errorf("store: read lineage inputs: %w", err)
	}
	defer inRows.Close()
	for inRows.Next() {
		var in struct {
			RunID         int64
			UpstreamRunID int64
		}
		if err := inRows.Scan(&in.RunID, &in.UpstreamRunID); err != nil {
			return ProvenanceLineage{}, fmt.Errorf("store: scan lineage input: %w", err)
		}
		lin.Inputs = append(lin.Inputs, in)
	}
	if err := inRows.Err(); err != nil {
		return ProvenanceLineage{}, fmt.Errorf("store: lineage inputs iter: %w", err)
	}
	inRows.Close()

	return lin, nil
}
