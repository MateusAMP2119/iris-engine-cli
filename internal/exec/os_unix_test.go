//go:build unix

package exec_test

import (
	"bufio"
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/MateusAMP2119/iris-engine-cli/internal/exec"
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

// TestOSRunnerImplementsRunner proves the real subprocess runner sits behind the
// exec seam, exactly as the fake does.
//
// spec: S16/real-process-io-throwaway-scripts
func TestOSRunnerImplementsRunner(_ *testing.T) {
	// Compile-time proof the real runner is assignable to the exec seam; the
	// behavioral tests below drive it through that seam.
	var _ exec.Runner = exec.NewOSRunner()
}

// TestOSRunnerCapturesOutput runs a real throwaway script and proves stdout and
// stderr are captured to the seam's writers, with a clean exit status.
//
// spec: S16/real-process-io-throwaway-scripts
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
//
// spec: S16/real-process-io-throwaway-scripts
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
//
// spec: S16/real-process-io-throwaway-scripts
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

// TestOSRunnerDirectExecNoShell proves the seam direct-execs argv without a
// shell: an argument full of shell metacharacters arrives at the program
// verbatim, never expanded or interpreted.
//
// spec: S16/real-process-io-throwaway-scripts
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
//
// spec: S16/real-process-io-throwaway-scripts
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
//
// spec: S16/real-process-io-throwaway-scripts
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

// TestOSRunnerWaitKeysOffChildReap proves Wait returns when the subprocess
// itself is reaped, not when every descendant that inherited its output pipe
// finally exits. A script backgrounds a long-lived grandchild -- which keeps the
// stdout pipe open -- and then exits 0; Wait must return promptly with the
// child's own exit status and its own captured output, never blocking for the
// grandchild's whole lifetime. Timing is asserted with a deadline, never a fixed
// sleep.
//
// spec: S16/real-process-io-throwaway-scripts
func TestOSRunnerWaitKeysOffChildReap(t *testing.T) {
	dir := t.TempDir()
	// The grandchild outlives the parent and inherits the parent's stdout, so a
	// runner that keys Wait off pipe EOF would block for the grandchild's whole
	// lifetime. The parent prints its own line and exits 0.
	script := writeScript(t, dir, "daemonize.sh", "sleep 30 &\necho done\nexit 0\n")

	// A non-*os.File writer is exactly the case os/exec would drain via a copy
	// goroutine that cmd.Wait blocks on, so it is where the grandchild stalls a
	// naive Wait.
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
	// Clear the whole group at the end: after the child is reaped Handle.Kill is a
	// documented no-op, so the lingering grandchild is reaped by pgid directly.
	pgid := h.PGID()
	t.Cleanup(func() { _ = syscall.Kill(-pgid, syscall.SIGKILL) })

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
		if r.err != nil {
			t.Fatalf("Wait: %v", r.err)
		}
		if r.st.Code != 0 || r.st.Signaled {
			t.Errorf("exit status = %+v, want code 0, not signaled", r.st)
		}
		if !strings.Contains(out.String(), "done") {
			t.Errorf("stdout = %q, want the child's own output 'done'", out.String())
		}
	case <-time.After(1 * time.Second):
		t.Fatalf("Wait did not return within the deadline: it is blocking on the backgrounded grandchild, not the child's reap")
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
