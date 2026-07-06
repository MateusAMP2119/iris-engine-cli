package store

import (
	"context"
	"errors"
	"testing"
)

// countingRows is a minimal poolRows fake: an empty result set that records it was
// consumed and closed.
type countingRows struct {
	closed bool
}

func (r *countingRows) Next() bool        { return false }
func (r *countingRows) Scan(...any) error { return nil }
func (r *countingRows) Err() error        { return nil }
func (r *countingRows) Close()            { r.closed = true }

// countingPool is a fake readPool that records how many times a query was issued,
// so a test can prove the reader attempts a failing read exactly once -- no
// busy-retry, no backoff loop (specification section 2: "No busy-retry anywhere").
type countingPool struct {
	attempts int
	err      error
	rows     *countingRows
}

func (p *countingPool) query(_ context.Context, _ string, _ ...any) (poolRows, error) {
	p.attempts++
	if p.err != nil {
		return nil, p.err
	}
	p.rows = &countingRows{}
	return p.rows, nil
}

// TestReaderNoBusyRetry proves the meta reader uses a plain MVCC query with no
// busy-retry: a failing read is attempted exactly once and its error propagates
// immediately, never re-attempted behind a retry or backoff loop.
//
// spec: S02/readers-plain-mvcc-no-retry
func TestReaderNoBusyRetry(t *testing.T) {
	t.Run("S02/readers-plain-mvcc-no-retry", func(t *testing.T) {
		t.Run("a failing read propagates immediately, attempted exactly once", func(t *testing.T) {
			boom := errors.New("connection reset by peer")
			pool := &countingPool{err: boom}
			r := newPgxReader(pool)

			_, err := r.Runs(context.Background(), RunFilter{})
			if !errors.Is(err, boom) {
				t.Fatalf("Runs error = %v, want it to wrap %v", err, boom)
			}
			if pool.attempts != 1 {
				t.Errorf("read attempted %d times, want exactly 1 (no busy-retry)", pool.attempts)
			}
		})

		t.Run("a successful read closes its rows and returns them once", func(t *testing.T) {
			pool := &countingPool{}
			r := newPgxReader(pool)
			runs, err := r.Runs(context.Background(), RunFilter{})
			if err != nil {
				t.Fatalf("Runs: %v", err)
			}
			if len(runs) != 0 {
				t.Errorf("empty result set yielded %d runs, want 0", len(runs))
			}
			if pool.attempts != 1 {
				t.Errorf("read attempted %d times, want exactly 1", pool.attempts)
			}
			if pool.rows == nil || !pool.rows.closed {
				t.Error("reader did not close the rows it opened")
			}
		})
	})
}
