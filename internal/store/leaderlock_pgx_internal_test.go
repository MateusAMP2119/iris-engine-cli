package store

import (
	"context"
	"errors"
	"testing"
)

// scriptedSession is a fake pinnedConn that records the exact statements the
// leader lock issues (SQL plus args) and every close, so a test can prove the
// advisory lock is acquired on one session-pinned connection and that the session
// is held (never returned/closed) for the whole leadership lifetime -- the
// distinction between a session-pinned connection and a pooled one whose return
// releases the lock (specification section 9).
type scriptedSession struct {
	execs      []scriptedExec
	closes     int
	execErr    error
	unlockErr  error // returned only for the release (pg_advisory_unlock) statement
	closeErr   error
	blockAcq   chan struct{} // when non-nil, exec blocks on it before returning (models a held lock)
	afterClose bool          // set true once close has been called, to detect use-after-return
	usedAfter  bool          // set if exec ran after a close (a pooled-conn misuse)
}

type scriptedExec struct {
	sql  string
	args []any
}

func (s *scriptedSession) exec(ctx context.Context, sql string, args ...any) error {
	if s.afterClose {
		s.usedAfter = true
	}
	if s.blockAcq != nil {
		select {
		case <-s.blockAcq:
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	s.execs = append(s.execs, scriptedExec{sql: sql, args: args})
	if sql == ReleaseLeaderLockSQL && s.unlockErr != nil {
		return s.unlockErr
	}
	return s.execErr
}

func (s *scriptedSession) close(context.Context) error {
	s.closes++
	s.afterClose = true
	return s.closeErr
}

// TestPgxLeaderLockSessionPinned proves the leader-election advisory lock is
// acquired and held on a session-pinned connection: the lock issues
// pg_advisory_lock(LeaderLockKey) on one dedicated connection, keeps that same
// connection open for its whole held lifetime (never returning it, as a pooled
// connection would -- which would release the lock), and only closes it on
// Release, alongside the matching pg_advisory_unlock.
//
// spec: S09/leader-lock-session-pinned-conn
func TestPgxLeaderLockSessionPinned(t *testing.T) {
	t.Run("S09/leader-lock-session-pinned-conn", func(t *testing.T) {
		t.Run("acquire issues pg_advisory_lock on the pinned session", func(t *testing.T) {
			sess := &scriptedSession{}
			lock, err := newPgxLeaderLock(sess)
			if err != nil {
				t.Fatalf("newPgxLeaderLock: %v", err)
			}
			if err := lock.Acquire(context.Background()); err != nil {
				t.Fatalf("Acquire: %v", err)
			}
			if len(sess.execs) != 1 {
				t.Fatalf("acquire issued %d statements, want 1: %+v", len(sess.execs), sess.execs)
			}
			if sess.execs[0].sql != AcquireLeaderLockSQL {
				t.Errorf("acquire SQL = %q, want %q", sess.execs[0].sql, AcquireLeaderLockSQL)
			}
			if len(sess.execs[0].args) != 1 || sess.execs[0].args[0] != LeaderLockKey {
				t.Errorf("acquire args = %v, want [%d]", sess.execs[0].args, LeaderLockKey)
			}
			// Held, not returned: the session stays open after acquiring (a pooled
			// connection would be returned to the pool here, dropping the lock).
			if sess.closes != 0 {
				t.Errorf("session was closed %d times while the lock was held; a session-pinned lock never returns its connection", sess.closes)
			}
		})

		t.Run("the connection is pinned for the whole held lifetime, released only on Release", func(t *testing.T) {
			sess := &scriptedSession{}
			lock, err := newPgxLeaderLock(sess)
			if err != nil {
				t.Fatalf("newPgxLeaderLock: %v", err)
			}
			ctx := context.Background()
			if err := lock.Acquire(ctx); err != nil {
				t.Fatalf("Acquire: %v", err)
			}
			// While held, the session is never closed/returned: the acquire and any
			// subsequent meta writes ride this one pinned session.
			if sess.closes != 0 {
				t.Fatalf("session closed while lock held (closes=%d)", sess.closes)
			}
			if err := lock.Release(ctx); err != nil {
				t.Fatalf("Release: %v", err)
			}
			// Release unlocks then closes the same session.
			if len(sess.execs) != 2 {
				t.Fatalf("after release, %d statements issued, want 2 (lock, unlock): %+v", len(sess.execs), sess.execs)
			}
			if sess.execs[1].sql != ReleaseLeaderLockSQL {
				t.Errorf("release SQL = %q, want %q", sess.execs[1].sql, ReleaseLeaderLockSQL)
			}
			if len(sess.execs[1].args) != 1 || sess.execs[1].args[0] != LeaderLockKey {
				t.Errorf("release args = %v, want [%d]", sess.execs[1].args, LeaderLockKey)
			}
			if sess.closes != 1 {
				t.Errorf("session closed %d times on release, want exactly 1", sess.closes)
			}
			// The lock never used the session after closing it: no pooled-style
			// check-in-then-reuse ever happened.
			if sess.usedAfter {
				t.Error("the leader lock issued a statement on a connection it had already returned/closed")
			}
		})

		t.Run("SessionLost closes on release", func(t *testing.T) {
			sess := &scriptedSession{}
			lock, err := newPgxLeaderLock(sess)
			if err != nil {
				t.Fatalf("newPgxLeaderLock: %v", err)
			}
			select {
			case <-lock.SessionLost():
				t.Fatal("SessionLost closed before the session ended")
			default:
			}
			if err := lock.Release(context.Background()); err != nil {
				t.Fatalf("Release: %v", err)
			}
			select {
			case <-lock.SessionLost():
			default:
				t.Error("SessionLost did not close after Release ended the session")
			}
		})

		t.Run("nil pinned connection is rejected", func(t *testing.T) {
			if _, err := newPgxLeaderLock(nil); !errors.Is(err, errNilPinnedConn) {
				t.Errorf("newPgxLeaderLock(nil) error = %v, want errNilPinnedConn", err)
			}
		})

		t.Run("Release surfaces both the unlock and the close error, neither dropped", func(t *testing.T) {
			unlockErr := errors.New("unlock failed")
			closeErr := errors.New("close failed")
			sess := &scriptedSession{unlockErr: unlockErr, closeErr: closeErr}
			lock, err := newPgxLeaderLock(sess)
			if err != nil {
				t.Fatalf("newPgxLeaderLock: %v", err)
			}
			if err := lock.Acquire(context.Background()); err != nil {
				t.Fatalf("Acquire: %v", err)
			}
			relErr := lock.Release(context.Background())
			if relErr == nil {
				t.Fatal("Release returned nil despite both unlock and close failing")
			}
			// Both errors are surfaced (joined): neither the unlock failure nor the
			// close/leaked-session failure is silently dropped.
			if !errors.Is(relErr, unlockErr) {
				t.Errorf("Release error does not carry the unlock failure: %v", relErr)
			}
			if !errors.Is(relErr, closeErr) {
				t.Errorf("Release error does not carry the close failure: %v", relErr)
			}
		})
	})
}
