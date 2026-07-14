// This file adds an in-memory fake of the leader-election lock
// (store.LeaderLock). A LockSet models one logical Postgres advisory lock
// contended by several daemon candidates: exactly one candidate holds it at a
// time, the rest block acquiring, and a release (or a scripted session loss)
// promotes the next waiter. This is the mechanism the election and single-writer
// wiring are proven against with no live Postgres (standby blocks, release
// promotes); E11 reuses it for the failover contracts.
package storetest

import (
	"context"
	"errors"
	"sync"

	"github.com/MateusAMP2119/iris-engine-cli/internal/store"
)

// ErrSessionEnded is returned by Acquire on a handle whose session has ended (a
// Release or a scripted LoseSession): a Postgres session that died cannot be
// revived, so a deposed candidate can never re-acquire the lock on its old
// session. Re-entering standby requires a FRESH session -- a new handle from the
// LockSet. This is what keeps a deposed leader's write guard refusing forever:
// its session has not, and can never have, re-acquired the lock.
var ErrSessionEnded = errors.New("storetest: lock session ended; contending again requires a fresh session")

// LockSet is one logical advisory lock several candidates contend for. It hands out
// FakeLock handles (one per candidate) that all race for the single lock: the token
// channel of capacity one is the lock itself, so exactly one Acquire holds it and
// the others block until it is released.
type LockSet struct {
	token chan struct{}
}

// NewLockSet returns a fresh logical lock, initially free.
func NewLockSet() *LockSet {
	return &LockSet{token: make(chan struct{}, 1)}
}

// New returns a candidate's handle to this logical lock. All handles from one
// LockSet contend for the same single lock, exactly as multiple daemon candidates
// contend for the one Postgres advisory lock.
func (s *LockSet) New() *FakeLock {
	return &FakeLock{set: s, lost: make(chan struct{})}
}

// FakeLock is one candidate's in-memory store.LeaderLock over a shared LockSet.
type FakeLock struct {
	set *LockSet

	mu     sync.Mutex
	held   bool
	closed bool
	lost   chan struct{}
}

// compile-time proof the fake satisfies the leader-lock seam it stands in for,
// and the gate the lock-guarded write connection consults before every meta write.
var (
	_ store.LeaderLock = (*FakeLock)(nil)
	_ store.LeaderGate = (*FakeLock)(nil)
)

// Acquire blocks until this candidate holds the shared lock or ctx is cancelled.
// With several handles from one LockSet, exactly one Acquire returns and the rest
// block here -- the standbys -- until the holder releases. A handle whose session
// has ended (Release or LoseSession) fails with ErrSessionEnded, blocked or not: a
// dead Postgres session cannot re-acquire; contending again takes a fresh session
// (a new handle).
func (l *FakeLock) Acquire(ctx context.Context) error {
	select {
	case l.set.token <- struct{}{}:
		l.mu.Lock()
		if l.closed {
			// The session ended while this candidate was blocked (or before it even
			// contended): hand the just-taken lock straight back so the next live
			// standby is promoted, and fail the acquire -- a dead session never leads.
			<-l.set.token
			l.mu.Unlock()
			return ErrSessionEnded
		}
		l.held = true
		l.mu.Unlock()
		return nil
	case <-l.lost:
		// The candidate's own session died while it stood blocked in the standby
		// queue: its blocking pg_advisory_lock call fails with the session.
		return ErrSessionEnded
	case <-ctx.Done():
		return ctx.Err()
	}
}

// Release relinquishes the shared lock, unblocking one waiting standby, and closes
// this handle's SessionLost channel. It is idempotent.
func (l *FakeLock) Release(context.Context) error {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.releaseLocked()
}

// releaseLocked frees the token if held and closes the lost channel once. The
// caller holds l.mu.
func (l *FakeLock) releaseLocked() error {
	if l.held {
		<-l.set.token
		l.held = false
	}
	if !l.closed {
		l.closed = true
		close(l.lost)
	}
	return nil
}

// SessionLost returns the channel closed when this candidate's lock session ends
// (on Release or a scripted LoseSession).
func (l *FakeLock) SessionLost() <-chan struct{} { return l.lost }

// LoseSession models the candidate's Postgres session dying: connection death
// releases the lock, so a blocked standby is promoted, and SessionLost fires. It
// is the hook E11's failover tests drive; E02.6 builds it for reuse.
func (l *FakeLock) LoseSession() {
	l.mu.Lock()
	defer l.mu.Unlock()
	_ = l.releaseLocked()
}

// Held reports whether this candidate currently holds the lock, for test assertions
// about which candidate became leader.
func (l *FakeLock) Held() bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.held
}
