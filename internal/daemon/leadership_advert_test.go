package daemon_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/MateusAMP2119/iris-lakehouse/internal/api"
	"github.com/MateusAMP2119/iris-lakehouse/internal/daemon"
	"github.com/MateusAMP2119/iris-lakehouse/internal/store"
	"github.com/MateusAMP2119/iris-lakehouse/internal/store/storetest"
)

// advertRecordingConn is a store.MetaWriteConn that records every statement AND its
// args, so a test can prove the leader advertised its address (the upsert into the
// leadership table) with the intended value.
type advertRecordingConn struct {
	mu    sync.Mutex
	execs []advertExec
}

type advertExec struct {
	sql  string
	args []any
}

func (c *advertRecordingConn) Exec(_ context.Context, sql string, args ...any) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.execs = append(c.execs, advertExec{sql: sql, args: args})
	return nil
}

// advertisedAddr returns the address the leader advertised (the arg of the leadership
// upsert), and whether the upsert was issued.
func (c *advertRecordingConn) advertisedAddr() (string, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	for _, e := range c.execs {
		if strings.Contains(e.sql, "INSERT INTO leadership") {
			if len(e.args) == 1 {
				if s, ok := e.args[0].(string); ok {
					return s, true
				}
			}
			return "", true
		}
	}
	return "", false
}

var _ store.MetaWriteConn = (*advertRecordingConn)(nil)

// blockingLock is a store.LeaderLock whose Acquire blocks until ctx is cancelled: a
// candidate holding it never wins leadership, so it stays a standby -- the shape the
// standby-poll leg of the advertisement test needs.
type blockingLock struct{ lost chan struct{} }

func newBlockingLock() *blockingLock { return &blockingLock{lost: make(chan struct{})} }

func (l *blockingLock) Acquire(ctx context.Context) error {
	<-ctx.Done()
	return ctx.Err()
}
func (l *blockingLock) Release(context.Context) error { return nil }
func (l *blockingLock) SessionLost() <-chan struct{}  { return l.lost }

// fixedLeaderAddr is a store.LeaderAddrReader returning a fixed advertised address:
// the fake meta advertisement a standby reads to name the leader.
type fixedLeaderAddr struct{ addr string }

func (r fixedLeaderAddr) LeaderAddr(context.Context) (string, error) { return r.addr, nil }

var _ store.LeaderAddrReader = fixedLeaderAddr{}

// notLeaderEnvelope decodes the not_leader error envelope, including the leader hint.
type advertEnvelope struct {
	Error struct {
		Code   string `json:"code"`
		Leader string `json:"leader"`
	} `json:"error"`
}

// TestLeaderAdvertisement proves the leader-advertisement mechanism at integration
// tier with fakes: a leader writes its advertised address into meta through the
// single writer, and a standby reads a meta advertisement and surfaces it in the
// not_leader rejection the CLI turns into exit-6 guidance -- so the guidance names
// the live leader for retargeting, no live Postgres.
func TestLeaderAdvertisement(t *testing.T) {
	const leaderAddr = "10.1.2.3:9099"

	t.Run("leader-advertises-address", func(t *testing.T) {
		t.Run("the leader advertises its address into meta on winning the lock", func(t *testing.T) {
			conn := &advertRecordingConn{}
			role := api.NewRoleState()
			cand := daemon.NewCandidate(storetest.NewLockSet().New(), role, conn, nil,
				daemon.WithLeaderAdvertiser(leaderAddr))

			ctx, cancel := context.WithCancel(context.Background())
			done := make(chan struct{})
			go func() { defer close(done); _ = cand.Serve(ctx) }()

			ok := pollUntil(func() bool { _, issued := conn.advertisedAddr(); return issued })
			cancel()
			<-done
			if !ok {
				t.Fatal("the leader never advertised its address into meta")
			}
			got, _ := conn.advertisedAddr()
			if got != leaderAddr {
				t.Errorf("advertised address = %q, want %q", got, leaderAddr)
			}
		})

		t.Run("a standby reads the advertisement and names the leader in the exit-6 envelope", func(t *testing.T) {
			role := api.NewRoleState()
			cand := daemon.NewCandidate(newBlockingLock(), role, &advertRecordingConn{}, nil,
				daemon.WithLeaderAddrReader(fixedLeaderAddr{addr: leaderAddr}))

			ctx, cancel := context.WithCancel(context.Background())
			done := make(chan struct{})
			go func() { defer close(done); _ = cand.Serve(ctx) }()
			t.Cleanup(func() { cancel(); <-done })

			// The standby polls the meta advertisement and records the leader address as
			// its guidance hint.
			if !pollUntil(func() bool { return role.LeaderHint() == leaderAddr }) {
				t.Fatalf("standby leader hint = %q, want %q (it must read the advertisement)", role.LeaderHint(), leaderAddr)
			}
			if role.Role() != api.RoleStandby {
				t.Errorf("role = %q, want standby", role.Role())
			}

			// The not_leader rejection the mux returns for a mutation names the live
			// leader for retargeting -- no longer "unknown".
			mux := api.NewMux(api.WithRole(role))
			rec := httptest.NewRecorder()
			mux.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/pipelines", nil))
			if rec.Code != api.StatusNotLeader {
				t.Fatalf("standby mutation status = %d, want %d", rec.Code, api.StatusNotLeader)
			}
			var env advertEnvelope
			if err := json.Unmarshal(rec.Body.Bytes(), &env); err != nil {
				t.Fatalf("decode not_leader envelope: %v", err)
			}
			if env.Error.Leader != leaderAddr {
				t.Errorf("exit-6 leader guidance = %q, want the advertised address %q", env.Error.Leader, leaderAddr)
			}
		})
	})
}
