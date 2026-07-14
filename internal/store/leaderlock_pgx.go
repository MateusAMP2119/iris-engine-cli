package store

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/jackc/pgx/v5"
)

// This file is the pgx-backed leader lock. The lock is held on a session-pinned
// connection: one dedicated *pgx.Conn, never a pooled connection whose return to
// a pool would silently release the advisory lock. The connection is obtained
// once (pgx.Connect, not a pool checkout), held for the whole leadership
// lifetime, and closed only on Release or session loss. While the lock is held,
// a watchdog pings the pinned session on an interval: a session that dies
// underneath a live daemon (Postgres restart, network drop, a terminated
// backend) fails the ping, which drops Held and fires SessionLost, so the
// daemon's self-demotion runs instead of the process staying a phantom leader.

const (
	// sessionPingInterval is how often the held-lock watchdog pings the pinned
	// session. It bounds how long a live daemon can keep reporting leader over a
	// dead meta session before self-demoting.
	sessionPingInterval = 5 * time.Second
	// sessionPingTimeout bounds one watchdog ping's round trip, measured from
	// AFTER the session mutex is acquired -- a ping queued behind a long meta
	// transaction must not count the wait as session death.
	sessionPingTimeout = 5 * time.Second
)

// pinnedConn is the session-pinned connection the leader lock holds. It is the
// minimal surface the advisory lock needs -- issue the acquire/release statements,
// probe the session's liveness, and close the session -- so a test can drive the
// lock mechanics against a scripted fake with no live Postgres. A single
// dedicated *pgx.Conn (adapted by pgxSessionConn) satisfies it; a pool never
// does, which is the whole point.
type pinnedConn interface {
	// exec issues one statement on the pinned session connection.
	exec(ctx context.Context, sql string, args ...any) error
	// ping probes the pinned session's liveness with one round trip; an error
	// means the session is dead (or unusable, which for a pinned session is the
	// same thing).
	ping(ctx context.Context) error
	// close ends the session, implicitly releasing any advisory lock it held.
	close(ctx context.Context) error
}

// pgxSessionConn adapts a dedicated *pgx.Conn to the pinnedConn seam. It is a
// single session connection, never drawn from a pool, so the advisory lock it
// carries is bound to this one session for its whole lifetime. mu is the session
// mutex shared with the write adapter over the same connection (pgxWriteConn):
// a *pgx.Conn is not safe for concurrent use, and the watchdog's ping runs on
// its own goroutine, so every statement on the session serializes through it.
type pgxSessionConn struct {
	conn *pgx.Conn
	mu   *sync.Mutex
}

// compile-time proof a dedicated pgx session connection satisfies the pinned seam
// (and, by construction, that the lock is never handed a pool).
var _ pinnedConn = (*pgxSessionConn)(nil)

func (c *pgxSessionConn) exec(ctx context.Context, sql string, args ...any) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	_, err := c.conn.Exec(ctx, sql, args...)
	return err
}

// ping probes the session with pgx's liveness round trip. The timeout starts
// only after the session mutex is held: a ping that waited out a long meta
// transaction still gets its full round-trip budget, so a busy session is never
// misread as a dead one.
func (c *pgxSessionConn) ping(ctx context.Context) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	ctx, cancel := context.WithTimeout(ctx, sessionPingTimeout)
	defer cancel()
	return c.conn.Ping(ctx)
}

func (c *pgxSessionConn) close(ctx context.Context) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.conn.Close(ctx)
}

// PgxLeaderLock is the pgx-backed leader-election lock, held on a session-pinned
// connection. It issues pg_advisory_lock on LeaderLockKey to acquire and
// pg_advisory_unlock to release, both on the same pinned session, so the lock's
// lifetime is exactly that session's -- the model failover relies on.
type PgxLeaderLock struct {
	conn pinnedConn

	// pingInterval is the held-lock watchdog's cadence, sessionPingInterval in
	// production; tests shorten it to drive session death without real waits.
	pingInterval time.Duration

	mu        sync.Mutex
	held      bool
	released  bool
	lostFired bool
	lost      chan struct{}
	// watchStop/watchDone bracket the watchdog goroutine: closing watchStop asks
	// it to exit, watchDone closing confirms it has, so Release never runs the
	// unlock/close statements while a ping is still possible.
	watchStop chan struct{}
	watchDone chan struct{}
}

// compile-time proof the pgx lock satisfies the behavioral seam the daemon elects
// against, and the gate the lock-guarded write connection consults before every
// meta write.
var (
	_ LeaderLock = (*PgxLeaderLock)(nil)
	_ LeaderGate = (*PgxLeaderLock)(nil)
)

// newPgxLeaderLock builds a leader lock over a pinned session connection. It is
// unexported: the only way to obtain one is through the meta client (Connect),
// which supplies a dedicated pgx session, never a pool.
func newPgxLeaderLock(conn pinnedConn) (*PgxLeaderLock, error) {
	if conn == nil {
		return nil, errNilPinnedConn
	}
	return &PgxLeaderLock{conn: conn, pingInterval: sessionPingInterval, lost: make(chan struct{})}, nil
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
	l.mu.Lock()
	l.held = true
	// The lock is held: start the session watchdog, so a session dying underneath
	// a live daemon drops Held and fires SessionLost instead of going unnoticed.
	if !l.released && !l.lostFired && l.watchStop == nil {
		l.watchStop = make(chan struct{})
		l.watchDone = make(chan struct{})
		// The watchdog outlives Acquire's call scope by design: it spans the whole
		// held-lock lifetime and is joined by Release, and its pings must not be
		// cancelled by the acquirer's context (a shutdown-cancelled ping would
		// misread a healthy session as dead).
		go l.watch(l.watchStop, l.watchDone) //nolint:gosec // G118: lock-lifetime goroutine, not request-scoped.
	}
	l.mu.Unlock()
	return nil
}

// watch is the held-lock session watchdog: it pings the pinned session on
// pingInterval and, on the first failed ping, marks the session lost (Held drops,
// SessionLost fires) and exits. A failed ping is conclusive: the pinned session
// is either dead or was killed by pgx on the ping's own timeout, and either way
// it can never carry the lock again. stop ends the watch without a verdict (a
// clean Release).
func (l *PgxLeaderLock) watch(stop, done chan struct{}) {
	defer close(done)
	ticker := time.NewTicker(l.pingInterval)
	defer ticker.Stop()
	for {
		select {
		case <-stop:
			return
		case <-ticker.C:
			if err := l.conn.ping(context.Background()); err != nil {
				l.markSessionLost()
				return
			}
		}
	}
}

// markSessionLost records a session death observed by the watchdog: Held drops,
// so the lock-guarded write connection refuses with ErrNoLeaderLock instead of
// surfacing raw connection errors, and SessionLost fires, so the daemon's
// self-demotion runs. It never touches the released flag: Release still owns the
// unlock/close teardown, which stays idempotent after a loss.
func (l *PgxLeaderLock) markSessionLost() {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.held = false
	if !l.lostFired {
		l.lostFired = true
		close(l.lost)
	}
}

// Held reports whether this session currently holds the leader lock: true from a
// successful Acquire until Release ends the session or the watchdog observes the
// session dead. It is the gate the lock-guarded write connection consults before
// every meta write, so a write is never issued over a session that has not
// (re-)acquired the lock.
func (l *PgxLeaderLock) Held() bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.held && !l.released
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
	l.held = false
	if !l.lostFired {
		l.lostFired = true
		close(l.lost)
	}
	stop, done := l.watchStop, l.watchDone
	l.watchStop, l.watchDone = nil, nil
	l.mu.Unlock()

	// Stop the watchdog and wait it out before ending the session, so no ping is
	// in flight (or can start) while the unlock and close statements run.
	if stop != nil {
		close(stop)
		<-done
	}

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

// SessionLost returns a channel closed when the lock's session ends: on Release,
// or when the held-lock watchdog observes the pinned session dead (a real
// connection death under a live daemon). The daemon's election loop watches the
// channel to self-demote; a standby blocked in Acquire is promoted by Postgres
// freeing the lock, not by this signal (its blocked pg_advisory_lock fails with
// the session instead).
func (l *PgxLeaderLock) SessionLost() <-chan struct{} { return l.lost }
