package store

import (
	"context"
	"errors"
	"reflect"
	"testing"
)

// listingRows is a poolRows fake yielding a seeded set of (name, active) pipeline rows,
// so the pipeline-list reader is proven with no live Postgres (integration tier).
type listingRows struct {
	rows   []PipelineListing
	i      int
	closed bool
}

func (r *listingRows) Next() bool { r.i++; return r.i <= len(r.rows) }

func (r *listingRows) Scan(dest ...any) error {
	row := r.rows[r.i-1]
	*(dest[0].(*string)) = row.Name
	*(dest[1].(*bool)) = row.Active
	return nil
}

func (r *listingRows) Err() error { return nil }
func (r *listingRows) Close()     { r.closed = true }

// listingPool is a fake readPool returning a seeded listing (or an error), recording the
// query attempts so the reader's single-query, no-retry behavior stays provable.
type listingPool struct {
	rows     []PipelineListing
	err      error
	attempts int
	last     *listingRows
}

func (p *listingPool) query(_ context.Context, _ string, _ ...any) (poolRows, error) {
	p.attempts++
	if p.err != nil {
		return nil, p.err
	}
	p.last = &listingRows{rows: p.rows}
	return p.last, nil
}

// TestPipelineListActiveDefault proves the pipeline-list surface: by default the listing
// carries only pipelines with a queued or running run (ActivePipelines), while --all
// expands to every registered pipeline (AllPipelines). The active flag is the queued/
// running-run predicate the base query computes; the default view filters on it and the
// all view keeps every row.
//
// spec: S08/pipeline-list-active-default
func TestPipelineListActiveDefault(t *testing.T) {
	t.Run("S08/pipeline-list-active-default", func(t *testing.T) {
		seed := []PipelineListing{
			{Name: "extract_orders", Active: true},  // has a running run
			{Name: "load_customers", Active: false}, // registered, no active run
			{Name: "reconcile", Active: true},       // has a queued run
			{Name: "sweep", Active: false},          // registered, idle
		}

		t.Run("default lists only pipelines with a queued or running run", func(t *testing.T) {
			pool := &listingPool{rows: seed}
			lister := newPgxPipelineLister(pool)

			got, err := lister.ActivePipelines(context.Background())
			if err != nil {
				t.Fatalf("ActivePipelines: %v", err)
			}
			want := []PipelineListing{{Name: "extract_orders", Active: true}, {Name: "reconcile", Active: true}}
			if !reflect.DeepEqual(got, want) {
				t.Errorf("active listing = %v, want %v", got, want)
			}
			if pool.attempts != 1 {
				t.Errorf("active listing issued %d queries, want 1", pool.attempts)
			}
			if pool.last == nil || !pool.last.closed {
				t.Error("reader did not close the rows it opened")
			}
		})

		t.Run("--all expands to every registered pipeline", func(t *testing.T) {
			pool := &listingPool{rows: seed}
			lister := newPgxPipelineLister(pool)

			got, err := lister.AllPipelines(context.Background())
			if err != nil {
				t.Fatalf("AllPipelines: %v", err)
			}
			if !reflect.DeepEqual(got, seed) {
				t.Errorf("all listing = %v, want %v", got, seed)
			}
		})

		t.Run("no active runs yields an empty default listing, not every pipeline", func(t *testing.T) {
			pool := &listingPool{rows: []PipelineListing{
				{Name: "a", Active: false}, {Name: "b", Active: false},
			}}
			lister := newPgxPipelineLister(pool)
			got, err := lister.ActivePipelines(context.Background())
			if err != nil {
				t.Fatalf("ActivePipelines: %v", err)
			}
			if len(got) != 0 {
				t.Errorf("active listing with no active runs = %v, want empty", got)
			}
		})

		t.Run("a read failure propagates, attempted exactly once", func(t *testing.T) {
			boom := errors.New("connection reset")
			pool := &listingPool{err: boom}
			lister := newPgxPipelineLister(pool)
			if _, err := lister.AllPipelines(context.Background()); !errors.Is(err, boom) {
				t.Fatalf("AllPipelines error = %v, want it to wrap %v", err, boom)
			}
			if pool.attempts != 1 {
				t.Errorf("read attempted %d times, want exactly 1 (no busy-retry)", pool.attempts)
			}
		})
	})
}
