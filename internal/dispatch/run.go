package dispatch

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"sync"

	"github.com/MateusAMP2119/iris-engine-cli/internal/exec"
	"github.com/MateusAMP2119/iris-engine-cli/internal/store"
)

// This file is the run-start/cancel seam: the dispatcher-side glue that turns a
// declared run into a started subprocess and records its lifecycle through the
// single meta writer (specification section 1). Starting a run is a direct exec in
// the pipeline folder, in its own process group, with the composed environment;
// cancelling one kills that group and dead-letters the run as stopped, touching
// nothing else. It never imposes a timeout: a run ends only by exiting on its own or
// by explicit cancellation (specification section 6.3 clock doctrine).

// DBConnEnvVar is the environment variable through which the engine injects a run's
// scoped database connection URL (specification section 1: env = inherited + declared
// + injected scoped DB connection). It is the documented seam the real per-pipeline
// scoped connection (E04.4) later populates; today StartRun injects RunSpec.DBURL
// under this name so a run can already resolve its connection from a single place.
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
	panic("todo")
}

// CancelRun kills the run's process group and dead-letters it as stopped, touching
// nothing else.
func (m *RunManager) CancelRun(ctx context.Context, runID string) error {
	panic("todo")
}

// CheckRunTransition validates a proposed run state transition over the closed run
// lifecycle enum.
func CheckRunTransition(from, to store.RunState) error {
	panic("todo")
}

// composeEnv builds a run's child environment: the inherited daemon environment
// first, then the declared entries, then the injected scoped DB connection last, so
// each later group overrides an earlier duplicate key (os/exec keeps the last value
// for a duplicate key).
func composeEnv(spec RunSpec) []string {
	env := os.Environ()
	env = append(env, spec.Env...)
	env = append(env, DBConnEnvVar+"="+spec.DBURL)
	return env
}

// ensure the manager compiles against the seam it drives.
var _ = fmt.Sprintf
