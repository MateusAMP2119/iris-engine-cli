// Package exectest provides a fake of the subprocess execution seam
// (internal/exec) with no real process. Runs are scripted: each program name
// maps to an Outcome that streams stdout/stderr to the seam's writers and either
// exits with a code or blocks until cancelled. It lets dispatch tests start runs
// in composer order, stream their output, and cancel a run mid-flight with no OS
// process (S16/integration-fakes-interfaces).
//
// This is test-support infrastructure imported only by _test.go files.
package exectest

import (
	"context"
	"io"
	"sync"
	"syscall"

	"github.com/MateusAMP2119/iris-engine-cli/internal/exec"
)

// Outcome scripts one fake subprocess: the bytes it streams to stdout and stderr
// on start, and its termination -- either a natural exit code, or Block, meaning
// the run hangs until it is killed or its context is cancelled (a hung run held
// for a mid-flight cancel).
type Outcome struct {
	// Stdout is written to the spec's stdout writer when the run starts.
	Stdout string
	// Stderr is written to the spec's stderr writer when the run starts.
	Stderr string
	// Exit is the exit code of a non-blocking run.
	Exit int
	// Block makes the run hang until Kill or context cancellation, reported then
	// as a signaled termination.
	Block bool
}

// Runner is a fake exec.Runner. Outcomes are matched by the first argv element
// (the program); an unscripted program uses the default Outcome (a clean exit 0
// unless SetDefault says otherwise). The zero value is not usable; construct one
// with New.
type Runner struct {
	mu      sync.Mutex
	scripts map[string]Outcome
	def     Outcome
	nextPID int
}

// New returns a fake runner with no scripted programs.
func New() *Runner {
	return &Runner{scripts: map[string]Outcome{}, nextPID: 1000}
}

// compile-time proof the fake satisfies the seam it stands in for.
var _ exec.Runner = (*Runner)(nil)

// Script maps a program (matched against Spec.Argv[0]) to an Outcome.
func (r *Runner) Script(program string, o Outcome) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.scripts[program] = o
}

// SetDefault sets the Outcome used for programs with no explicit Script.
func (r *Runner) SetDefault(o Outcome) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.def = o
}

// Start streams the matched Outcome's output to the spec's writers, assigns a
// fake process-group id, and returns a Handle. A blocking Outcome yields a
// Handle whose Wait blocks until Kill or ctx cancellation.
func (r *Runner) Start(ctx context.Context, spec exec.Spec) (exec.Handle, error) {
	r.mu.Lock()
	program := ""
	if len(spec.Argv) > 0 {
		program = spec.Argv[0]
	}
	o, ok := r.scripts[program]
	if !ok {
		o = r.def
	}
	r.nextPID++
	pgid := r.nextPID
	r.mu.Unlock()

	// Stream the scripted output up front (the fake's "run"), so a caller sees
	// it even for a run cancelled mid-flight.
	if spec.Stdout != nil && o.Stdout != "" {
		if _, err := io.WriteString(spec.Stdout, o.Stdout); err != nil {
			return nil, err
		}
	}
	if spec.Stderr != nil && o.Stderr != "" {
		if _, err := io.WriteString(spec.Stderr, o.Stderr); err != nil {
			return nil, err
		}
	}

	h := &fakeHandle{pgid: pgid, block: o.Block, exit: o.Exit, done: make(chan struct{})}
	if o.Block {
		// A blocking run is unblocked by Kill or by context cancellation; the
		// watcher exits when the run is killed, so it never leaks.
		go func() {
			select {
			case <-ctx.Done():
				_ = h.Kill()
			case <-h.done:
			}
		}()
	}
	return h, nil
}

// fakeHandle is a started fake run: a fake process-group id and either an
// immediate scripted exit or a block until killed.
type fakeHandle struct {
	pgid  int
	block bool
	exit  int
	done  chan struct{}
	once  sync.Once
}

// PGID returns the fake process-group id.
func (h *fakeHandle) PGID() int { return h.pgid }

// Wait returns the scripted exit status immediately for a non-blocking run, or
// blocks until Kill/cancel and reports a signaled termination for a blocking one.
func (h *fakeHandle) Wait() (exec.ExitStatus, error) {
	if !h.block {
		return exec.ExitStatus{Code: h.exit}, nil
	}
	<-h.done
	return exec.ExitStatus{Code: -1, Signaled: true, Signal: syscall.SIGKILL}, nil
}

// Kill unblocks a blocking run, modeling a process-group kill.
func (h *fakeHandle) Kill() error {
	h.once.Do(func() { close(h.done) })
	return nil
}
