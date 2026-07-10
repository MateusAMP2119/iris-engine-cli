package daemon

// Internal tests for the production fresh-session wiring (freshLeaderSession): the
// "re-enters standby on a FRESH session" half of self-demotion (specification
// section 15), proven over a fake session maker with no live database. The
// Candidate-level behavior (kill in-flight, re-enter, re-lead) is proven in
// failover_test.go over the store fakes; these cover the production callback's own
// logic -- transient-failure retry and the shutdown fallback.

import (
	"context"
	"errors"
	"testing"

	"github.com/MateusAMP2119/iris-engine-cli/internal/store"
	"github.com/MateusAMP2119/iris-engine-cli/internal/store/storetest"
)

// fakeSessionMaker mints a fresh session after failN failures, so a test can drive
// the retry loop deterministically.
type fakeSessionMaker struct {
	failN  int
	calls  int
	lock   store.LeaderLock
	writer store.MetaWriteConn
	err    error
}

func (m *fakeSessionMaker) NewLeaderSession(context.Context) (store.LeaderLock, store.MetaWriteConn, error) {
	m.calls++
	if m.calls <= m.failN {
		if m.err != nil {
			return nil, nil, m.err
		}
		return nil, nil, errors.New("meta blip")
	}
	return m.lock, m.writer, nil
}

// TestFreshLeaderSessionRetriesUntilOpen proves the production fresh-session callback
// survives a transient meta-database failure: a self-demotion must not end the daemon
// just because minting the fresh session blipped once, so it retries and returns the
// eventually-opened session for standby re-entry.
//
// spec: S15/self-demotion-on-session-loss
func TestFreshLeaderSessionRetriesUntilOpen(t *testing.T) {
	set := storetest.NewLockSet()
	wantLock := set.New()
	wantWriter := storetest.NewWriteRecorder()
	maker := &fakeSessionMaker{failN: 1, lock: wantLock, writer: wantWriter}

	fresh := freshLeaderSession(context.Background(), maker, nil)
	gotLock, gotWriter := fresh()

	if maker.calls != 2 {
		t.Errorf("maker called %d times, want 2 (one failure then one success)", maker.calls)
	}
	if gotLock != store.LeaderLock(wantLock) {
		t.Errorf("fresh session returned a different lock than the maker minted")
	}
	if gotWriter != store.MetaWriteConn(wantWriter) {
		t.Errorf("fresh session returned a different writer than the maker minted")
	}
}

// TestFreshLeaderSessionShutdownRefuses proves that when the daemon is shutting down
// (ctx cancelled), the fresh-session callback does not spin forever trying to mint a
// session it will never use: it returns a lock that refuses to acquire, so Serve's
// re-entry loop returns cleanly.
//
// spec: S15/self-demotion-on-session-loss
func TestFreshLeaderSessionShutdownRefuses(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	maker := &fakeSessionMaker{err: errors.New("should not be called")}

	fresh := freshLeaderSession(ctx, maker, nil)
	gotLock, gotWriter := fresh()

	if maker.calls != 0 {
		t.Errorf("maker called %d times after shutdown; the callback must not mint a session it cannot use", maker.calls)
	}
	if gotWriter != nil {
		t.Errorf("shutdown fallback returned a non-nil writer; a refusing lock never pairs with a usable writer")
	}
	if err := gotLock.Acquire(context.Background()); !errors.Is(err, context.Canceled) {
		t.Errorf("shutdown fallback Acquire returned %v, want context.Canceled so Serve exits cleanly", err)
	}
}
