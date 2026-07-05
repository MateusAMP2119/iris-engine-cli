//go:build unix

package exec

import (
	"context"
	"errors"
	"fmt"
	osexec "os/exec"
	"sync"
	"syscall"
)

// OSRunner is the real subprocess runner: it spawns an OS process in its own
// process group, captures its output, and kills the whole group on Kill or
// context cancellation. It is the seed of E05.1's exec seam. Unix only
// (darwin + linux); Windows is deferred from v1 (section 16).
type OSRunner struct{}

// NewOSRunner returns the real subprocess runner.
func NewOSRunner() *OSRunner { return &OSRunner{} }

// compile-time proof the real runner satisfies the seam.
var _ Runner = (*OSRunner)(nil)

// Start spawns spec as a direct exec (never a shell) in its own process group,
// streaming stdout and stderr to the spec's writers. The returned Handle's PGID
// is the new group's id. When ctx is cancelled the group is killed.
func (r *OSRunner) Start(ctx context.Context, spec Spec) (Handle, error) {
	if len(spec.Argv) == 0 {
		return nil, errors.New("exec: empty argv")
	}
	cmd := osexec.Command(spec.Argv[0], spec.Argv[1:]...)
	cmd.Dir = spec.Dir
	cmd.Env = spec.Env
	cmd.Stdout = spec.Stdout
	cmd.Stderr = spec.Stderr
	// Setpgid puts the child in a new process group whose id equals its pid, so
	// killing the negative pgid reaches the child and every descendant.
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}

	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("exec: start %v: %w", spec.Argv, err)
	}

	h := &osHandle{cmd: cmd, pgid: cmd.Process.Pid, done: make(chan struct{})}

	// Cancelling ctx kills the group; the watcher exits when the process is
	// waited on, so it never leaks.
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
	cmd  *osexec.Cmd
	pgid int
	done chan struct{}
	once sync.Once
}

// PGID returns the subprocess's process-group id.
func (h *osHandle) PGID() int { return h.pgid }

// Wait waits for the subprocess to terminate and returns its exit status,
// translating a signaled termination into a terminal status rather than an
// error.
func (h *osHandle) Wait() (ExitStatus, error) {
	err := h.cmd.Wait()
	h.once.Do(func() { close(h.done) })

	if err == nil {
		return ExitStatus{Code: 0}, nil
	}
	var ee *osexec.ExitError
	if errors.As(err, &ee) {
		if ws, ok := ee.Sys().(syscall.WaitStatus); ok {
			if ws.Signaled() {
				return ExitStatus{Code: -1, Signaled: true, Signal: ws.Signal()}, nil
			}
			return ExitStatus{Code: ws.ExitStatus()}, nil
		}
		return ExitStatus{Code: ee.ExitCode()}, nil
	}
	return ExitStatus{}, fmt.Errorf("exec: wait %v: %w", h.cmd.Args, err)
}

// Kill terminates the whole process group with SIGKILL. Killing the negated pgid
// signals every member of the group.
func (h *osHandle) Kill() error {
	if err := syscall.Kill(-h.pgid, syscall.SIGKILL); err != nil {
		if errors.Is(err, syscall.ESRCH) {
			return nil // group already gone
		}
		return fmt.Errorf("exec: kill group %d: %w", h.pgid, err)
	}
	return nil
}
