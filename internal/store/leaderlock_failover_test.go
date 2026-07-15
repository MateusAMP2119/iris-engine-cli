package store_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/MateusAMP2119/iris-lakehouse/internal/store"
	"github.com/MateusAMP2119/iris-lakehouse/internal/store/storetest"
)

// This file proves the failover core of leader election at integration tier,
// against the meta-store fake and with no live Postgres. Leadership is a Postgres
// session advisory lock: the leader holds it, standby candidates block acquiring
// it, and the holder's session ending -- an explicit release or connection death
// -- promotes the next standby. The other half of the invariant is the write
// guard: meta writes are never issued over a session that has not (re-)acquired
// the leader lock, so across a failover there is no second writer and no
// overlapping run. The one real conformance leg (two daemon candidates, leader
// killed) rides E13; everything here drives the fake (storetest.LockSet /
// storetest.FakeLock): standby blocks, release promotes.

// promotionTimeout bounds every positive wait (a promotion that must happen);
// assertions wait on lock state, never a fixed sleep standing in for readiness.
const promotionTimeout = 3 * time.Second

// blockedProbe is the short observation window for the negative assertion that a
// standby stays blocked while the lock is held: the standby must NOT acquire within
// it. It is deliberately small -- a bounded probe of "still blocked", not a
// readiness sleep.
const blockedProbe = 50 * time.Millisecond

// TestAdvisoryLockLeaderElection proves the election mechanism: leadership is a
// session advisory lock, a standby blocked acquiring the meta leader lock stays
// blocked while the holder's session is alive, and it acquires the lock --
// becomes leader -- when the previous holder's SESSION ENDS (connection death,
// the failover trigger; not an explicit release, which TestFailoverLockFake
// covers).
func TestAdvisoryLockLeaderElection(t *testing.T) {
	ctx := context.Background()
	set := storetest.NewLockSet()

	// The first candidate acquires the lock and leads.
	leader := set.New()
	if err := leader.Acquire(ctx); err != nil {
		t.Fatalf("first candidate Acquire: %v", err)
	}
	if !leader.Held() {
		t.Fatal("first candidate does not report the lock held after acquiring")
	}

	// A standby contends for the same lock and blocks.
	standby := set.New()
	acquired := make(chan error, 1)
	go func() { acquired <- standby.Acquire(ctx) }()

	// While the holder's session is alive, the standby stays blocked: it never
	// acquires, and never reports the lock held.
	select {
	case err := <-acquired:
		t.Fatalf("standby acquired the leader lock while the holder's session was alive (err=%v)", err)
	case <-time.After(blockedProbe):
	}
	if standby.Held() {
		t.Fatal("blocked standby reports the lock held")
	}

	// The previous holder's session ends -- connection death releases the lock --
	// and the blocked standby acquires it, becoming leader.
	leader.LoseSession()

	select {
	case err := <-acquired:
		if err != nil {
			t.Fatalf("standby Acquire after the holder's session ended: %v", err)
		}
	case <-time.After(promotionTimeout):
		t.Fatal("standby was not promoted after the previous holder's session ended")
	}
	if !standby.Held() {
		t.Error("promoted standby does not report the lock held")
	}
	if leader.Held() {
		t.Error("the deposed holder still reports the lock held after its session ended")
	}
}

// TestFailoverLockFake proves the fake-tier failover doctrine: against the
// meta-store fake, a standby candidate blocks while the leader lock is held and
// is promoted when the lock is released.
func TestFailoverLockFake(t *testing.T) {
	ctx := context.Background()
	set := storetest.NewLockSet()

	leader := set.New()
	if err := leader.Acquire(ctx); err != nil {
		t.Fatalf("leader Acquire: %v", err)
	}

	standby := set.New()
	acquired := make(chan error, 1)
	go func() { acquired <- standby.Acquire(ctx) }()

	// Blocked while held: within the probe window the standby must not acquire, and
	// mutual exclusion holds -- the leader alone reports the lock held.
	select {
	case err := <-acquired:
		t.Fatalf("standby acquired the leader lock while it was held (err=%v)", err)
	case <-time.After(blockedProbe):
	}
	if standby.Held() {
		t.Fatal("blocked standby reports the lock held")
	}
	if !leader.Held() {
		t.Fatal("leader lost the lock without releasing it")
	}

	// Release promotes: the blocked standby acquires the lock.
	if err := leader.Release(ctx); err != nil {
		t.Fatalf("leader Release: %v", err)
	}
	select {
	case err := <-acquired:
		if err != nil {
			t.Fatalf("standby Acquire after release: %v", err)
		}
	case <-time.After(promotionTimeout):
		t.Fatal("standby was not promoted after the leader released the lock")
	}
	if !standby.Held() {
		t.Error("promoted standby does not report the lock held")
	}
	if leader.Held() {
		t.Error("the former leader still reports the lock held after releasing")
	}
}

// TestNoMetaWriteWithoutLock proves the single-writer invariant across failover:
// meta writes are never issued over a session that has not re-acquired the leader
// lock. The lock-guarded write connection refuses a write from a session that
// never acquired the lock, refuses every write from a deposed leader whose
// session ended, and a dead session can never re-acquire -- leadership (and with
// it the write path) requires a fresh session. So across a failover the meta
// write record shows exactly one writer at a time and no overlapping run.
func TestNoMetaWriteWithoutLock(t *testing.T) {
	ctx := context.Background()

	t.Run("a session that never acquired the lock cannot write meta", func(t *testing.T) {
		set := storetest.NewLockSet()
		meta := storetest.NewWriteRecorder()
		conn, err := store.NewLockGuardedConn(set.New(), meta)
		if err != nil {
			t.Fatalf("NewLockGuardedConn: %v", err)
		}

		if err := conn.Exec(ctx, "INSERT INTO runs (pipeline) VALUES ($1)", "p"); !errors.Is(err, store.ErrNoLeaderLock) {
			t.Errorf("Exec before acquiring the lock: err = %v, want ErrNoLeaderLock", err)
		}
		if err := conn.ExecTx(ctx, []store.Statement{{SQL: "DELETE FROM lanes WHERE lane = $1", Args: []any{"l"}}}); !errors.Is(err, store.ErrNoLeaderLock) {
			t.Errorf("ExecTx before acquiring the lock: err = %v, want ErrNoLeaderLock", err)
		}
		if got := meta.Statements(); len(got) != 0 {
			t.Errorf("a lockless session issued %d meta statements, want 0: %+v", len(got), got)
		}
	})

	t.Run("the lock-holding session writes meta", func(t *testing.T) {
		set := storetest.NewLockSet()
		meta := storetest.NewWriteRecorder()
		lock := set.New()
		conn, err := store.NewLockGuardedConn(lock, meta)
		if err != nil {
			t.Fatalf("NewLockGuardedConn: %v", err)
		}
		if err := lock.Acquire(ctx); err != nil {
			t.Fatalf("Acquire: %v", err)
		}

		if err := conn.Exec(ctx, "UPDATE runs SET state = $1 WHERE id = $2", "running", int64(1)); err != nil {
			t.Fatalf("Exec on the lock-holding session: %v", err)
		}
		if err := conn.ExecTx(ctx, []store.Statement{{SQL: "INSERT INTO lanes (lane, pipeline, pos) VALUES ($1, $2, $3)", Args: []any{"l", "p", int64(0)}}}); err != nil {
			t.Fatalf("ExecTx on the lock-holding session: %v", err)
		}
		if got := meta.Statements(); len(got) != 2 {
			t.Errorf("the lock-holding session recorded %d statements, want 2: %+v", len(got), got)
		}
		if got := meta.Transactions(); len(got) != 1 {
			t.Errorf("the lock-holding session recorded %d transactions, want 1", len(got))
		}
	})

	t.Run("no second writer across a failover", func(t *testing.T) {
		set := storetest.NewLockSet()
		// One shared recorder stands in for meta itself: every statement any session
		// manages to issue lands here, so the final record IS the writer history.
		meta := storetest.NewWriteRecorder()

		// Leader A acquires the lock and writes.
		lockA := set.New()
		connA, err := store.NewLockGuardedConn(lockA, meta)
		if err != nil {
			t.Fatalf("NewLockGuardedConn(A): %v", err)
		}
		if err := lockA.Acquire(ctx); err != nil {
			t.Fatalf("A Acquire: %v", err)
		}
		if err := connA.Exec(ctx, "UPDATE runs SET state = $1 WHERE id = $2", "running", int64(1)); err != nil {
			t.Fatalf("A Exec while leading: %v", err)
		}

		// A's meta session dies (the failover trigger); standby B is promoted on a
		// FRESH session and takes over the write path.
		lockA.LoseSession()
		lockB := set.New()
		if err := lockB.Acquire(ctx); err != nil {
			t.Fatalf("B Acquire after A's session ended: %v", err)
		}
		connB, err := store.NewLockGuardedConn(lockB, meta)
		if err != nil {
			t.Fatalf("NewLockGuardedConn(B): %v", err)
		}
		if err := connB.Exec(ctx, "UPDATE runs SET state = $1 WHERE id = $2", "running", int64(2)); err != nil {
			t.Fatalf("B Exec while leading: %v", err)
		}

		// The deposed leader's session has NOT re-acquired the lock: every write over
		// it is refused -- single Execs and atomic batches alike -- so no second
		// writer and no overlapping run can exist.
		if err := connA.Exec(ctx, "UPDATE runs SET state = $1 WHERE id = $2", "running", int64(3)); !errors.Is(err, store.ErrNoLeaderLock) {
			t.Errorf("deposed leader Exec: err = %v, want ErrNoLeaderLock", err)
		}
		if err := connA.ExecTx(ctx, []store.Statement{{SQL: "DELETE FROM runs WHERE id = $1", Args: []any{int64(3)}}}); !errors.Is(err, store.ErrNoLeaderLock) {
			t.Errorf("deposed leader ExecTx: err = %v, want ErrNoLeaderLock", err)
		}

		// And the dead session can never re-acquire: re-acquisition demands a fresh
		// session (a new handle), since a deposed leader re-enters standby only on a
		// fresh session. The refused Acquire keeps the deposed session permanently
		// writeless.
		if err := lockA.Acquire(ctx); !errors.Is(err, storetest.ErrSessionEnded) {
			t.Errorf("deposed leader re-Acquire on its dead session: err = %v, want ErrSessionEnded", err)
		}
		if err := connA.Exec(ctx, "UPDATE runs SET state = $1 WHERE id = $2", "running", int64(3)); !errors.Is(err, store.ErrNoLeaderLock) {
			t.Errorf("deposed leader Exec after re-acquire attempt: err = %v, want ErrNoLeaderLock", err)
		}

		// The meta record shows exactly the two lock-held writes, in leadership
		// order: A's while it led, then B's -- never a deposed write in between or
		// after.
		got := meta.Statements()
		if len(got) != 2 {
			t.Fatalf("meta recorded %d statements, want 2 (one per lock-holding leader): %+v", len(got), got)
		}
		if got[0].Args[1] != int64(1) || got[1].Args[1] != int64(2) {
			t.Errorf("meta write order = %+v, want A's run-1 write then B's run-2 write", got)
		}
	})

	t.Run("the single Writer composes with the guard", func(t *testing.T) {
		set := storetest.NewLockSet()
		meta := storetest.NewWriteRecorder()
		lock := set.New()
		conn, err := store.NewLockGuardedConn(lock, meta)
		if err != nil {
			t.Fatalf("NewLockGuardedConn: %v", err)
		}
		if err := lock.Acquire(ctx); err != nil {
			t.Fatalf("Acquire: %v", err)
		}
		w := store.NewWriter(conn)
		if err := w.MarkRunRunning(ctx, "1", 4242, ""); err != nil {
			t.Fatalf("MarkRunRunning while leading: %v", err)
		}

		// The leader's session ends; every write path through the one Writer is now
		// refused before it reaches meta -- the Exec path and the atomic-transaction
		// path both.
		lock.LoseSession()
		if err := w.MarkRunRunning(ctx, "2", 4243, ""); !errors.Is(err, store.ErrNoLeaderLock) {
			t.Errorf("MarkRunRunning after session loss: err = %v, want ErrNoLeaderLock", err)
		}
		if err := w.RewriteLane(ctx, "lane", []string{"a", "b"}); !errors.Is(err, store.ErrNoLeaderLock) {
			t.Errorf("RewriteLane after session loss: err = %v, want ErrNoLeaderLock", err)
		}
		if got := meta.Statements(); len(got) != 1 {
			t.Errorf("meta recorded %d statements, want only the pre-demotion write: %+v", len(got), got)
		}
	})

	t.Run("the guard is constructed only over a lock and a connection", func(t *testing.T) {
		set := storetest.NewLockSet()
		if _, err := store.NewLockGuardedConn(nil, storetest.NewWriteRecorder()); err == nil {
			t.Error("NewLockGuardedConn(nil lock) succeeded, want an error")
		}
		if _, err := store.NewLockGuardedConn(set.New(), nil); err == nil {
			t.Error("NewLockGuardedConn(nil conn) succeeded, want an error")
		}
	})
}
