// This file adds an in-memory fake of the leader-election lock
// (store.LeaderLock). A LockSet models one logical Postgres advisory lock
// contended by several daemon candidates: exactly one candidate holds it at a
// time, the rest block acquiring, and a release (or a scripted session loss)
// promotes the next waiter. This is the mechanism the election and single-writer
// wiring are proven against with no live Postgres (standby blocks, release
// promotes); the failover contracts (self-demotion on session loss, standby
// promotion) are proven against the same fake.
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

// LockSet is one logical advisory lock several candidates contend for. It hands
// out FakeLock handles (one per candidate) that all race for the single lock:
// exactly one Acquire holds it and the others queue behind it in FIFO order. A
// release hands the lock directly to the queue's head, so a candidate that is
// observably waiting (Waiters) is promoted before any later arrival -- tests
// sequence failovers on that guarantee instead of on scheduling luck.
type LockSet struct {
	mu      sync.Mutex
	held    bool
	waiters []*waiter
}

// waiter is one queued Acquire. Its grant channel (capacity one, so a handoff
// never blocks the releasing candidate) receives when ownership transfers.
type waiter struct {
	grant chan struct{}
}

// NewLockSet returns a fresh logical lock, initially free.
func NewLockSet() *LockSet {
	return &LockSet{}
}

// New returns a candidate's handle to this logical lock. All handles from one
// LockSet contend for the same single lock, exactly as multiple daemon candidates
// contend for the one Postgres advisory lock.
func (s *LockSet) New() *FakeLock {
	return &FakeLock{set: s, lost: make(chan struct{})}
}

// Waiters reports how many candidates are queued for the lock, excluding the
// holder. It is the observable-contention hook for failover tests: once a
// candidate is counted here, a release is guaranteed to promote it ahead of any
// candidate that arrives later (the demoted leader's own fresh-session re-entry
// included), so a test can end the holder's session knowing who wins the lock.
func (s *LockSet) Waiters() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.waiters)
}

// releaseLocked passes the lock on: the queue's head is granted ownership, or
// the lock goes free if nobody waits. The caller holds s.mu.
func (s *LockSet) releaseLocked() {
	if len(s.waiters) > 0 {
		w := s.waiters[0]
		s.waiters = s.waiters[1:]
		w.grant <- struct{}{}
		return
	}
	s.held = false
}

// release is releaseLocked for callers not holding s.mu.
func (s *LockSet) release() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.releaseLocked()
}

// abandon withdraws a queued Acquire whose candidate stopped waiting (ctx
// cancelled or session lost). If the grant already landed -- the removal and the
// handoff are ordered by s.mu, so a waiter absent from the queue has a buffered
// grant -- ownership arrived for a candidate that can no longer take it and is
// passed straight on.
func (s *LockSet) abandon(w *waiter) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for i, queued := range s.waiters {
		if queued == w {
			s.waiters = append(s.waiters[:i], s.waiters[i+1:]...)
			return
		}
	}
	<-w.grant
	s.releaseLocked()
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
// queue here -- the standbys -- until the holder releases. A handle whose session
// has ended (Release or LoseSession) fails with ErrSessionEnded, queued or not: a
// dead Postgres session cannot re-acquire; contending again takes a fresh session
// (a new handle).
func (l *FakeLock) Acquire(ctx context.Context) error {
	// Mutex order is l.mu then s.mu everywhere (the session paths nest them
	// that way); here the handle check releases l.mu before s.mu is taken, so
	// the two are never held together in the opposite order.
	l.mu.Lock()
	if l.closed {
		l.mu.Unlock()
		return ErrSessionEnded
	}
	l.mu.Unlock()

	s := l.set
	s.mu.Lock()
	if !s.held {
		s.held = true
		s.mu.Unlock()
		return l.take()
	}
	w := &waiter{grant: make(chan struct{}, 1)}
	s.waiters = append(s.waiters, w)
	s.mu.Unlock()

	select {
	case <-w.grant:
		return l.take()
	case <-l.lost:
		// The candidate's own session died while it stood queued as a standby:
		// its blocking pg_advisory_lock call fails with the session.
		s.abandon(w)
		return ErrSessionEnded
	case <-ctx.Done():
		s.abandon(w)
		return ctx.Err()
	}
}

// take records ownership on this handle, unless the session ended while the
// grant was in flight -- a dead session never leads, so the just-received lock
// is passed straight on to the next live standby.
func (l *FakeLock) take() error {
	l.mu.Lock()
	if l.closed {
		l.mu.Unlock()
		l.set.release()
		return ErrSessionEnded
	}
	l.held = true
	l.mu.Unlock()
	return nil
}

// Release relinquishes the shared lock, promoting the next queued standby, and
// closes this handle's SessionLost channel. It is idempotent.
func (l *FakeLock) Release(context.Context) error {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.releaseLocked()
}

// releaseLocked frees the lock if held and closes the lost channel once. The
// caller holds l.mu.
func (l *FakeLock) releaseLocked() error {
	if l.held {
		l.held = false
		l.set.release()
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
// releases the lock, so a queued standby is promoted, and SessionLost fires. It
// is the hook the failover tests drive (store's own leaderlock_failover_test.go
// and the daemon's failover tests).
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
