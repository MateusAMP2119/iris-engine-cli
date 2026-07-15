package daemon

import (
	"sync"

	"github.com/MateusAMP2119/iris-lakehouse/internal/exec"
)

// This file is the daemon-held in-flight run registry: the production
// InflightKiller the self-demotion kill acts through. A daemon that loses its meta
// session stops dispatching and kills its in-flight runs at once, writing NOTHING
// to meta (a deposed session cannot carry a meta write, and the runs' records are
// the new leader's to dead-letter). The dispatch.RunManager satisfies
// InflightKiller for the lane-loop path; the manual `iris pipeline run` path runs
// subprocesses directly (not through a RunManager), so this registry gives those
// runs the same in-flight visibility, tracking each live process-group handle from
// start until reap.

// inflightRuns tracks the daemon's live manual-run process-group handles by run id,
// so a self-demotion can SIGKILL every one of them at once. It satisfies
// InflightKiller. Access is guarded because the manual orchestrator tracks and
// untracks from run goroutines while the election goroutine may fire the kill.
type inflightRuns struct {
	mu   sync.Mutex
	runs map[string]exec.Handle
}

// newInflightRuns builds an empty in-flight run registry.
func newInflightRuns() *inflightRuns {
	return &inflightRuns{runs: map[string]exec.Handle{}}
}

// track records a live run's process-group handle so the self-demotion kill can
// reach it. A later untrack (or a kill) drops it.
func (r *inflightRuns) track(runID string, h exec.Handle) {
	r.mu.Lock()
	r.runs[runID] = h
	r.mu.Unlock()
}

// untrack drops a run whose subprocess has been reaped, so a completed run is never
// a kill target and no handle leaks.
func (r *inflightRuns) untrack(runID string) {
	r.mu.Lock()
	delete(r.runs, runID)
	r.mu.Unlock()
}

// kill best-effort SIGKILLs one tracked run's process group, reporting whether it was
// in flight (tracked). It is the reach an operator `iris run cancel` uses to end a
// single running run; the run's own reap path untracks it, and the caller dead-letters
// it as stopped. An already-gone group is not an error; a run not tracked here (already
// terminal or never started on this daemon) returns false.
func (r *inflightRuns) kill(runID string) bool {
	r.mu.Lock()
	h, ok := r.runs[runID]
	r.mu.Unlock()
	if !ok {
		return false
	}
	_ = h.Kill() // best-effort by design: the group may already be gone
	return true
}

// KillInflight best-effort SIGKILLs every tracked run's process group and returns
// how many groups it signalled -- the deposed side's half of the failover kill. It
// writes nothing to meta: the deposed session carries no meta write, and the run
// records are the new leader's to dead-letter. An already-gone group is not an
// error. It snapshots the handles under the guard, then kills outside it, so a run
// reaping and untracking concurrently never blocks the kill.
func (r *inflightRuns) KillInflight() int {
	r.mu.Lock()
	handles := make([]exec.Handle, 0, len(r.runs))
	for _, h := range r.runs {
		handles = append(handles, h)
	}
	r.mu.Unlock()

	for _, h := range handles {
		_ = h.Kill() // best-effort by design: the group may already be gone
	}
	return len(handles)
}
