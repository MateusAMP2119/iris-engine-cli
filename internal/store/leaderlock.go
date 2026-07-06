package store

import (
	"context"
	"fmt"
)

// This file holds the leader-election lock: the Postgres session advisory lock a
// candidate acquires to become the sole leader (specification sections 2, 9, and
// 15). Leadership is a single advisory lock: the leader holds it, standbys block
// acquiring it, and connection death releases it so the next standby acquires and
// becomes leader. The lock MUST be held on a session-pinned connection -- one
// dedicated pgx *Conn, never a pooled connection whose return to the pool would
// release the lock (specification section 9: "a session-pinned connection pooling
// would break"). This package owns that lock; failover (self-demotion on session
// loss, standby promotion) is E11 -- E02.6 lands election and the single-writer
// path.

// LeaderLockKey is the fixed 64-bit key of the leader-election advisory lock
// (specification section 15). It is a single, documented constant so every engine
// candidate contends for the identical lock: the high 32 bits spell the ASCII of
// "iris" (0x69726973) and the low 32 bits are the lock purpose (0x00000001 =
// leader election), leaving room for future engine-owned advisory locks under the
// same "iris" namespace without colliding with this one.
const LeaderLockKey int64 = 0x6972697300000001

// AcquireLeaderLockSQL is the blocking acquire: pg_advisory_lock waits until the
// session-level lock on LeaderLockKey is granted, so a standby's Acquire blocks
// exactly as long as another session holds leadership. It is a session-level lock
// (not xact-level), so it is held until explicitly released or the session ends,
// which is precisely the leadership lifetime.
const AcquireLeaderLockSQL = "SELECT pg_advisory_lock($1)"

// ReleaseLeaderLockSQL releases the session-level advisory lock on LeaderLockKey.
// It is the explicit demotion path; connection death releases the lock implicitly
// too (the whole basis of failover), so a failed release is not fatal to
// correctness -- the session ending frees the lock regardless.
const ReleaseLeaderLockSQL = "SELECT pg_advisory_unlock($1)"

// LeaderLock is the leader-election lock seam. Acquire blocks until leadership is
// held; Release relinquishes it; SessionLost signals that the underlying session
// died (connection death releases the lock, the basis of failover). A pgx-backed
// implementation holds it on a session-pinned connection (leaderlock_pgx.go); an
// in-memory fake (internal/store/storetest) scripts acquire/block/release so the
// election wiring is proven with no live Postgres.
type LeaderLock interface {
	// Acquire blocks until this candidate holds the leader lock, or ctx is
	// cancelled. A standby's Acquire stays blocked as long as the leader holds it.
	Acquire(ctx context.Context) error
	// Release relinquishes the leader lock, so a blocked standby can acquire it.
	Release(ctx context.Context) error
	// SessionLost returns a channel closed when the lock's session dies. Connection
	// death releases the lock (specification section 15); the daemon watches this to
	// self-demote. Failover consumption of it is E11.
	SessionLost() <-chan struct{}
}

// errNilPinnedConn guards construction of a pgx lock with no session connection.
var errNilPinnedConn = fmt.Errorf("store: leader lock requires a session-pinned connection")
