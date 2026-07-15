//go:build unix

package dispatch_test

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/MateusAMP2119/iris-lakehouse/internal/dispatch"
	"github.com/MateusAMP2119/iris-lakehouse/internal/exec"
	"github.com/MateusAMP2119/iris-lakehouse/internal/store"
	"github.com/MateusAMP2119/iris-lakehouse/internal/store/storetest"
)

// --- test-only run-log seam (a fake internal/daemon RunLogWriter) --------------

// lockedBuffer is a threadsafe io.WriteCloser standing in for a per-run log file:
// os/exec's copy goroutine writes to it concurrently with the test reading it, so
// every access is mutex-guarded to stay clean under -race.
type lockedBuffer struct {
	mu     sync.Mutex
	b      strings.Builder
	closed bool
}

func (w *lockedBuffer) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.b.Write(p)
}

func (w *lockedBuffer) Close() error {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.closed = true
	return nil
}

func (w *lockedBuffer) String() string {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.b.String()
}

// fakeLog is a dispatch.RunLog: it hands out one lockedBuffer per run id so a test
// can read back exactly what the engine captured for a run, with no real file.
type fakeLog struct {
	mu   sync.Mutex
	bufs map[string]*lockedBuffer
}

func newFakeLog() *fakeLog { return &fakeLog{bufs: map[string]*lockedBuffer{}} }

func (l *fakeLog) Open(runID string) (dispatch.WriteCloser, string, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	b := &lockedBuffer{}
	l.bufs[runID] = b
	return b, "logs/run-" + runID + ".log", nil
}

func (l *fakeLog) contents(runID string) string {
	l.mu.Lock()
	b := l.bufs[runID]
	l.mu.Unlock()
	if b == nil {
		return ""
	}
	return b.String()
}

// --- harness -------------------------------------------------------------------

// harness wires a RunManager to the real OS runner, a real dispatcher over a
// recording meta-write fake (no live Postgres), and a fake run-log seam. Every run
// it starts is cancelled on cleanup, so no throwaway process outlives the test.
type harness struct {
	t   *testing.T
	mgr *dispatch.RunManager
	rec *storetest.WriteRecorder
	log *fakeLog
	ctx context.Context
}

func newHarness(t *testing.T) *harness {
	t.Helper()
	ctx := context.Background()
	rec := storetest.NewWriteRecorder()
	disp := dispatch.New(rec)
	disp.Start(ctx)
	t.Cleanup(disp.Stop)
	log := newFakeLog()
	mgr := dispatch.NewRunManager(exec.NewOSRunner(), disp, log)
	return &harness{t: t, mgr: mgr, rec: rec, log: log, ctx: ctx}
}

// start starts a run and registers a cleanup that cancels it, so a blocking
// throwaway script never lingers past the test even on a failing assertion.
func (h *harness) start(spec dispatch.RunSpec) dispatch.RunHandle {
	h.t.Helper()
	rh, err := h.mgr.StartRun(h.ctx, spec)
	if err != nil {
		h.t.Fatalf("StartRun(%s): %v", spec.RunID, err)
	}
	h.t.Cleanup(func() { _ = h.mgr.CancelRun(context.Background(), spec.RunID) })
	return rh
}

// writeScript writes an executable throwaway script into dir and returns its path.
func writeScript(t *testing.T, dir, name, body string) string {
	t.Helper()
	path := filepath.Join(dir, name)
	if err := os.WriteFile(path, []byte(body), 0o755); err != nil {
		t.Fatalf("write script %s: %v", path, err)
	}
	return path
}

// waitFor polls cond until it is true or the deadline elapses; the poll interval is
// a tiny bound, never a fixed wait-for-event sleep.
func waitFor(t *testing.T, cond func() bool, msg string) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatalf("condition not met within deadline: %s", msg)
}

// groupAlive reports whether any process in the group survives, probing with signal
// 0 (existence check, no signal delivered): nil while the group has a live member,
// ESRCH once it is empty.
func groupAlive(pgid int) bool { return syscall.Kill(-pgid, 0) == nil }

// blockingScript prints a "started" marker then loops forever, so a test can poll
// its output to know it is running and then kill its group to end it.
const blockingScript = "#!/bin/sh\nprintf 'started\\n'\nwhile true; do sleep 1; done\n"

// markRunningPGID returns the process-group id recorded by the run-start write for
// runID, if the single writer recorded one.
func markRunningPGID(stmts []storetest.RecordedStatement, runID string) (int, bool) {
	// The mark-running args are (state, pgid, log_ref, id, guard-state).
	for _, s := range stmts {
		if len(s.Args) >= 4 && s.Args[0] == store.RunRunning && s.Args[3] == runID {
			if pgid, ok := s.Args[1].(int); ok {
				return pgid, true
			}
		}
	}
	return 0, false
}

// deadLetteredStopped reports whether the single writer recorded a dead-letter of
// runID with the reason token "stopped".
func deadLetteredStopped(stmts []storetest.RecordedStatement, runID string) bool {
	for _, s := range stmts {
		if len(s.Args) >= 4 && s.Args[0] == store.RunDeadLettered &&
			s.Args[1] == runID && s.Args[3] == store.ReasonStopped {
			return true
		}
	}
	return false
}

// --- tests ---------------------------------------------------------------------

// TestStartRunDirectExecNoShell proves the engine starts a run by direct exec of the
// declared argv, never through a shell: shell metacharacters in arguments -- a pipe,
// a glob, a variable, a command substitution, a semicolon -- reach the process
// literally, with no interpretation.
func TestStartRunDirectExecNoShell(t *testing.T) {
	h := newHarness(t)
	dir := t.TempDir()
	script := writeScript(t, dir, "echo-args.sh",
		"#!/bin/sh\nfor a in \"$@\"; do printf '%s\\n' \"$a\"; done\n")

	metachars := []string{"a|b", "*", "$HOME", "c;d", "$(whoami)", "x&y", ">out"}
	rh := h.start(dispatch.RunSpec{
		RunID: "r-metachars",
		Dir:   dir,
		Argv:  append([]string{script}, metachars...),
	})
	if _, err := rh.Wait(); err != nil {
		t.Fatalf("Wait: %v", err)
	}

	got := h.log.contents("r-metachars")
	for _, want := range metachars {
		if !strings.Contains(got, want+"\n") {
			t.Errorf("argument %q was not passed literally; output was:\n%s", want, got)
		}
	}
	// A glob must not have expanded (no shell): the literal "*" line is present and
	// no directory listing leaked in.
	if strings.Contains(got, "echo-args.sh\n") {
		t.Errorf("glob was expanded by a shell; output leaked a file listing:\n%s", got)
	}
}

// TestStartRunCwdEnvInjection proves a run's working directory is its pipeline
// folder and its environment is inherited + declared + the injected scoped DB
// connection: the script sees the pipeline folder as cwd, an inherited variable, its
// declared variable, and the injected IRIS_DB_URL.
func TestStartRunCwdEnvInjection(t *testing.T) {
	h := newHarness(t)
	dir := t.TempDir()
	script := writeScript(t, dir, "probe.sh",
		"#!/bin/sh\n/bin/pwd\nprintf 'DECLARED=%s\\n' \"$DECLARED_VAR\"\n"+
			"printf 'DBURL=%s\\n' \"$IRIS_DB_URL\"\nprintf 'INHERITED=%s\\n' \"$IRIS_TEST_INHERITED\"\n")

	t.Setenv("IRIS_TEST_INHERITED", "inherited-value")
	const dbURL = "postgres://scoped-conn/for-this-run"
	rh := h.start(dispatch.RunSpec{
		RunID: "r-env",
		Dir:   dir,
		Argv:  []string{script},
		Env:   []string{"DECLARED_VAR=declared-value"},
		DBURL: dbURL,
	})
	if _, err := rh.Wait(); err != nil {
		t.Fatalf("Wait: %v", err)
	}

	got := h.log.contents("r-env")

	// cwd is the pipeline folder (resolve symlinks: macOS temp dirs live behind
	// /private).
	wantDir, _ := filepath.EvalSymlinks(dir)
	line := strings.SplitN(got, "\n", 2)[0]
	gotDir, _ := filepath.EvalSymlinks(strings.TrimSpace(line))
	if gotDir != wantDir {
		t.Errorf("cwd = %q, want the pipeline folder %q", gotDir, wantDir)
	}
	for _, want := range []string{
		"DECLARED=declared-value",
		"DBURL=" + dbURL,
		"INHERITED=inherited-value",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("run env missing %q; output was:\n%s", want, got)
		}
	}
}

// TestStartRunHandleIsProcessGroup proves the engine records the run's process-group
// id as its handle (runs.handle) when the subprocess starts: the value written
// through the single writer equals the started group's id, and that id names a real,
// live process group.
func TestStartRunHandleIsProcessGroup(t *testing.T) {
	h := newHarness(t)
	dir := t.TempDir()
	script := writeScript(t, dir, "block.sh", blockingScript)

	rh := h.start(dispatch.RunSpec{RunID: "r-handle", Dir: dir, Argv: []string{script}})

	if rh.PGID() <= 0 {
		t.Fatalf("PGID() = %d, want a positive process-group id", rh.PGID())
	}
	waitFor(t, func() bool { return groupAlive(rh.PGID()) }, "started run's process group is alive")

	pgid, ok := markRunningPGID(h.rec.Statements(), "r-handle")
	if !ok {
		t.Fatal("no run-start write recorded a process-group handle")
	}
	if pgid != rh.PGID() {
		t.Errorf("recorded handle = %d, want the started process-group id %d", pgid, rh.PGID())
	}
}

// TestStartRunOutputCaptured proves the engine captures a run's output: both its
// stdout and its stderr land in the run's log sink.
func TestStartRunOutputCaptured(t *testing.T) {
	h := newHarness(t)
	dir := t.TempDir()
	script := writeScript(t, dir, "noisy.sh",
		"#!/bin/sh\nprintf 'to-stdout\\n'\nprintf 'to-stderr\\n' >&2\n")

	rh := h.start(dispatch.RunSpec{RunID: "r-out", Dir: dir, Argv: []string{script}})
	if _, err := rh.Wait(); err != nil {
		t.Fatalf("Wait: %v", err)
	}

	got := h.log.contents("r-out")
	for _, want := range []string{"to-stdout", "to-stderr"} {
		if !strings.Contains(got, want) {
			t.Errorf("captured output missing %q; got:\n%s", want, got)
		}
	}
}

// TestCancelKillsGroupDeadLetters proves iris run cancel kills the run's process
// group and dead-letters it as stopped while touching nothing else: a second,
// concurrent run's group and its meta record are untouched.
func TestCancelKillsGroupDeadLetters(t *testing.T) {
	h := newHarness(t)
	dir := t.TempDir()
	script := writeScript(t, dir, "block.sh", blockingScript)

	a := h.start(dispatch.RunSpec{RunID: "run-a", Dir: dir, Argv: []string{script}})
	b := h.start(dispatch.RunSpec{RunID: "run-b", Dir: dir, Argv: []string{script}})
	waitFor(t, func() bool { return groupAlive(a.PGID()) && groupAlive(b.PGID()) },
		"both runs' groups are alive before the cancel")

	if err := h.mgr.CancelRun(h.ctx, "run-a"); err != nil {
		t.Fatalf("CancelRun(run-a): %v", err)
	}

	// run-a's group dies; run-b's group is untouched.
	waitFor(t, func() bool { return !groupAlive(a.PGID()) }, "cancelled run's group is dead")
	if !groupAlive(b.PGID()) {
		t.Error("cancel touched the other run: run-b's group was killed too")
	}

	// run-a is dead-lettered as stopped; run-b has no dead-letter write at all.
	stmts := h.rec.Statements()
	if !deadLetteredStopped(stmts, "run-a") {
		t.Error("cancelled run-a was not dead-lettered as stopped")
	}
	if deadLetteredStopped(stmts, "run-b") {
		t.Error("cancel dead-lettered the untouched run-b")
	}
}

// TestNoEngineTimeout proves the engine never kills a run on elapsed time: a
// long-running run started with an ordinary (deadline-free) context stays alive on
// its own and ends only when an explicit cancel kills it. The API carries no timeout
// or deadline knob to trip -- StartRun and RunSpec expose none. A run reaches a
// terminal state only by its process exiting or by an explicit cancel; no clock ever
// ends one (clock doctrine: runs end only by exit or cancellation).
func TestNoEngineTimeout(t *testing.T) {
	h := newHarness(t)
	dir := t.TempDir()
	script := writeScript(t, dir, "block.sh", blockingScript)

	rh := h.start(dispatch.RunSpec{RunID: "r-long", Dir: dir, Argv: []string{script}})

	// The run is running: nothing has ended it, and repeated observation confirms
	// the engine never steps in with a clock.
	waitFor(t, func() bool { return strings.Contains(h.log.contents("r-long"), "started") },
		"long run is running")
	for i := 0; i < 5; i++ {
		if !groupAlive(rh.PGID()) {
			t.Fatalf("run was killed with no cancellation (an engine timeout); iteration %d", i)
		}
	}

	// Only the explicit cancel ends it.
	if err := h.mgr.CancelRun(h.ctx, "r-long"); err != nil {
		t.Fatalf("CancelRun(r-long): %v", err)
	}
	waitFor(t, func() bool { return !groupAlive(rh.PGID()) }, "explicit cancel ended the run")
}

// TestRunTransitionRejectsOutOfEnum proves the run state machine is a closed enum: a
// transition function accepts the legal lifecycle edges and rejects any value outside
// the four run states, so an out-of-enum state never reaches a meta write.
func TestRunTransitionRejectsOutOfEnum(t *testing.T) {
	legal := []struct{ from, to store.RunState }{
		{store.RunQueued, store.RunRunning},
		{store.RunRunning, store.RunSucceeded},
		{store.RunRunning, store.RunDeadLettered},
	}
	for _, tc := range legal {
		if err := dispatch.CheckRunTransition(tc.from, tc.to); err != nil {
			t.Errorf("CheckRunTransition(%q, %q) = %v, want nil (a legal edge)", tc.from, tc.to, err)
		}
	}

	outOfEnum := []struct{ from, to store.RunState }{
		{store.RunQueued, "cancelled"},  // "cancelled" is prose, not a run state
		{"bogus", store.RunRunning},     // unknown source state
		{store.RunRunning, "in_flight"}, // near-miss target
		{store.RunQueued, ""},           // empty is not a state
	}
	for _, tc := range outOfEnum {
		if err := dispatch.CheckRunTransition(tc.from, tc.to); err == nil {
			t.Errorf("CheckRunTransition(%q, %q) = nil, want an out-of-enum rejection", tc.from, tc.to)
		}
	}

	illegal := []struct{ from, to store.RunState }{
		{store.RunSucceeded, store.RunRunning},      // terminal has no successor
		{store.RunDeadLettered, store.RunSucceeded}, // terminal has no successor
		{store.RunQueued, store.RunSucceeded},       // must pass through running
		{store.RunRunning, store.RunQueued},         // never runs backward
	}
	for _, tc := range illegal {
		if err := dispatch.CheckRunTransition(tc.from, tc.to); err == nil {
			t.Errorf("CheckRunTransition(%q, %q) = nil, want an illegal-edge rejection", tc.from, tc.to)
		}
	}
}
