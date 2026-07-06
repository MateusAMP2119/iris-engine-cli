package daemon_test

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/MateusAMP2119/iris-engine-cli/internal/api"
	"github.com/MateusAMP2119/iris-engine-cli/internal/daemon"
	"github.com/MateusAMP2119/iris-engine-cli/internal/store"
	"github.com/MateusAMP2119/iris-engine-cli/internal/store/storetest"
)

// leaderWriteConn is a store.MetaWriteConn that records the writes a leader's
// dispatcher issues, so a test can prove only the leader wrote meta.
type leaderWriteConn struct {
	mu    sync.Mutex
	stmts []string
}

func (c *leaderWriteConn) Exec(_ context.Context, sql string, _ ...any) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.stmts = append(c.stmts, sql)
	return nil
}

func (c *leaderWriteConn) wrote() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return len(c.stmts) > 0
}

// compile-time proof the recorder stands in for the leader's meta write connection.
var _ store.MetaWriteConn = (*leaderWriteConn)(nil)

// acquireErrLock is a store.LeaderLock whose Acquire returns a scripted error, so a
// test can drive Serve's acquire-error branch (ctx-cancel is a clean shutdown; any
// other error propagates) with no real lock contention.
type acquireErrLock struct {
	acquireErr error
	lost       chan struct{}
}

func (l *acquireErrLock) Acquire(context.Context) error { return l.acquireErr }
func (l *acquireErrLock) Release(context.Context) error { return nil }
func (l *acquireErrLock) SessionLost() <-chan struct{}  { return l.lost }

// TestCandidateServeShutdown proves Serve's acquire-error handling: a context
// cancellation while still a standby is a clean shutdown (nil), while any other
// acquire failure propagates as an error -- and either way the candidate never
// reports the leader role.
//
// spec: S02/one-leader-sole-dispatcher
func TestCandidateServeShutdown(t *testing.T) {
	t.Run("ctx cancellation before winning the lock is a clean shutdown (nil)", func(t *testing.T) {
		for _, ctxErr := range []error{context.Canceled, context.DeadlineExceeded} {
			role := api.NewRoleState()
			cand := daemon.NewCandidate(&acquireErrLock{acquireErr: ctxErr}, role, &leaderWriteConn{}, nil)
			if err := cand.Serve(context.Background()); err != nil {
				t.Errorf("Serve with acquire=%v returned %v, want nil (clean standby shutdown)", ctxErr, err)
			}
			if role.Role() == api.RoleLeader {
				t.Error("a candidate that never acquired the lock reported the leader role")
			}
		}
	})

	t.Run("a non-ctx acquire failure propagates", func(t *testing.T) {
		boom := errors.New("meta session died")
		role := api.NewRoleState()
		cand := daemon.NewCandidate(&acquireErrLock{acquireErr: boom}, role, &leaderWriteConn{}, nil)
		err := cand.Serve(context.Background())
		if !errors.Is(err, boom) {
			t.Errorf("Serve error = %v, want it to wrap %v", err, boom)
		}
		if role.Role() == api.RoleLeader {
			t.Error("a candidate whose acquire failed reported the leader role")
		}
	})
}

// candidate bundles one daemon candidate for the election test: its Candidate, its
// role state, its meta write conn, and the context that keeps it running.
type candidate struct {
	cand   *daemon.Candidate
	role   *api.RoleState
	conn   *leaderWriteConn
	cancel context.CancelFunc
	done   chan struct{}
}

// pollUntil waits until cond holds or the deadline passes, polling on a condition
// (never a fixed sleep standing in for readiness): it returns whether cond held.
func pollUntil(cond func() bool) bool {
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return true
		}
		time.Sleep(2 * time.Millisecond)
	}
	return cond()
}

// leaders returns the candidates currently reporting the leader role.
func leaders(cands []*candidate) []*candidate {
	var out []*candidate
	for _, c := range cands {
		if c.role.Role() == api.RoleLeader {
			out = append(out, c)
		}
	}
	return out
}

// TestLeaderElectionSingleWriter proves the leader-election single-writer path with
// several candidates contending for one advisory lock: exactly one acquires
// leadership and starts dispatching, only that leader writes meta (through its one
// dispatcher), and the rest stay standby -- and on the leader's departure a standby
// is promoted and takes over the single-writer path.
func TestLeaderElectionSingleWriter(t *testing.T) {
	set := storetest.NewLockSet()
	const n = 5
	cands := make([]*candidate, n)
	for i := range cands {
		ctx, cancel := context.WithCancel(context.Background())
		role := api.NewRoleState()
		conn := &leaderWriteConn{}
		c := &candidate{
			cand:   daemon.NewCandidate(set.New(), role, conn, nil),
			role:   role,
			conn:   conn,
			cancel: cancel,
			done:   make(chan struct{}),
		}
		cands[i] = c
		go func() {
			defer close(c.done)
			_ = c.cand.Serve(ctx)
		}()
	}
	t.Cleanup(func() {
		for _, c := range cands {
			c.cancel()
			<-c.done
		}
	})

	// spec: S02/one-leader-sole-dispatcher
	t.Run("S02/one-leader-sole-dispatcher", func(t *testing.T) {
		if !pollUntil(func() bool { return len(leaders(cands)) == 1 }) {
			t.Fatalf("want exactly one leader, got %d", len(leaders(cands)))
		}
		ls := leaders(cands)
		if len(ls) != 1 {
			t.Fatalf("exactly one candidate must lead, got %d", len(ls))
		}
		// Every other candidate is a standby (blocked acquiring the lock), never a
		// second leader.
		standbys := 0
		for _, c := range cands {
			if c.role.Role() == api.RoleStandby {
				standbys++
			}
		}
		if standbys != n-1 {
			t.Errorf("standbys = %d, want %d (all non-leaders block on the lock)", standbys, n-1)
		}
	})

	// spec: S02/leader-only-meta-writes
	// spec: S04/only-leader-writes-meta
	t.Run("only the leader dispatches meta writes", func(t *testing.T) {
		if !pollUntil(func() bool { return len(leaders(cands)) == 1 }) {
			t.Fatalf("no single leader")
		}
		leader := leaders(cands)[0]
		// The leader's dispatcher issued the schema re-check: only it wrote meta.
		if !pollUntil(leader.conn.wrote) {
			t.Error("the leader did not write meta through its dispatcher")
		}
		for _, c := range cands {
			if c == leader {
				continue
			}
			if c.conn.wrote() {
				t.Error("a standby wrote meta; only the leader writes meta")
			}
		}
	})

	// Promotion: when the leader departs, a standby acquires the lock and becomes the
	// new single writer -- the failover the advisory lock provides.
	t.Run("a standby is promoted when the leader departs", func(t *testing.T) {
		if !pollUntil(func() bool { return len(leaders(cands)) == 1 }) {
			t.Fatalf("no single leader to depose")
		}
		old := leaders(cands)[0]
		old.cancel()
		<-old.done

		if !pollUntil(func() bool {
			ls := leaders(cands)
			return len(ls) == 1 && ls[0] != old
		}) {
			t.Fatalf("no new leader was promoted after the old leader departed (leaders=%d)", len(leaders(cands)))
		}
		newLeader := leaders(cands)[0]
		if !pollUntil(newLeader.conn.wrote) {
			t.Error("the promoted leader did not take over the single-writer path")
		}
	})
}
