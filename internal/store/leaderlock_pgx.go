package store

import (
	"context"
	"errors"
	"fmt"
	"sync"

	"github.com/jackc/pgx/v5"
)

// This file is the pgx-backed leader lock. The lock is held on a session-pinned
// connection: one dedicated *pgx.Conn, never a pooled connection whose return to a
// pool would silently release the advisory lock (specification section 9). The
// connection is obtained once (pgx.Connect, not a pool checkout), held for the
// whole leadership lifetime, and closed only on Release or session loss.

// pinnedConn is the session-pinned connection the leader lock holds. It is the
// minimal surface the advisory lock needs -- issue the acquire/release statements
// and close the session -- so a test can drive the lock mechanics against a
// scripted fake with no live Postgres. A single dedicated *pgx.Conn (adapted by
// pgxSessionConn) satisfies it; a pool never does, which is the whole point. The
// active session-liveness watch (a ping loop that fires SessionLost on real
// connection death) lands with failover in E11; E02.6 fires SessionLost on Release.
type pinnedConn interface {
	// exec issues one statement on the pinned session connection.
	exec(ctx context.Context, sql string, args ...any) error
	// close ends the session, implicitly releasing any advisory lock it held.
	close(ctx context.Context) error
}

// pgxSessionConn adapts a dedicated *pgx.Conn to the pinnedConn seam. It is a
// single session connection, never drawn from a pool, so the advisory lock it
// carries is bound to this one session for its whole lifetime.
type pgxSessionConn struct {
	conn *pgx.Conn
}

// compile-time proof a dedicated pgx session connection satisfies the pinned seam
// (and, by construction, that the lock is never handed a pool).
var _ pinnedConn = (*pgxSessionConn)(nil)

func (c *pgxSessionConn) exec(ctx context.Context, sql string, args ...any) error {
	_, err := c.conn.Exec(ctx, sql, args...)
	return err
}

func (c *pgxSessionConn) close(ctx context.Context) error { return c.conn.Close(ctx) }

// PgxLeaderLock is the pgx-backed leader-election lock, held on a session-pinned
// connection. It issues pg_advisory_lock on LeaderLockKey to acquire and
// pg_advisory_unlock to release, both on the same pinned session, so the lock's
// lifetime is exactly that session's -- the model failover relies on.
type PgxLeaderLock struct {
	conn pinnedConn

	mu       sync.Mutex
	released bool
	lost     chan struct{}
}

// compile-time proof the pgx lock satisfies the behavioral seam the daemon elects
// against.
var _ LeaderLock = (*PgxLeaderLock)(nil)

// newPgxLeaderLock builds a leader lock over a pinned session connection. It is
// unexported: the only way to obtain one is through the meta client (Connect),
// which supplies a dedicated pgx session, never a pool.
func newPgxLeaderLock(conn pinnedConn) (*PgxLeaderLock, error) {
	if conn == nil {
		return nil, errNilPinnedConn
	}
	return &PgxLeaderLock{conn: conn, lost: make(chan struct{})}, nil
}

// Acquire blocks until this candidate holds the leader lock. pg_advisory_lock is a
// blocking session-level acquire: it returns only once the lock on LeaderLockKey
// is granted, so a standby stays blocked here until the current leader's session
// releases it (or dies). The statement runs on the pinned session connection, so
// the lock is bound to that session.
func (l *PgxLeaderLock) Acquire(ctx context.Context) error {
	if err := l.conn.exec(ctx, AcquireLeaderLockSQL, LeaderLockKey); err != nil {
		return fmt.Errorf("store: acquire leader lock: %w", err)
	}
	return nil
}

// Release relinquishes the advisory lock and closes the pinned session. It runs
// pg_advisory_unlock on the same session that acquired the lock (a session may
// only unlock a lock it holds), then closes the connection, which would release
// the lock even if the unlock statement failed. It is idempotent.
func (l *PgxLeaderLock) Release(ctx context.Context) error {
	l.mu.Lock()
	if l.released {
		l.mu.Unlock()
		return nil
	}
	l.released = true
	close(l.lost)
	l.mu.Unlock()

	unlockErr := l.conn.exec(ctx, ReleaseLeaderLockSQL, LeaderLockKey)
	closeErr := l.conn.close(ctx)
	// Closing the session releases the lock regardless of the unlock result, so the
	// close is the authoritative release and its error is primary; a concurrent
	// unlock error is joined for diagnosis rather than dropped, so a leaked session
	// (close failed) is never masked and the unlock failure is never lost.
	var errs []error
	if closeErr != nil {
		errs = append(errs, fmt.Errorf("store: close leader-lock session: %w", closeErr))
	}
	if unlockErr != nil {
		errs = append(errs, fmt.Errorf("store: release leader lock: %w", unlockErr))
	}
	return errors.Join(errs...)
}

// SessionLost returns a channel closed when the lock's session ends: on Release, or
// when a liveness check finds the session dead. The daemon watches it to
// self-demote; E11 drives promotion off it.
func (l *PgxLeaderLock) SessionLost() <-chan struct{} { return l.lost }
