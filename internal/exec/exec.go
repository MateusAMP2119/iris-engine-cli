// Package exec is the subprocess execution seam: the only code that spawns
// pipeline subprocesses (specification section 10). It owns direct exec (never a
// shell), process groups, kill/cancel, and output capture. A run's handle is its
// process-group id (runs.handle); the engine owns the run, captures its output,
// and cancels or kills it reliably (section 1).
//
// This package defines the seam and its real unix implementation (the seed of
// E05.1's exec seam), kept small and production-quality: Start(ctx, spec) yields
// a Handle over its own process group, which the caller Waits on and can Kill as
// a group. A fake (internal/exec/exectest) satisfies the same interface with no
// real process, so dispatch tests run runs, stream output, and cancel mid-flight
// with no OS process (S16/integration-fakes-interfaces); the real implementation
// here is exercised against throwaway scripts for real
// (S16/real-process-io-throwaway-scripts).
package exec

import (
	"context"
	"io"
	"syscall"
)

// Spec describes one subprocess to run. It is a direct exec of Argv -- never a
// shell, so Argv carries no pipes, globs, or metacharacter expansion -- in
// working directory Dir with environment Env, streaming stdout and stderr to the
// given writers.
type Spec struct {
	// Dir is the working directory (the pipeline folder). Empty means the
	// caller's current directory.
	Dir string
	// Argv is the program and its arguments; Argv[0] is the executable. It must
	// be non-empty.
	Argv []string
	// Env is the full child environment (os/exec semantics: a nil Env inherits
	// the parent's). The caller composes inherited + declared + injected entries.
	Env []string
	// Stdout receives the subprocess's standard output; nil discards it.
	Stdout io.Writer
	// Stderr receives the subprocess's standard error; nil discards it.
	Stderr io.Writer
}

// ExitStatus is the terminal outcome of a subprocess.
type ExitStatus struct {
	// Code is the exit code of a process that exited normally; -1 when the
	// process was terminated by a signal.
	Code int
	// Signaled reports whether the process was terminated by a signal (killed or
	// cancelled) rather than exiting on its own.
	Signaled bool
	// Signal is the terminating signal when Signaled; zero otherwise.
	Signal syscall.Signal
}

// Handle is a started subprocess: its process-group id (runs.handle) plus the
// operations the engine performs on it.
type Handle interface {
	// PGID returns the process-group id of the started subprocess. Killing the
	// negative of this value terminates the whole group.
	PGID() int
	// Wait blocks until the subprocess is reaped and returns its exit status. A
	// signaled (killed or cancelled) or non-zero termination is a terminal status,
	// not an error. After the subprocess exits, output capture is bounded: if a
	// destination writer fails, or a descendant that inherited the output pipe
	// holds it open past that bound, Wait returns a non-nil error alongside the
	// recorded exit status -- never a silent success -- and output the descendant
	// writes past the bound is truncated.
	Wait() (ExitStatus, error)
	// Kill terminates the whole process group with SIGKILL; an already-gone group
	// is not an error. Once the subprocess is reaped its pgid may in principle be
	// recycled -- an inherent POSIX race, since a pgid stays reserved only while
	// the group has a live member.
	Kill() error
}

// Runner starts subprocesses. The real implementation (NewOSRunner) spawns an OS
// process in its own process group; a fake starts scripted runs with no real
// process. Start honors ctx: cancelling it kills the process group.
type Runner interface {
	// Start spawns spec and returns a Handle over its process group. Cancelling
	// ctx kills that group.
	Start(ctx context.Context, spec Spec) (Handle, error)
}
