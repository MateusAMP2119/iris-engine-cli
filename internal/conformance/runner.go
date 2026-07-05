// Package conformance is the Iris conformance-tier harness: the shared runner
// the whole end-to-end suite uses to drive the actual shipped iris binary as a
// real process. It builds ./cmd/iris once, invokes it with arguments and
// environment, captures stdout/stderr/exit code, asserts exit codes, and parses
// the single-JSON-document --json envelope on stdout
// (S16/conformance-real-binary-json). From E02 onward the daemon helpers here
// also start the daemon over a unix socket and speak HTTP to it; see daemon.go.
//
// Conformance is the one tier with a real Postgres, created by the engine
// itself and hosting both databases -- the single place generated SQL meets a
// live database. Grants enforced, every write captured, wipes exact, standbys
// taking over: only a live database proves these, so this tier is where they are
// proven, against the real binary and nothing faked.
//
// # Acceptance scenario is this tier's spine (S16/acceptance-steps-cover-contracts)
//
// The doctrine this harness serves: each numbered step of the specification's
// acceptance scenario (spec section 15) is the conformance suite for its
// section's contracts. Driving the sample workspace end-to-end and asserting
// each step's exit codes and --json IS enforcing those sections' contracts, so
// "the sample passes" and "the spec is enforced" are one statement. As later
// epics fill the binary, their conformance tests reuse this harness to drive the
// matching acceptance step; the harness is deliberately generic so no step needs
// bespoke plumbing.
//
// This is test-support infrastructure: it imports testing and is meant to be
// used from _test.go files carrying the `conformance` build tag, which run only
// in the conformance CI job (real binary, real daemon, real Postgres).
package conformance

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"testing"
	"time"
)

// irisPkg is the import path of the iris command that Build compiles. Building
// by import path (rather than a relative ./cmd/iris) keeps Build independent of
// the caller's working directory within the module.
const irisPkg = "github.com/MateusAMP2119/iris-engine-cli/cmd/iris"

// defaultRunTimeout bounds a single binary invocation so a hung process fails
// the test promptly instead of the CI job's wall clock.
const defaultRunTimeout = 60 * time.Second

// Binary is a compiled iris binary on disk, ready to be driven as a real
// process. Build compiles it once; share the returned *Binary across a test's
// subtests rather than rebuilding per invocation.
type Binary struct {
	path string
}

// Build compiles ./cmd/iris into a fresh location under the test's temp
// directory and returns a Binary pointing at it, failing the test if the build
// fails. Go's build cache makes the compile step cheap on repeat, but callers
// should still build once per test run and share the result, since each call
// re-links a fresh executable.
func Build(t testing.TB) *Binary {
	t.Helper()
	out := filepath.Join(t.TempDir(), binName())
	cmd := exec.Command("go", "build", "-o", out, irisPkg)
	cmd.Env = os.Environ()
	if combined, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("conformance: go build %s failed: %v\n%s", irisPkg, err, combined)
	}
	return &Binary{path: out}
}

// Path is the filesystem path of the built binary, for callers that drive it
// through machinery beyond Run (for example, launching it as a daemon).
func (b *Binary) Path() string { return b.path }

// RunOptions configures a single invocation of the binary.
type RunOptions struct {
	// Args are the arguments after the program name.
	Args []string
	// Env is extra environment appended to the parent process's environment.
	Env []string
	// Dir is the working directory; the caller's directory is used when empty.
	Dir string
	// Stdin, when non-nil, is fed to the process's standard input.
	Stdin []byte
	// Timeout bounds the invocation; defaultRunTimeout is used when zero.
	Timeout time.Duration
}

// Result is the captured outcome of one invocation: the arguments, both output
// streams, and the process exit code.
type Result struct {
	Args     []string
	Stdout   []byte
	Stderr   []byte
	ExitCode int
}

// Run invokes the binary with opts and returns the captured Result. A non-zero
// exit code is data the caller asserts, never a test failure on its own: Run
// fails the test only when the process cannot be started or does not exit before
// its timeout. Assert the exit code with Result.RequireExit.
func (b *Binary) Run(t testing.TB, opts RunOptions) Result {
	t.Helper()

	timeout := opts.Timeout
	if timeout == 0 {
		timeout = defaultRunTimeout
	}
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()

	cmd := exec.CommandContext(ctx, b.path, opts.Args...)
	cmd.Dir = opts.Dir
	env := os.Environ()
	env = append(env, opts.Env...)
	cmd.Env = env
	if opts.Stdin != nil {
		cmd.Stdin = bytes.NewReader(opts.Stdin)
	}
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr

	err := cmd.Run()
	if errors.Is(ctx.Err(), context.DeadlineExceeded) {
		t.Fatalf("conformance: %s %v timed out after %s\nstdout:\n%s\nstderr:\n%s",
			b.path, opts.Args, timeout, stdout.Bytes(), stderr.Bytes())
	}

	res := Result{Args: opts.Args, Stdout: stdout.Bytes(), Stderr: stderr.Bytes()}
	if err == nil {
		return res
	}
	var exitErr *exec.ExitError
	if errors.As(err, &exitErr) {
		res.ExitCode = exitErr.ExitCode()
		return res
	}
	t.Fatalf("conformance: starting %s %v: %v", b.path, opts.Args, err)
	return res
}

// RequireExit fails the test unless the process exited with want, printing both
// output streams for diagnosis.
func (r Result) RequireExit(t testing.TB, want int) {
	t.Helper()
	if r.ExitCode != want {
		t.Fatalf("exit code = %d, want %d\nargs: %v\nstdout:\n%s\nstderr:\n%s",
			r.ExitCode, want, r.Args, r.Stdout, r.Stderr)
	}
}

// DecodeJSON asserts that stdout is exactly one JSON document -- the --json
// convention of a single structured envelope on stdout and nothing else -- and
// decodes it into v, failing the test on any decode error or trailing content.
func (r Result) DecodeJSON(t testing.TB, v any) {
	t.Helper()
	dec := json.NewDecoder(bytes.NewReader(r.Stdout))
	if err := dec.Decode(v); err != nil {
		t.Fatalf("--json: stdout is not one JSON document: %v\nstdout:\n%s", err, r.Stdout)
	}
	if _, err := dec.Token(); !errors.Is(err, io.EOF) {
		t.Fatalf("--json: stdout carries content after the single JSON document (err=%v)\nstdout:\n%s",
			err, r.Stdout)
	}
}

// binName is the built binary's filename for the host platform.
func binName() string {
	if runtime.GOOS == "windows" {
		return "iris.exe"
	}
	return "iris"
}
