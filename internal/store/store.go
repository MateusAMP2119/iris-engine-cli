// Package store is the meta client seam: the sole path Iris uses to read and
// write meta, the dedicated control-plane database (specification section 10).
// Only the leader writes meta, through one dispatcher-owned single-writer path
// guarded by the leader lock (sections 2 and 15); this package will own that
// path and that lock.
//
// This is the minimal, behavior-focused seam the later epics extend: E02 lands
// the real Postgres-backed implementation, the eighteen-table roster, and the
// leader lock; E05's dispatcher drives run records through it. It is deliberately
// small -- run records and their states, the surface the dispatch tests need --
// so the seam can grow without churn. A fake (internal/store/storetest) satisfies
// the interface so the wiring around meta is tested with no live database
// (S16/integration-fakes-interfaces).
package store

import (
	"context"
	"errors"
)

// ErrRunNotFound is returned by the id-addressed methods when no run has the
// given id. Callers test it with errors.Is.
var ErrRunNotFound = errors.New("store: run not found")

// RunState is a run's lifecycle state (specification section 1). Dead-lettered
// is the single non-success terminal state (a failed, cancelled, or
// upstream-dead-lettered run).
type RunState string

// The run states.
const (
	// RunQueued is a run recorded but not yet started.
	RunQueued RunState = "queued"
	// RunRunning is a run whose subprocess is in flight.
	RunRunning RunState = "running"
	// RunSucceeded is a run that exited zero.
	RunSucceeded RunState = "succeeded"
	// RunDeadLettered is the single non-success terminal state, parked in the
	// dead-letter worklist. Its wire token is the spec's DDL/grammar form
	// `dead_lettered` (specification sections 4 and 7), the value E02's Postgres
	// CHECK constraint and every --json golden pin.
	RunDeadLettered RunState = "dead_lettered"
)

// Run is one execution record in meta: a minimal slice of the runs table
// (section 4) carrying only what the dispatch seam needs today.
type Run struct {
	// ID is the run's stable identity, assigned on creation.
	ID string
	// Pipeline is the declared pipeline this run executes.
	Pipeline string
	// Lane is the lane the pipeline belongs to.
	Lane string
	// State is the run's current lifecycle state.
	State RunState
	// Handle is the process-group id of the run's subprocess (runs.handle), set
	// when the run starts; zero before then.
	Handle int
	// ExitCode is the subprocess exit code, set on a terminal state that carries
	// one; nil otherwise.
	ExitCode *int
	// Reason is the dead-letter reason, set when State is RunDeadLettered.
	Reason string
	// Seq is a monotonic ordering identity assigned on creation: identity, never
	// a clock (section 4, ordering-identity-never-clock).
	Seq int64
}

// RunSpec is the input to CreateRun: the pipeline and lane a new run executes.
type RunSpec struct {
	// Pipeline is the pipeline to run.
	Pipeline string
	// Lane is the pipeline's lane.
	Lane string
}

// RunFilter selects a subset of runs for ListRuns. A zero field matches any
// value, so the zero RunFilter matches every run.
type RunFilter struct {
	// Pipeline, when set, restricts to runs of that pipeline.
	Pipeline string
	// Lane, when set, restricts to runs in that lane.
	Lane string
	// State, when set, restricts to runs in that state.
	State RunState
}

// RunUpdate mutates the run fields that accompany a state transition. Callers
// pass the exported constructors (WithHandle, WithExitCode, WithReason) to
// SetRunState.
type RunUpdate func(*Run)

// WithHandle records the run's process-group id, set as the run starts.
func WithHandle(pgid int) RunUpdate {
	return func(r *Run) { r.Handle = pgid }
}

// WithExitCode records the subprocess exit code on a terminal transition.
func WithExitCode(code int) RunUpdate {
	return func(r *Run) {
		c := code
		r.ExitCode = &c
	}
}

// WithReason records the dead-letter reason on a dead-lettering transition.
func WithReason(reason string) RunUpdate {
	return func(r *Run) { r.Reason = reason }
}

// Store is the meta client seam. Its methods are the single-writer path onto the
// control-plane database; a real Postgres-backed implementation and an in-memory
// fake both satisfy it. All blocking calls take a context.
type Store interface {
	// CreateRun inserts a new run in the queued state and returns it with its
	// assigned id and monotonic ordering sequence.
	CreateRun(ctx context.Context, spec RunSpec) (Run, error)
	// SetRunState transitions the run with the given id to state, applying any
	// field updates, and returns the updated run. It reports ErrRunNotFound when
	// no such run exists.
	SetRunState(ctx context.Context, id string, state RunState, updates ...RunUpdate) (Run, error)
	// GetRun returns the run with the given id, or ErrRunNotFound.
	GetRun(ctx context.Context, id string) (Run, error)
	// ListRuns returns the runs matching filter, in creation (sequence) order.
	ListRuns(ctx context.Context, filter RunFilter) ([]Run, error)
}
