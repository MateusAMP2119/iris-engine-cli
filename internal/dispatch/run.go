package dispatch

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"strconv"
	"sync"

	"github.com/MateusAMP2119/iris-engine-cli/internal/exec"
	"github.com/MateusAMP2119/iris-engine-cli/internal/pg"
	"github.com/MateusAMP2119/iris-engine-cli/internal/store"
)

// This file is the run-start/cancel seam: the dispatcher-side glue that turns a
// declared run into a started subprocess and records its lifecycle through the single
// meta writer. Starting a run is a direct exec in the pipeline folder, in its own
// process group, with the composed environment; cancelling one kills that group and
// dead-letters the run as stopped, touching nothing else. It never imposes a timeout:
// a run ends only by exiting on its own or by explicit cancellation.

// DBConnEnvVar is the environment variable through which the engine injects a run's
// scoped database connection URL (env = inherited + declared + injected scoped DB
// connection). StartRun injects RunSpec.DBURL under this name, with the run's id
// riding it as the iris.run_id session setting, so a run resolves its connection
// from a single place. What the daemon supplies as RunSpec.DBURL today is the
// engine's own data-database DSN: the per-pipeline least-privilege login role (pg's
// ProvisionPipelineRole, over store's roles/grants/credentials ledger) is rendered
// but never provisioned for a run, so a run does not yet connect as its own scoped
// role -- this name remains the seam such a connection would arrive through.
const DBConnEnvVar = "IRIS_DB_URL"

// ErrRunNotInFlight reports that no in-flight run has the given id: it has already
// exited, was already cancelled, or was never started through this manager. Cancel
// acts only on a live run, so a caller learns its cancel found nothing to kill rather
// than silently dead-lettering a run that already finished.
var ErrRunNotInFlight = errors.New("dispatch: run not in flight")

// WriteCloser is the run-log sink a started run streams its output into: an
// io.WriteCloser the RunManager writes stdout and stderr to and closes when the run
// is reaped. It is the io.WriteCloser interface, named here only so the RunLog seam
// reads cleanly.
type WriteCloser = io.WriteCloser

// RunLog opens the per-run output sink a started run streams its stdout and stderr
// into. The daemon's per-run log writer (internal/daemon.RunLogWriter) is the
// production implementation, adapted to this seam; a test supplies a fake. It is an
// interface so dispatch depends on the log seam, not the daemon package (import
// direction: daemon -> dispatch).
type RunLog interface {
	// Open creates the per-run log for runID and returns the writer the run's output
	// is streamed into plus the reference recorded in runs.log_ref. The RunManager
	// closes the returned writer once the run is reaped.
	Open(runID string) (WriteCloser, string, error)
}

// RunSpec describes one run to start: the queued run's id, the pipeline folder it
// executes in, the direct-exec argv, the declared environment entries, and the
// scoped database connection URL injected into its environment.
type RunSpec struct {
	// RunID is the queued run's meta id; MarkRunRunning transitions it to running.
	RunID string
	// Dir is the pipeline folder, the subprocess working directory.
	Dir string
	// Argv is the direct-exec command; Argv[0] is the executable. Never a shell, so
	// it carries no pipes, globs, or metacharacter expansion.
	Argv []string
	// Env is the declared environment entries (KEY=VALUE), merged onto the inherited
	// daemon environment.
	Env []string
	// DBURL is the scoped database connection injected as DBConnEnvVar.
	DBURL string
}

// RunHandle is a started run: its process-group id (runs.handle), the log reference
// recorded in runs.log_ref, and a Wait the caller (a lane runner) blocks on for the
// run's terminal status. Cancellation is done through the RunManager, not here, so it
// always rides the meta write that dead-letters the run.
type RunHandle struct {
	pgid int
	ref  string
	h    exec.Handle
}

// PGID returns the run's process-group id, recorded as runs.handle.
func (rh RunHandle) PGID() int { return rh.pgid }

// LogRef returns the per-run log reference recorded in runs.log_ref.
func (rh RunHandle) LogRef() string { return rh.ref }

// Wait blocks until the run's subprocess is reaped and returns its terminal status.
func (rh RunHandle) Wait() (exec.ExitStatus, error) { return rh.h.Wait() }

// RunManager starts and cancels runs, recording each lifecycle change through the one
// dispatcher-owned single meta writer. It owns the in-flight table mapping a run id
// to its live process handle, so a cancel can reach the right group.
type RunManager struct {
	runner exec.Runner
	disp   *Dispatcher
	log    RunLog

	mu       sync.Mutex
	inflight map[string]exec.Handle
}

// NewRunManager builds a run manager over the process runner, the dispatcher whose
// single writer records run lifecycle, and the per-run log seam.
func NewRunManager(runner exec.Runner, disp *Dispatcher, log RunLog) *RunManager {
	return &RunManager{
		runner:   runner,
		disp:     disp,
		log:      log,
		inflight: map[string]exec.Handle{},
	}
}

// StartRun starts spec as a direct exec in its pipeline folder and its own process
// group, streams its output to the per-run log, and records the run running with its
// process-group handle through the single writer.
func (m *RunManager) StartRun(ctx context.Context, spec RunSpec) (RunHandle, error) {
	if spec.RunID == "" {
		return RunHandle{}, errors.New("dispatch: start run: empty run id")
	}
	if len(spec.Argv) == 0 {
		return RunHandle{}, errors.New("dispatch: start run: empty argv")
	}

	sink, ref, err := m.log.Open(spec.RunID)
	if err != nil {
		return RunHandle{}, fmt.Errorf("dispatch: start run %s: open log: %w", spec.RunID, err)
	}

	h, err := m.runner.Start(ctx, exec.Spec{
		Dir:    spec.Dir,
		Argv:   spec.Argv,
		Env:    composeEnv(spec),
		Stdout: sink,
		Stderr: sink,
	})
	if err != nil {
		// Nothing started: close the sink we opened so no descriptor leaks.
		_ = sink.Close()
		return RunHandle{}, fmt.Errorf("dispatch: start run %s: %w", spec.RunID, err)
	}

	// Record the started run through the single writer: state -> running, handle =
	// process-group id. If that write fails, the subprocess is already running but
	// unrecorded -- kill its group and drain before returning, so no orphaned,
	// untracked process escapes and the sink is closed.
	if err := m.disp.Submit(ctx, func(w *store.Writer) error {
		return w.MarkRunRunning(ctx, spec.RunID, h.PGID())
	}); err != nil {
		_ = h.Kill()
		_, _ = h.Wait()
		_ = sink.Close()
		return RunHandle{}, fmt.Errorf("dispatch: start run %s: record running: %w", spec.RunID, err)
	}

	m.mu.Lock()
	m.inflight[spec.RunID] = h
	m.mu.Unlock()

	// Reap on a manager-owned goroutine: whether the run exits on its own or a
	// cancel kills its group, wait for it to be reaped, then close its log sink and
	// drop it from the in-flight table so no handle, goroutine, or descriptor leaks.
	// Terminal-state recording (succeeded/failed) belongs to the lane runner, not
	// this seam, so it is deliberately not done here.
	go func() {
		_, _ = h.Wait()
		_ = sink.Close()
		m.mu.Lock()
		delete(m.inflight, spec.RunID)
		m.mu.Unlock()
	}()

	return RunHandle{pgid: h.PGID(), ref: ref, h: h}, nil
}

// CancelRun kills the run's process group and dead-letters it as stopped, touching
// nothing else.
func (m *RunManager) CancelRun(ctx context.Context, runID string) error {
	m.mu.Lock()
	h, ok := m.inflight[runID]
	m.mu.Unlock()
	if !ok {
		return fmt.Errorf("dispatch: cancel run %s: %w", runID, ErrRunNotInFlight)
	}

	// Kill the whole process group first: the subprocess (and every descendant that
	// inherited the group) is gone or going before the run is parked terminal. An
	// already-gone group is not an error. The reap goroutine that StartRun launched
	// observes the kill, closes the log, and drops the in-flight entry.
	if err := h.Kill(); err != nil {
		return fmt.Errorf("dispatch: cancel run %s: kill group: %w", runID, err)
	}

	// Dead-letter the run as stopped through the single writer -- one atomic CTE that
	// transitions running -> dead_lettered and records the worklist row. Only this
	// run is touched.
	if err := m.disp.Submit(ctx, func(w *store.Writer) error {
		return w.DeadLetterRun(ctx, runID, store.ReasonStopped, "run cancelled by iris run cancel")
	}); err != nil {
		return fmt.Errorf("dispatch: cancel run %s: dead-letter: %w", runID, err)
	}
	return nil
}

// KillInflight best-effort SIGKILLs every in-flight run's process group and returns
// how many groups it signalled. It is the self-demotion kill: a daemon that loses its
// meta session stops dispatching and kills its in-flight runs at once. It
// deliberately writes NOTHING to meta -- a deposed session cannot carry a meta write
// (the lock guard refuses a session that has not re-acquired the leader lock), and
// the runs' records are the NEW leader's to dead-letter during its startup
// reconciliation, which cannot reach these processes across hosts; this kill is the
// deposed side's half of that contract. An already-gone group is not an error. The
// per-run reap goroutines StartRun launched observe each kill, close the log sinks,
// and clear the in-flight table, exactly as a cancel would.
func (m *RunManager) KillInflight() int {
	m.mu.Lock()
	handles := make([]exec.Handle, 0, len(m.inflight))
	for _, h := range m.inflight {
		handles = append(handles, h)
	}
	m.mu.Unlock()

	for _, h := range handles {
		_ = h.Kill() // best-effort by design: the group may already be gone
	}
	return len(handles)
}

// ErrRunStateUnknown reports a run state value outside the closed lifecycle enum
// (queued, running, succeeded, dead_lettered): a mistyped or invented state that must
// never reach a meta write.
var ErrRunStateUnknown = errors.New("dispatch: unknown run state")

// ErrRunStateIllegal reports a well-formed but illegal lifecycle edge: a transition
// between two real run states the machine does not allow (e.g. a terminal state to
// any other, or queued straight to succeeded).
var ErrRunStateIllegal = errors.New("dispatch: illegal run state transition")

// runStates is the closed set of run lifecycle states (queued, running, succeeded,
// dead-lettered). A from or to value absent here is out-of-enum and rejected.
var runStates = map[store.RunState]bool{
	store.RunQueued:       true,
	store.RunRunning:      true,
	store.RunSucceeded:    true,
	store.RunDeadLettered: true,
}

// runTransitions is the closed run lifecycle graph: queued advances only to running,
// running ends in succeeded or dead_lettered, and the two terminal states have no
// successors (absent keys).
var runTransitions = map[store.RunState]map[store.RunState]bool{
	store.RunQueued:  {store.RunRunning: true},
	store.RunRunning: {store.RunSucceeded: true, store.RunDeadLettered: true},
}

// CheckRunTransition validates a proposed run state transition over the closed run
// lifecycle enum. It is pure -- no I/O -- and is the guard the run-record writes rely
// on: it rejects any from or to value that is not one of the four run states
// (ErrRunStateUnknown, so an out-of-enum value never reaches meta), and any pair of
// real states that is not a legal lifecycle edge (ErrRunStateIllegal).
func CheckRunTransition(from, to store.RunState) error {
	if !runStates[from] {
		return fmt.Errorf("%w: from %q", ErrRunStateUnknown, from)
	}
	if !runStates[to] {
		return fmt.Errorf("%w: to %q", ErrRunStateUnknown, to)
	}
	if !runTransitions[from][to] {
		return fmt.Errorf("%w: %q -> %q", ErrRunStateIllegal, from, to)
	}
	return nil
}

// composeEnv builds a run's child environment: the inherited daemon environment
// first, then the declared entries, then the injected scoped DB connection last, so
// each later group overrides an earlier duplicate key (os/exec keeps the last value
// for a duplicate key). The injected connection carries the run's id as the per-session
// iris.run_id setting the capture trigger reads (see injectedDBURL), so every write the
// run makes is attributed to it.
func composeEnv(spec RunSpec) []string {
	env := os.Environ()
	env = append(env, spec.Env...)
	env = append(env, DBConnEnvVar+"="+injectedDBURL(spec))
	return env
}

// injectedDBURL is the scoped connection URL the run receives as IRIS_DB_URL,
// carrying the run's id so the capture trigger attributes every write to it
// in-transaction (the run id rides the injected connection as a per-session setting
// at spawn). The id rides the DSN via pg.InjectRunID, the same mechanism the capture
// path reads back with current_setting('iris.run_id'). A run id that is not a bigint
// meta identity (only a synthetic non-numeric id, never a real run) leaves the URL
// unchanged; the capture trigger then fails any such run's write loudly rather than
// stamping an unattributed row -- fail-closed, never a silent unattributed write.
func injectedDBURL(spec RunSpec) string {
	id, err := strconv.ParseInt(spec.RunID, 10, 64)
	if err != nil {
		return spec.DBURL
	}
	return pg.InjectRunID(spec.DBURL, id)
}

// ResolveRunArgv selects the direct-exec argv for a run given its declared run
// vector and (optional) artifact hash. When artifactHash is non-nil the engine
// executes the content-addressed binary directly and ignores the declared run
// (built mode); otherwise it executes the declared source vector via its runtime
// (dev mode). objects may be nil for dev-only resolution in tests.
func ResolveRunArgv(declared []string, artifactHash *string, objects *store.ObjectStore) []string {
	if artifactHash != nil && objects != nil {
		// Built: exec the binary at the content-addressed path; ignore declared.
		return []string{objects.Path(*artifactHash)}
	}
	if artifactHash != nil {
		// Hash known but no objects seam: caller supplies materialized path; for
		// pure selection without objects return a distinct form (tests may override).
		return []string{"<built:" + *artifactHash + ">"}
	}
	// Dev: use the declared run vector verbatim.
	cp := make([]string, len(declared))
	copy(cp, declared)
	return cp
}
