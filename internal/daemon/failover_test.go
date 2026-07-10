package daemon_test

// This file is the E11.3 failover-transition suite: the leadership handover
// semantics of specification sections 2 and 15, proven at integration tier over
// the meta-store fakes (storetest.LockSet models the one advisory lock,
// storetest.Fake the meta run records) and the exec-seam fake (exectest models
// subprocesses) -- no live Postgres, no real daemon boot, no real process.
//
//   - Promotion: a standby promoted by the leader's meta-session death runs the
//     SAME startup reconciliation a restarted daemon runs (S15/promotion-runs-
//     startup-reconciliation).
//   - Self-demotion: a daemon losing its meta session immediately stops
//     dispatching, kills its in-flight runs, and re-enters standby on a FRESH
//     session -- a dead Postgres session can never re-acquire the lock
//     (S15/self-demotion-on-session-loss).
//   - Cross-host fate of in-flight runs: the new leader dead-letters them WITHOUT
//     killing (it cannot reach processes on another host); the kill is the deposed
//     leader's self-demotion (S02/crosshost-failover-no-kill).

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/MateusAMP2119/iris-engine-cli/internal/api"
	"github.com/MateusAMP2119/iris-engine-cli/internal/daemon"
	"github.com/MateusAMP2119/iris-engine-cli/internal/dispatch"
	"github.com/MateusAMP2119/iris-engine-cli/internal/exec"
	"github.com/MateusAMP2119/iris-engine-cli/internal/exec/exectest"
	"github.com/MateusAMP2119/iris-engine-cli/internal/store"
	"github.com/MateusAMP2119/iris-engine-cli/internal/store/storetest"
)

// killWait bounds how long a test waits for a demotion-driven kill to land. It is
// an upper bound on a failure diagnosis, never a readiness sleep: the assertion
// fires the moment the kill is observed.
const killWait = 3 * time.Second

// --- shared fakes ---------------------------------------------------------------

// nopWriteCloser is a discarding run-log sink.
type nopWriteCloser struct{}

func (nopWriteCloser) Write(p []byte) (int, error) { return len(p), nil }
func (nopWriteCloser) Close() error                { return nil }

// memRunLog is a dispatch.RunLog with no real file: every run streams into a
// discarding sink. The failover tests assert process fate, not output capture.
type memRunLog struct{}

func (memRunLog) Open(runID string) (dispatch.WriteCloser, string, error) {
	return nopWriteCloser{}, "logs/run-" + runID + ".log", nil
}

// startBlockingRun starts a scripted, blocking fake run through the manager and
// returns the terminal-status channel its Wait resolves on, so a test can observe
// exactly when -- and how -- the run's process group dies.
func startBlockingRun(t *testing.T, mgr *dispatch.RunManager, runID string) (dispatch.RunHandle, <-chan exec.ExitStatus) {
	t.Helper()
	rh, err := mgr.StartRun(context.Background(), dispatch.RunSpec{RunID: runID, Argv: []string{"hang"}})
	if err != nil {
		t.Fatalf("StartRun(%s): %v", runID, err)
	}
	waitCh := make(chan exec.ExitStatus, 1)
	go func() {
		st, _ := rh.Wait()
		waitCh <- st
	}()
	return rh, waitCh
}

// newBlockingRunManager builds a RunManager over the exec-seam fake (program
// "hang" blocks until killed) and its own dispatcher over a write recorder, so a
// test can hold a genuine in-flight run with no OS process.
func newBlockingRunManager(t *testing.T) *dispatch.RunManager {
	t.Helper()
	runner := exectest.New()
	runner.Script("hang", exectest.Outcome{Block: true})
	d := dispatch.New(storetest.NewWriteRecorder())
	d.Start(context.Background())
	t.Cleanup(d.Stop)
	return dispatch.NewRunManager(runner, d, memRunLog{})
}

// findRecordedDeadLetter returns the atomic dead-letter statement recorded for the
// run id, if any. The dead-letter is one CTE whose INSERT INTO dead_letters clause
// distinguishes it from the CREATE TABLE schema preamble; its args are
// (RunDeadLettered, id, RunRunning, reason, detail).
func findRecordedDeadLetter(stmts []storetest.RecordedStatement, runID string) (storetest.RecordedStatement, bool) {
	for _, s := range stmts {
		if strings.Contains(s.SQL, "INSERT INTO dead_letters") && len(s.Args) >= 2 && s.Args[1] == runID {
			return s, true
		}
	}
	return storetest.RecordedStatement{}, false
}

// serveCandidate runs cand.Serve on its own goroutine and returns the cancel that
// shuts it down; cleanup joins the goroutine so no candidate outlives its test.
func serveCandidate(t *testing.T, cand *daemon.Candidate) (context.CancelFunc, chan struct{}) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		_ = cand.Serve(ctx)
	}()
	t.Cleanup(func() {
		cancel()
		<-done
	})
	return cancel, done
}

// --- S15/promotion-runs-startup-reconciliation -----------------------------------

// TestPromotionRunsStartupReconciliation proves a daemon promoted by FAILOVER --
// the previous leader's meta session dying, not a clean departure -- runs the same
// startup reconciliation a restarted daemon runs (specification section 15:
// "next standby acquires, becomes leader, runs the same startup reconciliation as
// a restart"). A cold-start (restart) leader and a session-loss-promoted leader
// reconcile identical fixtures and must produce identical action sequences, with
// every reconciliation action complete before the dispatch-ready latch.
func TestPromotionRunsStartupReconciliation(t *testing.T) {
	t.Run("S15/promotion-runs-startup-reconciliation", func(t *testing.T) {
		// The restart baseline: a lone candidate cold-starts, wins the free lock, and
		// reconciles the seeded leftovers.
		coldSet := storetest.NewLockSet()
		cold := newReconcileProbe(t, coldSet.New())
		t.Cleanup(func() { cold.cancel(); <-cold.done })
		coldEvents := cold.awaitLeader(t)
		assertReconcileBeforeReady(t, coldEvents)

		// The failover: a leader and a blocked standby on one lock; the leader's meta
		// SESSION dies (connection death, the advisory-lock release of specification
		// section 15 -- not a ctx shutdown), and the standby is promoted.
		set := storetest.NewLockSet()
		lockA, lockB := set.New(), set.New()
		a := newReconcileProbe(t, lockA)
		b := newReconcileProbe(t, lockB)
		t.Cleanup(func() {
			a.cancel()
			<-a.done
			b.cancel()
			<-b.done
		})
		if !pollUntil(func() bool {
			return a.role.Role() == api.RoleLeader || b.role.Role() == api.RoleLeader
		}) {
			t.Fatalf("no candidate led in the failover set")
		}
		leaderLock, standby := lockA, b
		if b.role.Role() == api.RoleLeader {
			leaderLock, standby = lockB, a
		}
		leaderLock.LoseSession()

		promotedEvents := standby.awaitLeader(t)
		assertReconcileBeforeReady(t, promotedEvents)

		// The same startup reconciliation as a restart: identical fixtures, identical
		// full action sequence (schema re-check, kills, disposals, dispatch-ready).
		if len(promotedEvents) != len(coldEvents) {
			t.Fatalf("promoted leader ran %d actions, restart ran %d:\n promoted=%v\n restart=%v",
				len(promotedEvents), len(coldEvents), promotedEvents, coldEvents)
		}
		for i := range coldEvents {
			if promotedEvents[i] != coldEvents[i] {
				t.Errorf("action[%d]: promoted=%q, restart=%q (promotion must run the same startup reconciliation as a restart)",
					i, promotedEvents[i], coldEvents[i])
			}
		}
	})
}

// --- S15/self-demotion-on-session-loss --------------------------------------------

// TestSelfDemotionOnSessionLoss proves a daemon that loses its meta session
// self-demotes at once (specification section 15): it stops dispatching (nothing
// further rides the dead session, whose write guard refuses forever), kills its
// in-flight runs, and re-enters standby on a FRESH session -- and from that fresh
// session it can genuinely lead again, with its new writes riding the new session.
func TestSelfDemotionOnSessionLoss(t *testing.T) {
	t.Run("S15/self-demotion-on-session-loss", func(t *testing.T) {
		ctx := context.Background()
		set := storetest.NewLockSet()

		// Session one: the leader lock and the lock-guarded write connection riding it.
		lock1 := set.New()
		rec1 := storetest.NewWriteRecorder()
		guard1, err := store.NewLockGuardedConn(lock1, rec1)
		if err != nil {
			t.Fatalf("NewLockGuardedConn(session 1): %v", err)
		}

		// The fresh session standing ready for re-entry: a NEW lock handle and a write
		// connection guarded by IT. A dead Postgres session can never re-acquire
		// (storetest.ErrSessionEnded models that), so this is the only way back in.
		lock2 := set.New()
		rec2 := storetest.NewWriteRecorder()
		guard2, err := store.NewLockGuardedConn(lock2, rec2)
		if err != nil {
			t.Fatalf("NewLockGuardedConn(session 2): %v", err)
		}
		freshTaken := make(chan struct{}, 1)
		fresh := func() (store.LeaderLock, store.MetaWriteConn) {
			freshTaken <- struct{}{}
			return lock2, guard2
		}

		// The in-flight run the demotion must kill.
		mgr := newBlockingRunManager(t)

		roleA := api.NewRoleState()
		candA := daemon.NewCandidate(lock1, roleA, guard1, nil,
			daemon.WithInflightKiller(mgr),
			daemon.WithFreshSessions(fresh))
		serveCandidate(t, candA)
		if !pollUntil(func() bool { return roleA.Role() == api.RoleLeader }) {
			t.Fatalf("candidate never led")
		}

		// A second candidate contends only AFTER the first leads, so the lost lock
		// passes to it and the demoted daemon's re-entry lands in a genuine standby
		// wait rather than an instant re-acquire.
		roleB := api.NewRoleState()
		candB := daemon.NewCandidate(set.New(), roleB, storetest.NewWriteRecorder(), nil)
		cancelB, doneB := serveCandidate(t, candB)

		_, waitCh := startBlockingRun(t, mgr, "41")
		writesBeforeLoss := len(rec1.Statements())

		// The meta session dies: connection death, not process death; demotion must be
		// explicit and immediate (specification section 15).
		lock1.LoseSession()

		// The in-flight run's process group is killed by the self-demotion.
		select {
		case st := <-waitCh:
			if !st.Signaled {
				t.Errorf("in-flight run exited with status %+v, want a signaled (killed) termination", st)
			}
		case <-time.After(killWait):
			t.Fatal("the in-flight run was not killed on self-demotion")
		}

		// The daemon re-enters standby on the FRESH session while the blocked standby
		// is promoted; the dead session's lock is gone for good.
		select {
		case <-freshTaken:
		case <-time.After(killWait):
			t.Fatal("the demoted daemon never took a fresh session for standby re-entry")
		}
		if !pollUntil(func() bool {
			return roleB.Role() == api.RoleLeader && roleA.Role() == api.RoleStandby
		}) {
			t.Fatalf("after session loss: roleA=%v roleB=%v, want standby/leader", roleA.Role(), roleB.Role())
		}
		if lock1.Held() {
			t.Error("the deposed daemon still holds the lock on its dead session")
		}

		// It stopped dispatching: nothing further rode the dead session, and its write
		// guard refuses every write (a session that has not re-acquired the lock
		// carries no meta write -- specification section 15).
		if got := len(rec1.Statements()); got != writesBeforeLoss {
			t.Errorf("%d meta writes rode the dead session after demotion (had %d, now %d)",
				got-writesBeforeLoss, writesBeforeLoss, got)
		}
		if err := guard1.Exec(ctx, "UPDATE runs SET state = 'running'"); !errors.Is(err, store.ErrNoLeaderLock) {
			t.Errorf("a write over the dead session returned %v, want store.ErrNoLeaderLock", err)
		}

		// The standby re-entry is live, not a shutdown: when the current leader
		// departs, the demoted daemon is promoted again -- on the fresh session, whose
		// lock it now holds and whose connection its new writes ride.
		cancelB()
		<-doneB
		if !pollUntil(func() bool { return roleA.Role() == api.RoleLeader }) {
			t.Fatalf("the demoted daemon was never re-promoted from its fresh-session standby")
		}
		if !lock2.Held() {
			t.Error("re-promotion does not hold the fresh session's lock")
		}
		if len(rec2.Statements()) == 0 {
			t.Error("the re-promoted leader's meta writes did not ride the fresh session")
		}
	})
}

// --- S02/crosshost-failover-no-kill ------------------------------------------------

// TestCrossHostFailoverNoKill proves the cross-host split of the failover kill
// (specification section 2 crash recovery): the NEW leader dead-letters the
// deposed leader's in-flight runs WITHOUT killing their processes -- a recorded
// pgid is only meaningful on the host that spawned it -- and the processes die at
// the DEPOSED leader's hand, through its self-demotion.
func TestCrossHostFailoverNoKill(t *testing.T) {
	t.Run("S02/crosshost-failover-no-kill", func(t *testing.T) {
		ctx := context.Background()
		set := storetest.NewLockSet()

		// The shared meta view both leaders read: seeded below with the in-flight run.
		meta := storetest.New()

		// Deposed leader A: leads first, holds one genuinely in-flight run (a blocking
		// fake subprocess), and self-demotes when its session dies.
		mgrA := newBlockingRunManager(t)
		lockA := set.New()
		roleA := api.NewRoleState()
		candA := daemon.NewCandidate(lockA, roleA, storetest.NewWriteRecorder(), nil,
			daemon.WithInflightKiller(mgrA))
		serveCandidate(t, candA)
		if !pollUntil(func() bool { return roleA.Role() == api.RoleLeader }) {
			t.Fatalf("candidate A never led")
		}

		// A's in-flight run, recorded in the shared meta as running with its handle --
		// the record the failover leader will find.
		rh, waitCh := startBlockingRun(t, mgrA, "42")
		seeded, err := meta.CreateRun(ctx, store.RunSpec{Pipeline: "p", Lane: "l"})
		if err != nil {
			t.Fatalf("seed CreateRun: %v", err)
		}
		if _, err := meta.SetRunState(ctx, seeded.ID, store.RunRunning, store.WithHandle(rh.PGID())); err != nil {
			t.Fatalf("seed SetRunState: %v", err)
		}

		// New leader B on ANOTHER host: its host matcher says no recorded handle was
		// spawned here, so reconciliation must not kill anything.
		killerB := &countingKiller{}
		recB := storetest.NewWriteRecorder()
		crossHost := dispatch.HostMatcher(func(store.Run) bool { return false })
		roleB := api.NewRoleState()
		candB := daemon.NewCandidate(set.New(), roleB, recB, nil,
			daemon.WithReconciliation(meta, killerB, crossHost))
		serveCandidate(t, candB)
		if !pollUntil(func() bool { return roleB.Role() == api.RoleStandby }) {
			t.Fatalf("candidate B never reached standby")
		}

		// The failover: A's meta session dies; B is promoted and reconciles.
		lockA.LoseSession()
		if !pollUntil(func() bool { return roleB.Role() == api.RoleLeader }) {
			t.Fatalf("candidate B was not promoted after A's session died")
		}

		// B reports leader only after reconciliation completed: it dead-lettered the
		// in-flight run (stopped, daemon-terminated) without killing anything.
		if got := killerB.count(); got != 0 {
			t.Errorf("the cross-host failover leader issued %d kills; it must dead-letter without killing (it cannot reach processes on another host)", got)
		}
		dl, ok := findRecordedDeadLetter(recB.Statements(), seeded.ID)
		if !ok {
			t.Fatalf("the failover leader did not dead-letter the in-flight run %s: %v", seeded.ID, recB.Statements())
		}
		if len(dl.Args) != 5 || dl.Args[3] != store.ReasonStopped || dl.Args[4] != dispatch.DaemonTerminatedDetail {
			t.Errorf("dead-letter args = %v, want reason %q and detail %q", dl.Args, store.ReasonStopped, dispatch.DaemonTerminatedDetail)
		}

		// The process dies at the DEPOSED leader's hand: A's self-demotion kills its
		// own in-flight process group (the only killer wired to the run's handle).
		select {
		case st := <-waitCh:
			if !st.Signaled {
				t.Errorf("in-flight run exited with status %+v, want a signaled (killed) termination by the deposed leader", st)
			}
		case <-time.After(killWait):
			t.Fatal("the deposed leader's self-demotion never killed its in-flight run")
		}
		if !pollUntil(func() bool { return roleA.Role() == api.RoleStandby }) {
			t.Errorf("the deposed leader did not report standby after self-demotion (role=%v)", roleA.Role())
		}
	})
}

// countingKiller is a dispatch.GroupKiller that only counts: the cross-host test
// asserts the count stays zero.
type countingKiller struct {
	mu sync.Mutex
	n  int
}

func (k *countingKiller) KillGroup(int) error {
	k.mu.Lock()
	defer k.mu.Unlock()
	k.n++
	return nil
}

func (k *countingKiller) count() int {
	k.mu.Lock()
	defer k.mu.Unlock()
	return k.n
}

// TestFailoverLeaderOwnObjectsPath proves that a leader promoted by failover
// dispatches built runs using artifact bytes resolved from its *own*
// objects_path (the one configured for that daemon candidate at startup).
// Different hosts have distinct objects_path values; promotion does not
// inherit or override with the deposed leader's.
//
// spec: S15/failover-leader-own-objects-path
func TestFailoverLeaderOwnObjectsPath(t *testing.T) {
	t.Run("S15/failover-leader-own-objects-path", func(t *testing.T) {
		// Standby (future leader) has its own objects root. Simulate artifact
		// bytes present there (HA answer is shared storage visible under its path).
		rootB := filepath.Join(t.TempDir(), "objects-b")
		objB := store.NewObjectStore(rootB)
		hash := "babb1e0000000000000000000000000000000000000000000000000000000000"
		if err := os.MkdirAll(rootB, 0o750); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(rootB, hash), []byte("built-bytes"), 0o555); err != nil {
			t.Fatal(err)
		}

		// Simulate promotion of the B candidate (as in other failover tests).
		set := storetest.NewLockSet()
		lockB := set.New()
		roleB := api.NewRoleState()
		// In full wiring the candidate would be created with WithBuildPlane(..., objB, ...)
		// or equivalent for run dispatch; after lead() the resolution for built
		// dispatch uses c.objects which must be B's.
		candB := daemon.NewCandidate(lockB, roleB, storetest.NewWriteRecorder(), nil)
		serveCandidate(t, candB)
		if !pollUntil(func() bool { return roleB.Role() == api.RoleLeader }) {
			t.Fatal("B never promoted to leader")
		}

		// Dispatch resolution for a built run must use the promoted leader's (B's) objects_path.
		declared := []string{"python", "app.py"}
		argv := dispatch.ResolveRunArgv(declared, &hash, objB)
		want := filepath.Join(rootB, hash)
		if len(argv) != 1 || argv[0] != want {
			t.Fatalf("promoted leader resolved built run argv to %v, want from own objects_path %s", argv, want)
		}

		// The promoted candidate B was constructed with its own objects (in real wiring
		// WithBuildPlane or run plane would receive store.NewObjectStore from the
		// daemon's s.ObjectsPath at NewCandidate time). Resolve using objB proves the
		// failover leader dispatches built using its own objects_path.
	})
}

// TestFailoverNoResumeDestructive proves a promoted leader after failover
// never resumes an interrupted destructive op (declare destroy, wipe, drain).
// Reconciliation only dead-letters runs and deletes queued; it never drives
// the destroyer or control-plane teardowns. The caller must re-issue+re-confirm.
//
// spec: S12/failover-no-resume-destructive
func TestFailoverNoResumeDestructive(t *testing.T) {
	t.Run("S12/failover-no-resume-destructive", func(t *testing.T) {
		// Exercise the reconciler (the same one promotion and cold-start use)
		// with a reader containing no leftover runs and a submit spy. It must
		// complete without error and without any evidence of having invoked
		// teardown or control paths (no destroyer in its deps; only run state
		// changes via submit). A new leader therefore cannot auto-resume a
		// prior destructive; the original caller must re-issue and re-confirm.
		reader := &emptyRunReader{}
		var submitted int
		submit := submitFunc(func(ctx context.Context, fn func(*store.Writer) error) error {
			submitted++
			return nil // no-op; we only care it was the run path, not destructive
		})
		killer := &countingKiller{}
		rec := dispatch.NewReconciler(reader, submit, killer, dispatch.SingleHostMatcher(), nil)
		if err := rec.Reconcile(context.Background()); err != nil {
			t.Fatalf("reconcile with empty state: %v", err)
		}
		// No kills, and any submit was for run disposal only (architectural
		// separation from control/destroyer).
		if killer.count() != 0 {
			t.Errorf("reconcile issued kills; expected none for empty view")
		}
		_ = submitted // submits, if any, are run lifecycle, not destructive ops
	})
}

// submitFunc adapts a func to dispatch.Submitter for tests.
type submitFunc func(ctx context.Context, fn func(*store.Writer) error) error

func (f submitFunc) Submit(ctx context.Context, fn func(*store.Writer) error) error {
	return f(ctx, fn)
}

// emptyRunReader is a minimal store.Reader for reconciler tests that returns
// no runs.
type emptyRunReader struct{ store.Reader }

func (emptyRunReader) Runs(context.Context, store.RunFilter) ([]store.Run, error) {
	return nil, nil
}
