//go:build unix

package exec_test

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	osexec "os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/MateusAMP2119/iris-lakehouse/internal/exec"
)

// writeScript writes an executable throwaway /bin/sh script into dir and returns
// its absolute path. The real exec seam direct-execs it (shebang), never a shell
// the engine spawns.
func writeScript(t *testing.T, dir, name, body string) string {
	t.Helper()
	path := filepath.Join(dir, name)
	if err := os.WriteFile(path, []byte("#!/bin/sh\n"+body), 0o755); err != nil {
		t.Fatalf("write script %s: %v", path, err)
	}
	return path
}

// TestOSRunnerCapturesOutput runs a real throwaway script and proves stdout and
// stderr are captured to the seam's writers, with a clean exit status.
func TestOSRunnerCapturesOutput(t *testing.T) {
	dir := t.TempDir()
	script := writeScript(t, dir, "echo.sh", "echo out-line\necho err-line 1>&2\n")

	var out, errb bytes.Buffer
	h, err := exec.NewOSRunner().Start(context.Background(), exec.Spec{
		Argv:   []string{script},
		Env:    os.Environ(),
		Stdout: &out,
		Stderr: &errb,
	})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	if h.PGID() <= 0 {
		t.Errorf("PGID() = %d, want the real process-group id", h.PGID())
	}
	st, err := h.Wait()
	if err != nil {
		t.Fatalf("Wait: %v", err)
	}
	if st.Code != 0 || st.Signaled {
		t.Errorf("exit status = %+v, want code 0, not signaled", st)
	}
	if !strings.Contains(out.String(), "out-line") {
		t.Errorf("stdout = %q, want it to contain out-line", out.String())
	}
	if !strings.Contains(errb.String(), "err-line") {
		t.Errorf("stderr = %q, want it to contain err-line", errb.String())
	}
}

// TestOSRunnerExitCode proves a non-zero exit code is reported through the exit
// status, not swallowed and not surfaced as a Go error.
func TestOSRunnerExitCode(t *testing.T) {
	dir := t.TempDir()
	script := writeScript(t, dir, "fail.sh", "exit 7\n")

	h, err := exec.NewOSRunner().Start(context.Background(), exec.Spec{Argv: []string{script}, Env: os.Environ()})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	st, err := h.Wait()
	if err != nil {
		t.Fatalf("Wait: %v", err)
	}
	if st.Code != 7 || st.Signaled {
		t.Errorf("exit status = %+v, want code 7, not signaled", st)
	}
}

// TestOSRunnerCwdAndEnv proves the seam runs the subprocess with the pipeline
// folder as working directory and injects the declared environment.
func TestOSRunnerCwdAndEnv(t *testing.T) {
	dir := t.TempDir()
	workdir := filepath.Join(dir, "pipeline_folder")
	if err := os.Mkdir(workdir, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	script := writeScript(t, dir, "report.sh", "printf 'pwd=%s\\n' \"$(pwd -P)\"\nprintf 'var=%s\\n' \"$IRIS_TEST_VAR\"\n")

	var out bytes.Buffer
	h, err := exec.NewOSRunner().Start(context.Background(), exec.Spec{
		Argv:   []string{script},
		Dir:    workdir,
		Env:    append(os.Environ(), "IRIS_TEST_VAR=injected-value"),
		Stdout: &out,
	})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	if _, err := h.Wait(); err != nil {
		t.Fatalf("Wait: %v", err)
	}

	wantPwd, err := filepath.EvalSymlinks(workdir)
	if err != nil {
		t.Fatalf("eval symlinks: %v", err)
	}
	if !strings.Contains(out.String(), "pwd="+wantPwd+"\n") {
		t.Errorf("output = %q, want working dir %q", out.String(), wantPwd)
	}
	if !strings.Contains(out.String(), "var=injected-value\n") {
		t.Errorf("output = %q, want injected env var", out.String())
	}
}

// TestOSRunnerDirectExecNoShell proves the seam direct-execs the argv vector and
// never runs it through a shell: a shell metacharacter argument -- a variable, a
// command chain, a glob -- reaches the process verbatim, with no expansion,
// substitution, or word splitting. The run vector is a plain direct-exec argv,
// no shell.
func TestOSRunnerDirectExecNoShell(t *testing.T) {
	dir := t.TempDir()
	script := writeScript(t, dir, "arg.sh", "printf 'arg=%s' \"$1\"\n")
	const raw = "$HOME && echo pwned; rm -rf /*"

	var out bytes.Buffer
	h, err := exec.NewOSRunner().Start(context.Background(), exec.Spec{
		Argv:   []string{script, raw},
		Env:    os.Environ(),
		Stdout: &out,
	})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	if _, err := h.Wait(); err != nil {
		t.Fatalf("Wait: %v", err)
	}
	if out.String() != "arg="+raw {
		t.Errorf("output = %q, want the metacharacter argument passed verbatim: arg=%s", out.String(), raw)
	}
}

// TestOSRunnerKillKillsGroup proves cancel/kill reaches the whole process group,
// not just the direct child: a script forks a long-lived grandchild, and killing
// the group reaps both. The parent's Wait reports a signaled terminal status.
// Synchronization is on the script's own "ready" output and on process liveness,
// never a fixed sleep.
func TestOSRunnerKillKillsGroup(t *testing.T) {
	dir := t.TempDir()
	// The script forks a grandchild that outlives a naive parent-only kill, then
	// announces the grandchild pid and readiness before blocking.
	script := writeScript(t, dir, "hang.sh", "sleep 300 &\nchild=$!\necho \"child=$child\"\necho ready\nwait\n")

	pr, pw, err := os.Pipe()
	if err != nil {
		t.Fatalf("pipe: %v", err)
	}
	defer func() { _ = pr.Close() }()

	h, err := exec.NewOSRunner().Start(context.Background(), exec.Spec{
		Argv:   []string{script},
		Env:    os.Environ(),
		Stdout: pw,
		Stderr: pw,
	})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	_ = pw.Close() // parent drops its write end; the child holds its own

	// Read until the script is up and we know the grandchild pid: pure output
	// synchronization, no sleep.
	childPid := 0
	sc := bufio.NewScanner(pr)
	for sc.Scan() {
		line := sc.Text()
		if rest, ok := strings.CutPrefix(line, "child="); ok {
			childPid, _ = strconv.Atoi(rest)
		}
		if line == "ready" {
			break
		}
	}
	if childPid <= 0 {
		t.Fatalf("did not read grandchild pid from script output")
	}
	if !processAlive(childPid) {
		t.Fatalf("grandchild %d not alive before kill", childPid)
	}

	// Kill the whole group.
	if err := h.Kill(); err != nil {
		t.Fatalf("Kill: %v", err)
	}

	// The parent (group leader) terminates by signal.
	st, err := h.Wait()
	if err != nil {
		t.Fatalf("Wait: %v", err)
	}
	if !st.Signaled {
		t.Errorf("exit status = %+v, want a signaled termination", st)
	}

	// The grandchild is reaped too: the group kill reached it. Wait on liveness
	// with a deadline; never a fixed sleep.
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if alive := pollUntil(ctx, func() bool { return !processAlive(childPid) }); !alive {
		t.Errorf("grandchild %d survived the group kill", childPid)
	}
}

// TestOSRunnerContextCancelKillsGroup proves the other cancel path: cancelling
// the context passed to Start kills the process group.
func TestOSRunnerContextCancelKillsGroup(t *testing.T) {
	dir := t.TempDir()
	script := writeScript(t, dir, "hang.sh", "echo ready\nsleep 300\n")

	pr, pw, err := os.Pipe()
	if err != nil {
		t.Fatalf("pipe: %v", err)
	}
	defer func() { _ = pr.Close() }()

	ctx, cancel := context.WithCancel(context.Background())
	h, err := exec.NewOSRunner().Start(ctx, exec.Spec{
		Argv:   []string{script},
		Env:    os.Environ(),
		Stdout: pw,
		Stderr: pw,
	})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	_ = pw.Close()

	sc := bufio.NewScanner(pr)
	for sc.Scan() {
		if sc.Text() == "ready" {
			break
		}
	}
	cancel() // cancel mid-flight

	st, err := h.Wait()
	if err != nil {
		t.Fatalf("Wait: %v", err)
	}
	if !st.Signaled {
		t.Errorf("exit status = %+v, want a signaled termination from context cancel", st)
	}
}

// TestOSRunnerGrandchildBoundedByWaitDelay proves a pipeline that backgrounds a
// long-lived grandchild -- which inherits and holds the output pipe open past the
// child's own exit -- does not stall Wait for the grandchild's lifetime. os/exec's
// WaitDelay bounds the post-reap output drain: Wait returns within WaitDelay of
// the child's exit, carrying the child's own recorded status and its own output,
// and the drain that could not finish surfaces as ErrWaitDelay -- never a silent
// success. Timing is asserted with a deadline, never a fixed sleep.
func TestOSRunnerGrandchildBoundedByWaitDelay(t *testing.T) {
	dir := t.TempDir()
	// The grandchild outlives the parent and inherits its stdout, keeping the pipe
	// open; the parent prints its own line and exits 0.
	script := writeScript(t, dir, "daemonize.sh", "sleep 30 &\necho done\nexit 0\n")

	// A non-*os.File writer routes output through an os/exec copy goroutine, the
	// path a lingering grandchild can hold open; WaitDelay is what bounds it.
	var out bytes.Buffer
	h, err := exec.NewOSRunner().Start(context.Background(), exec.Spec{
		Argv:   []string{script},
		Env:    os.Environ(),
		Stdout: &out,
		Stderr: &out,
	})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	// The seam's Kill sweeps the still-live grandchild through the group.
	t.Cleanup(func() { _ = h.Kill() })

	type result struct {
		st  exec.ExitStatus
		err error
	}
	done := make(chan result, 1)
	go func() {
		st, werr := h.Wait()
		done <- result{st, werr}
	}()

	select {
	case r := <-done:
		if !errors.Is(r.err, osexec.ErrWaitDelay) {
			t.Errorf("Wait error = %v, want ErrWaitDelay (drain bounded, never a silent success)", r.err)
		}
		if r.st.Code != 0 || r.st.Signaled {
			t.Errorf("exit status = %+v, want the child's own code 0, not signaled", r.st)
		}
		if !strings.Contains(out.String(), "done") {
			t.Errorf("stdout = %q, want the child's own output 'done'", out.String())
		}
	case <-time.After(4 * time.Second):
		t.Fatalf("Wait did not return within the WaitDelay bound: it is blocking on the backgrounded grandchild")
	}
}

// TestOSRunnerSharedWriterCapturesAllBytes proves stdout and stderr aimed at the
// same writer are captured through a single pump, with no concurrent Write and no
// dropped bytes: a script interleaves many lines across both streams into one
// shared buffer, and every line comes back. Two independent pumps on one writer
// would race and lose output.
func TestOSRunnerSharedWriterCapturesAllBytes(t *testing.T) {
	dir := t.TempDir()
	const perStream = 1000
	// Heavy interleaving across both streams into one writer: two pumps would
	// call Write on it concurrently.
	body := fmt.Sprintf("i=0\nwhile [ $i -lt %d ]; do echo \"out-$i\"; echo \"err-$i\" 1>&2; i=$((i+1)); done\n", perStream)
	script := writeScript(t, dir, "interleave.sh", body)

	// One plain buffer for both streams: safe only because a shared writer is fed
	// by a single pump.
	var buf bytes.Buffer
	h, err := exec.NewOSRunner().Start(context.Background(), exec.Spec{
		Argv:   []string{script},
		Env:    os.Environ(),
		Stdout: &buf,
		Stderr: &buf,
	})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	st, err := h.Wait()
	if err != nil {
		t.Fatalf("Wait: %v", err)
	}
	if st.Code != 0 || st.Signaled {
		t.Errorf("exit status = %+v, want code 0, not signaled", st)
	}
	if got, want := strings.Count(buf.String(), "\n"), 2*perStream; got != want {
		t.Errorf("captured %d lines, want %d (a concurrent pump dropped output)", got, want)
	}
}

// throttledWriter delays every Write, modeling a destination slower than real
// time. The delay models writer slowness; it is not a test-synchronization sleep.
type throttledWriter struct {
	mu    sync.Mutex
	buf   bytes.Buffer
	delay time.Duration
}

func (w *throttledWriter) Write(p []byte) (int, error) {
	time.Sleep(w.delay)
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.buf.Write(p)
}

func (w *throttledWriter) len() int {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.buf.Len()
}

// TestOSRunnerSlowWriterCapturesBufferedOutput proves a destination writer slower
// than real time never truncates the child's own output when the drain finishes
// within WaitDelay: os/exec copies to EOF at the child's own pace, and with no
// lingering descendant EOF arrives at the child's exit. A blob larger than the
// pipe buffer is streamed through a throttled writer and every byte comes back
// with a clean status.
func TestOSRunnerSlowWriterCapturesBufferedOutput(t *testing.T) {
	dir := t.TempDir()
	const blob = 96 * 1024
	// No backgrounded descendant: the pipe reaches EOF at the child's exit, so the
	// copy completes regardless of writer speed, well inside WaitDelay.
	body := fmt.Sprintf("head -c %d /dev/zero | tr '\\0' x\nexit 0\n", blob)
	script := writeScript(t, dir, "bigdump.sh", body)

	// Slow, but the whole blob drains in a handful of 32KiB writes, far inside the
	// 2s WaitDelay bound.
	w := &throttledWriter{delay: 40 * time.Millisecond}
	h, err := exec.NewOSRunner().Start(context.Background(), exec.Spec{
		Argv:   []string{script},
		Env:    os.Environ(),
		Stdout: w,
	})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	st, err := h.Wait()
	if err != nil {
		t.Fatalf("Wait: %v", err)
	}
	if st.Code != 0 || st.Signaled {
		t.Errorf("exit status = %+v, want code 0, not signaled", st)
	}
	if got := w.len(); got != blob {
		t.Errorf("captured %d bytes, want the child's full %d bytes", got, blob)
	}
}

// errWriter fails every Write with a sentinel, standing in for a failed output
// sink (full disk, closed connection).
type errWriter struct{}

var errSink = errors.New("sink failed")

func (errWriter) Write([]byte) (int, error) { return 0, errSink }

// TestOSRunnerWriterErrorSurfacesFromWait proves a destination-writer failure is
// reported from Wait -- promptly and never as a silent success -- alongside the
// child's recorded exit status, even when the child emits far more than the pipe
// buffer. os/exec closes the pipe read end when the copy stops, delivering EPIPE
// so the child is never wedged in write(2); Wait returns quickly rather than
// hanging. Timing is asserted with a deadline, never a fixed sleep.
func TestOSRunnerWriterErrorSurfacesFromWait(t *testing.T) {
	dir := t.TempDir()
	// Far more than the ~64KiB pipe buffer, so a runner that abandoned the pipe on
	// a writer error would wedge the child mid-write forever.
	script := writeScript(t, dir, "flood.sh", "head -c 1048576 /dev/zero | tr '\\0' x\nexit 0\n")

	h, err := exec.NewOSRunner().Start(context.Background(), exec.Spec{
		Argv:   []string{script},
		Env:    os.Environ(),
		Stdout: errWriter{},
	})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}

	type result struct {
		st  exec.ExitStatus
		err error
	}
	done := make(chan result, 1)
	go func() {
		st, werr := h.Wait()
		done <- result{st, werr}
	}()

	select {
	case r := <-done:
		if !errors.Is(r.err, errSink) {
			t.Errorf("Wait error = %v, want the destination-writer error surfaced", r.err)
		}
		if r.st.Code != 0 || r.st.Signaled {
			t.Errorf("exit status = %+v, want the child's recorded exit 0 alongside the error", r.st)
		}
	case <-time.After(4 * time.Second):
		t.Fatalf("Wait hung on the failed writer; os/exec must EPIPE the child and surface the error promptly")
	}
}

// processAlive reports whether pid names a live (or not-yet-reaped) process, via
// the null signal.
func processAlive(pid int) bool {
	return syscall.Kill(pid, 0) == nil
}

// pollUntil returns true once cond holds, polling until it does or ctx is done.
// It is the no-fixed-sleeps convention: wait on state with a deadline, never
// time.Sleep a guessed duration.
func pollUntil(ctx context.Context, cond func() bool) bool {
	tick := time.NewTicker(2 * time.Millisecond)
	defer tick.Stop()
	for {
		if cond() {
			return true
		}
		select {
		case <-ctx.Done():
			return cond()
		case <-tick.C:
		}
	}
}
