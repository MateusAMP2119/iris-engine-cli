package dispatch

// This file is the reconciliation executor: it applies the pure Plan (reconcile.go)
// against the live world. It reads the leftover run records through the plain-MVCC
// reader seam, computes the Plan, best-effort SIGKILLs surviving process groups
// through the group-kill seam FIRST, then disposes of each run -- dead-letter a
// running run, delete a queued one -- through the single-writer dispatch path, so
// every reconciliation meta write rides the one meta writer (single writer). It runs
// before any lane dispatch, identically on cold start and failover, driven by the
// leader (internal/daemon).

import (
	"context"
	"fmt"
	"io"
	"log/slog"

	"github.com/MateusAMP2119/iris-lakehouse/internal/exec"
	"github.com/MateusAMP2119/iris-lakehouse/internal/store"
)

// GroupKiller best-effort SIGKILLs a surviving process group by its recorded
// handle (pgid). It is the seam the reconciler kills survivors through: the real
// implementation (RealGroupKiller) delegates to the exec seam; a test injects a
// recording fake so kills are proven with no real process. An already-gone group
// is not an error.
type GroupKiller interface {
	// KillGroup best-effort SIGKILLs the process group with the given pgid.
	KillGroup(pgid int) error
}

// execGroupKiller is the production GroupKiller: it SIGKILLs the group through the
// exec seam (the only package that owns process groups).
type execGroupKiller struct{}

// KillGroup SIGKILLs the process group through the exec seam.
func (execGroupKiller) KillGroup(pgid int) error { return exec.KillGroup(pgid) }

// RealGroupKiller returns the production group killer, backed by the exec seam. The
// leader wires it into the Reconciler; tests inject a recording fake instead.
func RealGroupKiller() GroupKiller { return execGroupKiller{} }

// Submitter is the single-writer submission seam the reconciler disposes runs
// through: the leader's Dispatcher implements it, so every reconciliation meta
// write rides the one meta-writer path (never a second writer). A test can stand a
// real Dispatcher over a recording connection in its place.
type Submitter interface {
	// Submit runs fn against the single Writer on the dispatcher goroutine.
	Submit(ctx context.Context, fn func(*store.Writer) error) error
}

// compile-time proof the Dispatcher is the single-writer submission seam.
var _ Submitter = (*Dispatcher)(nil)

// Reconciler applies startup reconciliation. Build it with NewReconciler over the
// meta reader, the single-writer submitter (the Dispatcher), the group killer, and
// the host-identity predicate; run it with Reconcile before dispatching any lane.
type Reconciler struct {
	reader  store.Reader
	submit  Submitter
	killer  GroupKiller
	matcher HostMatcher
	logger  *slog.Logger
}

// NewReconciler builds the reconciler. A nil matcher defaults to SingleHostMatcher
// (single-host: every survivor is killable here); a nil logger discards output.
func NewReconciler(reader store.Reader, submit Submitter, killer GroupKiller, matcher HostMatcher, logger *slog.Logger) *Reconciler {
	if matcher == nil {
		matcher = SingleHostMatcher()
	}
	if logger == nil {
		logger = slog.New(slog.NewTextHandler(io.Discard, nil))
	}
	return &Reconciler{reader: reader, submit: submit, killer: killer, matcher: matcher, logger: logger}
}

// Reconcile runs startup reconciliation: it reads the leftover running and queued
// run records through the reader, computes the Plan, best-effort SIGKILLs surviving
// process groups FIRST (a kill failure is logged, never fatal -- the survivor may
// already be gone), then disposes of each run through the single-writer path
// (dead-letter running runs, delete queued ones). A disposal failure aborts: the
// leader must not dispatch on an unreconciled meta. It never touches the journal.
func (r *Reconciler) Reconcile(ctx context.Context) error {
	running, err := r.reader.Runs(ctx, store.RunFilter{State: store.RunRunning})
	if err != nil {
		return fmt.Errorf("dispatch: reconcile read running runs: %w", err)
	}
	queued, err := r.reader.Runs(ctx, store.RunFilter{State: store.RunQueued})
	if err != nil {
		return fmt.Errorf("dispatch: reconcile read queued runs: %w", err)
	}

	view := ReconcileView{
		Runs:        append(append([]store.Run(nil), running...), queued...),
		SpawnedHere: r.matcher,
	}
	plan := Reconcile(view)

	// KILL survivors first, best-effort: a restarting same-host leader has only the
	// recorded handles, no live Handle, so it SIGKILLs each group by its pgid. A kill
	// failure (the group may already be gone) is logged, never fatal -- the run is
	// dead-lettered regardless.
	for _, k := range plan.Kills {
		if err := r.killer.KillGroup(k.Handle); err != nil {
			r.logger.Warn("iris reconcile: best-effort kill of surviving process group failed",
				"run", k.RunID, "pgid", k.Handle, "err", err)
		}
	}

	// THEN dispose of each run through the single-writer path: dead-letter running
	// runs, delete queued ones. A disposal failure aborts -- the leader must not
	// dispatch on an unreconciled meta.
	for _, d := range plan.Disposals {
		var derr error
		switch d.Kind {
		case ActionDeadLetter:
			derr = r.submit.Submit(ctx, func(w *store.Writer) error {
				return w.DeadLetterRun(ctx, d.RunID, d.Reason, d.Detail)
			})
		case ActionDeleteQueued:
			derr = r.submit.Submit(ctx, func(w *store.Writer) error {
				return w.DeleteQueuedRun(ctx, d.RunID)
			})
		default:
			derr = fmt.Errorf("dispatch: reconcile: unknown disposal kind %q", d.Kind)
		}
		if derr != nil {
			return fmt.Errorf("dispatch: reconcile dispose run %s (%s): %w", d.RunID, d.Kind, derr)
		}
	}
	return nil
}
