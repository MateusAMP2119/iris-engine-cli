//go:build unix

package exec

import (
	"context"
	"errors"
	"fmt"
	"os"
	osexec "os/exec"
	"syscall"
	"time"
)

// OSRunner is the real subprocess runner: it spawns an OS process in its own
// process group, captures its output through the standard library, and kills the
// whole group on Kill or context cancellation. It is the seed of E05.1's exec
// seam. Unix only (darwin + linux); Windows is deferred from v1 (section 16).
type OSRunner struct{}

// NewOSRunner returns the real subprocess runner.
func NewOSRunner() *OSRunner { return &OSRunner{} }

// compile-time proof the real runner satisfies the seam.
var _ Runner = (*OSRunner)(nil)

// waitDelay bounds the time cmd.Wait spends, after the subprocess has exited,
// waiting for the standard library's output copy goroutines to finish. It is the
// stdlib mechanism built for exactly the daemonizing-pipeline problem: a
// descendant that inherited the output pipe can keep it open indefinitely, so
// once the child is reaped os/exec waits at most this long for the copiers, then
// closes the pipes itself and Wait returns ErrWaitDelay. Output the subprocess
// wrote before it exited is captured while it drains within this bound; a
// descendant's output past it is truncated, and the truncation surfaces as
// ErrWaitDelay rather than a silent success.
const waitDelay = 2 * time.Second

// Start spawns spec as a direct exec (never a shell) in its own process group,
// streaming stdout and stderr to the spec's writers via the standard library.
// The returned Handle's PGID is the new group's id. When ctx is cancelled the
// group is killed.
//
// Wait returns once the subprocess itself is reaped, bounded after that by
// waitDelay for output to drain. When Stdout and Stderr are the same comparable
// writer, os/exec feeds both from one pipe, so they are never written
// concurrently. If a destination writer fails, or a descendant holds the output
// pipe open past waitDelay, Wait returns a non-nil error alongside the recorded
// exit status; a descendant's output past waitDelay is truncated.
func (r *OSRunner) Start(ctx context.Context, spec Spec) (Handle, error) {
	if len(spec.Argv) == 0 {
		return nil, errors.New("exec: empty argv")
	}
	cmd := osexec.Command(spec.Argv[0], spec.Argv[1:]...)
	cmd.Dir = spec.Dir
	cmd.Env = spec.Env
	// Assign the writers directly: os/exec owns the pipes and copy goroutines,
	// dedups a shared stdout/stderr writer to a single copier, and closes the pipe
	// read end when a copy stops -- delivering EPIPE to a child whose sink failed.
	cmd.Stdout = spec.Stdout
	cmd.Stderr = spec.Stderr
	// Setpgid puts the child in a new process group whose id equals its pid, so
	// killing the negative pgid reaches the child and every descendant.
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	// After the child exits, wait at most waitDelay for the output copiers before
	// os/exec closes the pipes and Wait returns ErrWaitDelay: a daemonizing
	// pipeline never stalls Wait for a descendant's lifetime.
	cmd.WaitDelay = waitDelay

	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("exec: start %v: %w", spec.Argv, err)
	}

	h := &osHandle{cmd: cmd, pgid: cmd.Process.Pid, done: make(chan struct{})}

	// Reap on a Start-owned goroutine so Handle.Wait can be called (or not) freely;
	// cmd.Wait bounds itself by waitDelay, so done closes at child exit + at most
	// waitDelay.
	go h.reap()

	// Cancelling ctx kills the group. The watcher stays armed until reap (done);
	// its window for signalling a recycled pgid is bounded by waitDelay after the
	// group can empty, the accepted trade for a bounded, dependency-proof Wait.
	go func() {
		select {
		case <-ctx.Done():
			_ = h.Kill()
		case <-h.done:
		}
	}()

	return h, nil
}

// osHandle is a started OS subprocess and its process group.
type osHandle struct {
	cmd     *osexec.Cmd
	pgid    int
	done    chan struct{} // closed when the child is reaped and its status recorded
	status  ExitStatus
	waitErr error
}

// reap waits on the child and records its terminal status and any output-drain
// error, then closes done. Writes here happen-before any Wait read via the done
// channel, so no lock is needed.
func (h *osHandle) reap() {
	h.status, h.waitErr = translateWait(h.cmd.Wait(), h.cmd)
	close(h.done)
}

// PGID returns the subprocess's process-group id.
func (h *osHandle) PGID() int { return h.pgid }

// Wait waits for the subprocess to be reaped and returns its exit status. A
// signaled or non-zero termination is a terminal status, not an error. After the
// child exits, output drain is bounded by waitDelay: a destination-writer failure
// or a drain that exceeds waitDelay surfaces as a non-nil error alongside the
// recorded exit status -- never a silent success -- and a descendant's output past
// waitDelay is truncated.
func (h *osHandle) Wait() (ExitStatus, error) {
	<-h.done
	return h.status, h.waitErr
}

// Kill terminates the whole process group with SIGKILL. Killing the negated pgid
// signals every member of the group; an already-gone group (ESRCH) is not an
// error. Once the subprocess is reaped its pgid may in principle be recycled -- an
// inherent POSIX race a caller cannot fully avoid, since a pgid is reserved only
// while the group has a live member.
func (h *osHandle) Kill() error {
	if err := syscall.Kill(-h.pgid, syscall.SIGKILL); err != nil {
		if errors.Is(err, syscall.ESRCH) {
			return nil // group already gone
		}
		return fmt.Errorf("exec: kill group %d: %w", h.pgid, err)
	}
	return nil
}

// translateWait maps an os/exec Wait error into an ExitStatus plus the error the
// seam surfaces. The exit status always comes from the recorded ProcessState. A
// signaled or non-zero exit (an *ExitError) is a terminal status with no error; a
// destination-writer error or ErrWaitDelay (the post-reap drain exceeded
// waitDelay) is surfaced alongside that status.
func translateWait(err error, cmd *osexec.Cmd) (ExitStatus, error) {
	st := statusOf(cmd.ProcessState)
	if err == nil {
		return st, nil
	}
	var ee *osexec.ExitError
	if errors.As(err, &ee) {
		return st, nil
	}
	return st, fmt.Errorf("exec: wait %v: %w", cmd.Args, err)
}

// statusOf reads the terminal ExitStatus from a finished process's state,
// reporting a signaled termination as a terminal status.
func statusOf(ps *os.ProcessState) ExitStatus {
	if ps == nil {
		return ExitStatus{}
	}
	if ws, ok := ps.Sys().(syscall.WaitStatus); ok {
		if ws.Signaled() {
			return ExitStatus{Code: -1, Signaled: true, Signal: ws.Signal()}
		}
		return ExitStatus{Code: ws.ExitStatus()}
	}
	return ExitStatus{Code: ps.ExitCode()}
}
