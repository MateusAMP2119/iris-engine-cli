// Package exectest provides a fake of the subprocess execution seam
// (internal/exec) with no real process. Runs are scripted: each program name
// maps to an Outcome that streams stdout/stderr to the seam's writers and either
// exits with a code or blocks until cancelled. It lets dispatch tests start runs
// in composer order, stream their output, and cancel a run mid-flight with no OS
// process.
//
// This is test-support infrastructure imported only by _test.go files.
package exectest

import (
	"context"
	"errors"
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
	// Stdout is streamed to the spec's stdout writer after the run starts.
	Stdout string
	// Stderr is streamed to the spec's stderr writer after the run starts.
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

// Start assigns a fake process-group id, launches the matched Outcome's output
// streaming on its own goroutine, and returns a Handle. It mirrors the real seam:
// Start returns immediately (never blocking on a backpressured writer), output is
// delivered concurrently with the handle's lifetime, and any writer error
// surfaces from Wait rather than Start. Argv must be non-empty, the precondition
// the real runner enforces. A blocking Outcome yields a Handle whose Wait blocks
// until Kill or ctx cancellation.
func (r *Runner) Start(ctx context.Context, spec exec.Spec) (exec.Handle, error) {
	if len(spec.Argv) == 0 {
		return nil, errors.New("exec: empty argv")
	}

	r.mu.Lock()
	program := spec.Argv[0]
	o, ok := r.scripts[program]
	if !ok {
		o = r.def
	}
	r.nextPID++
	pgid := r.nextPID
	r.mu.Unlock()

	h := &fakeHandle{
		pgid:     pgid,
		block:    o.Block,
		exit:     o.Exit,
		done:     make(chan struct{}),
		streamed: make(chan struct{}),
	}

	// Stream the scripted output asynchronously (the fake's "run"): Start returns
	// at once, the output arrives concurrently with the handle's lifetime -- so a
	// cancel can interleave with streaming and a backpressured writer never blocks
	// Start -- and a writer error is held for Wait to report.
	go func() {
		defer close(h.streamed)
		if spec.Stdout != nil && o.Stdout != "" {
			if _, err := io.WriteString(spec.Stdout, o.Stdout); err != nil {
				h.streamErr = err
				return
			}
		}
		if spec.Stderr != nil && o.Stderr != "" {
			if _, err := io.WriteString(spec.Stderr, o.Stderr); err != nil {
				h.streamErr = err
				return
			}
		}
	}()

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

// fakeHandle is a started fake run: a fake process-group id, the streaming
// goroutine's completion signal, and either an immediate scripted exit or a
// block until killed.
type fakeHandle struct {
	pgid      int
	block     bool
	exit      int
	done      chan struct{}
	streamed  chan struct{}
	streamErr error // writer error, read only after streamed is closed
	once      sync.Once
}

// PGID returns the fake process-group id.
func (h *fakeHandle) PGID() int { return h.pgid }

// Wait reports the run's terminal status once its output has finished streaming:
// the scripted exit status for a non-blocking run, or a signaled termination
// after Kill/cancel for a blocking one. A writer error seen while streaming is
// surfaced here rather than from Start, alongside that recorded terminal status
// (never a zero one). It mirrors the real runner's precedence: the error surfaces
// only when the run otherwise exited cleanly (code 0, not signaled), because a
// non-clean terminal status subsumes it just as os/exec's ExitError subsumes a
// copy error. Because Wait waits for streaming to finish, the captured output is
// complete and safe to read after it returns.
func (h *fakeHandle) Wait() (exec.ExitStatus, error) {
	if h.block {
		<-h.done // Kill or ctx cancellation unblocks the run
	}
	<-h.streamed // streaming finished: output complete and streamErr stable

	st := exec.ExitStatus{Code: h.exit}
	if h.block {
		st = exec.ExitStatus{Code: -1, Signaled: true, Signal: syscall.SIGKILL}
	}
	if h.streamErr != nil && st.Code == 0 && !st.Signaled {
		return st, h.streamErr
	}
	return st, nil
}

// Kill unblocks a blocking run, modeling a process-group kill.
func (h *fakeHandle) Kill() error {
	h.once.Do(func() { close(h.done) })
	return nil
}
