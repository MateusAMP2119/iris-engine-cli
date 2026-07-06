// Package storetest provides an in-memory fake of the meta client seam
// (internal/store). It stands in for meta so the wiring around the control-plane
// database -- run records and their state transitions -- is tested with no live
// Postgres (S16/integration-fakes-interfaces).
//
// The fake is a faithful, concurrency-safe stand-in with database-like value
// semantics: every method returns copies, so a caller cannot reach back through
// a returned run to mutate stored state. It is deliberately minimal, matching
// the store seam it satisfies; later epics extend both together.
package storetest

import (
	"context"
	"fmt"
	"sync"

	"github.com/MateusAMP2119/iris-engine-cli/internal/store"
)

// Fake is an in-memory store.Store. The zero value is not usable; construct one
// with New.
type Fake struct {
	mu   sync.Mutex
	seq  int64
	runs []store.Run // creation order preserved
}

// New returns an empty in-memory meta-store fake.
func New() *Fake {
	return &Fake{}
}

// compile-time proof the fake satisfies the seams it stands in for: the full
// meta client (store.Store) and, for the read paths that draw plain MVCC snapshots
// (crash reconciliation reads leftover run records this way), the reader seam
// (store.Reader).
var (
	_ store.Store  = (*Fake)(nil)
	_ store.Reader = (*Fake)(nil)
)

// Runs satisfies store.Reader: it serves the same creation-ordered, filtered,
// value-copied snapshot ListRuns returns, so a component that reads run records
// through the plain-MVCC reader seam (rather than the full store) can stand on the
// fake with no live Postgres.
func (f *Fake) Runs(ctx context.Context, filter store.RunFilter) ([]store.Run, error) {
	return f.ListRuns(ctx, filter)
}

// CreateRun inserts a new queued run with the next id and ordering sequence.
func (f *Fake) CreateRun(_ context.Context, spec store.RunSpec) (store.Run, error) {
	f.mu.Lock()
	defer f.mu.Unlock()

	f.seq++
	r := store.Run{
		ID:       fmt.Sprintf("run-%d", f.seq),
		Pipeline: spec.Pipeline,
		Lane:     spec.Lane,
		State:    store.RunQueued,
		Seq:      f.seq,
	}
	f.runs = append(f.runs, r)
	return cloneRun(r), nil
}

// SetRunState transitions the identified run to state, applies the updates, and
// returns the updated run. It reports store.ErrRunNotFound for an unknown id.
func (f *Fake) SetRunState(_ context.Context, id string, state store.RunState, updates ...store.RunUpdate) (store.Run, error) {
	f.mu.Lock()
	defer f.mu.Unlock()

	i, ok := f.indexOf(id)
	if !ok {
		return store.Run{}, fmt.Errorf("%w: %s", store.ErrRunNotFound, id)
	}
	f.runs[i].State = state
	for _, u := range updates {
		u(&f.runs[i])
	}
	return cloneRun(f.runs[i]), nil
}

// GetRun returns the identified run, or store.ErrRunNotFound.
func (f *Fake) GetRun(_ context.Context, id string) (store.Run, error) {
	f.mu.Lock()
	defer f.mu.Unlock()

	i, ok := f.indexOf(id)
	if !ok {
		return store.Run{}, fmt.Errorf("%w: %s", store.ErrRunNotFound, id)
	}
	return cloneRun(f.runs[i]), nil
}

// ListRuns returns the runs matching filter in creation order.
func (f *Fake) ListRuns(_ context.Context, filter store.RunFilter) ([]store.Run, error) {
	f.mu.Lock()
	defer f.mu.Unlock()

	var out []store.Run
	for _, r := range f.runs {
		if matches(r, filter) {
			out = append(out, cloneRun(r))
		}
	}
	return out, nil
}

// indexOf returns the slice index of the run with id, and whether it was found.
// The caller must hold f.mu.
func (f *Fake) indexOf(id string) (int, bool) {
	for i := range f.runs {
		if f.runs[i].ID == id {
			return i, true
		}
	}
	return 0, false
}

// matches reports whether r passes filter; a zero filter field matches anything.
func matches(r store.Run, filter store.RunFilter) bool {
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

// cloneRun returns a deep copy of r, including a fresh ExitCode pointer, so no
// returned run aliases stored state.
func cloneRun(r store.Run) store.Run {
	c := r
	if r.ExitCode != nil {
		code := *r.ExitCode
		c.ExitCode = &code
	}
	return c
}
