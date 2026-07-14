package store

import (
	"context"
	"errors"
	"fmt"
)

// This file is the lock-guarded meta write connection: the enforcement of the
// failover single-writer rule that meta writes are never issued over a session
// that has not re-acquired the leader lock. On the live path the rule is physical
// -- the single write connection IS the lock-holding session (client.go), so a
// dead session fails its writes at Postgres -- but physics alone does not refuse
// a write issued before the lock is acquired, or one racing a demotion.
// LockGuardedConn closes that: it wraps the write connection and consults the
// leader lock before EVERY statement, refusing with ErrNoLeaderLock unless this
// session currently holds the lock. A deposed leader's session never re-acquires
// (re-acquisition happens only on a fresh session), so its guard refuses forever:
// across a failover there is no
// second writer and no overlapping run. The same guard runs over the meta-store
// fake, which is how the rule is proven at integration tier with no live
// Postgres.

// ErrNoLeaderLock is returned when a meta write is refused because the session it
// would ride does not currently hold the leader lock: the session never acquired
// it, released it, or died and was deposed. The write never reaches meta.
var ErrNoLeaderLock = errors.New("store: meta write refused: session does not hold the leader lock")

// LeaderGate is the view of the leader lock the write guard consults before every
// meta write: whether this session currently holds the lock, and the channel
// closed when the session ends. Both the pgx-backed lock (PgxLeaderLock) and the
// in-memory fake (storetest.FakeLock) satisfy it, so the guard runs identically
// over a live session and a scripted one.
type LeaderGate interface {
	// Held reports whether this session currently holds the leader lock.
	Held() bool
	// SessionLost returns the channel closed when the lock's session ends.
	SessionLost() <-chan struct{}
}

// LockGuardedConn is a meta write connection gated on the leader lock: every Exec
// and ExecTx first checks that the lock's session still holds leadership and
// refuses with ErrNoLeaderLock otherwise, so no meta write is ever issued over a
// session that has not re-acquired the lock. It wraps the session's raw write
// connection; the dispatcher's single Writer runs over it unchanged.
type LockGuardedConn struct {
	gate LeaderGate
	conn MetaWriteConn
}

// compile-time proof the guard stands wherever the raw write connection does: the
// single-Exec seam and the atomic-transaction seam both.
var (
	_ MetaWriteConn = (*LockGuardedConn)(nil)
	_ MetaTxConn    = (*LockGuardedConn)(nil)
)

// NewLockGuardedConn builds the lock-guarded write connection over the session's
// leader-lock gate and its raw write connection. Both are required: a guard
// without a gate could not refuse anything, and one without a connection could not
// write.
func NewLockGuardedConn(gate LeaderGate, conn MetaWriteConn) (*LockGuardedConn, error) {
	if gate == nil {
		return nil, errors.New("store: lock-guarded write connection requires a leader gate")
	}
	if conn == nil {
		return nil, errors.New("store: lock-guarded write connection requires a write connection")
	}
	return &LockGuardedConn{gate: gate, conn: conn}, nil
}

// check refuses unless the session currently holds the leader lock: a dead session
// (SessionLost fired) or one that has not acquired the lock is not the leader, so
// nothing may be written over it.
func (g *LockGuardedConn) check() error {
	select {
	case <-g.gate.SessionLost():
		return fmt.Errorf("%w (lock session ended; leadership requires a fresh session)", ErrNoLeaderLock)
	default:
	}
	if !g.gate.Held() {
		return fmt.Errorf("%w (lock not acquired on this session)", ErrNoLeaderLock)
	}
	return nil
}

// Exec issues one write statement on the lock-holding session, or refuses with
// ErrNoLeaderLock if this session does not hold the leader lock.
func (g *LockGuardedConn) Exec(ctx context.Context, sql string, args ...any) error {
	if err := g.check(); err != nil {
		return err
	}
	return g.conn.Exec(ctx, sql, args...)
}

// ExecTx runs stmts as one atomic transaction on the lock-holding session, or
// refuses the whole batch with ErrNoLeaderLock if this session does not hold the
// leader lock -- nothing from a refused batch ever reaches meta. A wrapped
// connection without transactional capability fails loudly (errNoTxConn), exactly
// as the unguarded path would.
func (g *LockGuardedConn) ExecTx(ctx context.Context, stmts []Statement) error {
	if err := g.check(); err != nil {
		return err
	}
	tx, ok := g.conn.(MetaTxConn)
	if !ok {
		return errNoTxConn
	}
	return tx.ExecTx(ctx, stmts)
}
