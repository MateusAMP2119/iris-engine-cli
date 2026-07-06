package store

import (
	"context"
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
