package daemon_test

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"testing"

	"github.com/MateusAMP2119/iris-engine-cli/internal/api"
	"github.com/MateusAMP2119/iris-engine-cli/internal/daemon"
	"github.com/MateusAMP2119/iris-engine-cli/internal/dispatch"
	"github.com/MateusAMP2119/iris-engine-cli/internal/store"
	"github.com/MateusAMP2119/iris-engine-cli/internal/store/storetest"
)

// evLog is a shared, ordered event record for the leader-path reconciliation test:
// meta writes, kills, and the dispatch-ready latch, in the order they occur, so the
// test can assert reconciliation completes before the latch fires.
type evLog struct {
	mu     sync.Mutex
	events []string
}

func (l *evLog) add(e string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.events = append(l.events, e)
}

func (l *evLog) snapshot() []string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return append([]string(nil), l.events...)
}

// evConn is a store.MetaWriteConn recording every write into the shared event log.
type evConn struct{ log *evLog }

func (c *evConn) Exec(_ context.Context, sql string, _ ...any) error {
	c.log.add("write:" + sql)
	return nil
}

// evKiller is a dispatch.GroupKiller recording every kill into the shared event log.
type evKiller struct{ log *evLog }

func (k *evKiller) KillGroup(pgid int) error {
	k.log.add(fmt.Sprintf("kill:%d", pgid))
	return nil
}

// seedReconcileReader builds a meta reader seeded with one leftover running run
// (recorded handle 6001) and one queued never-started run, identically for every
// candidate so cold start and failover reconcile the same fixtures.
func seedReconcileReader(t *testing.T) store.Reader {
	t.Helper()
	ctx := context.Background()
	f := storetest.New()
	r1, err := f.CreateRun(ctx, store.RunSpec{Pipeline: "p", Lane: "l"})
	if err != nil {
		t.Fatalf("seed CreateRun: %v", err)
	}
	if _, err := f.SetRunState(ctx, r1.ID, store.RunRunning, store.WithHandle(6001)); err != nil {
		t.Fatalf("seed SetRunState: %v", err)
	}
	if _, err := f.CreateRun(ctx, store.RunSpec{Pipeline: "p", Lane: "l"}); err != nil {
		t.Fatalf("seed CreateRun queued: %v", err)
	}
	return f
}

// reconcileProbe is one candidate wired for the reconciliation-ordering test: its
// candidate, role, shared event log, and lifecycle handles.
type reconcileProbe struct {
	cand   *daemon.Candidate
	role   *api.RoleState
	log    *evLog
	cancel context.CancelFunc
	done   chan struct{}
}

// newReconcileProbe builds a candidate over the given lock with reconciliation and
// the dispatch-ready latch wired to a shared event log, then runs Serve in the
// background. The latch appends a "ready" event, so the test can prove it fires only
// after reconciliation's writes and kills.
func newReconcileProbe(t *testing.T, lock store.LeaderLock) *reconcileProbe {
	t.Helper()
	log := &evLog{}
	role := api.NewRoleState()
	cand := daemon.NewCandidate(lock, role, &evConn{log: log}, nil,
		daemon.WithReconciliation(seedReconcileReader(t), &evKiller{log: log}, dispatch.SingleHostMatcher()),
		daemon.WithDispatchReady(func() { log.add("ready") }),
	)
	ctx, cancel := context.WithCancel(context.Background())
	p := &reconcileProbe{cand: cand, role: role, log: log, cancel: cancel, done: make(chan struct{})}
	go func() {
		defer close(p.done)
		_ = cand.Serve(ctx)
	}()
	return p
}

// awaitLeader blocks until the probe reports the leader role, then returns its event
// log snapshot.
func (p *reconcileProbe) awaitLeader(t *testing.T) []string {
	t.Helper()
	if !pollUntil(func() bool { return p.role.Role() == api.RoleLeader }) {
		t.Fatalf("candidate never became leader")
	}
	return p.log.snapshot()
}

// assertReconcileBeforeReady proves the dispatch-ready latch fired only after every
// reconciliation action (kill and disposal write) completed: no kill or write event
// appears after "ready", and the reconciliation actually did work before it.
func assertReconcileBeforeReady(t *testing.T, events []string) {
	t.Helper()
	readyAt := -1
	for i, e := range events {
		if e == "ready" {
			readyAt = i
			break
		}
	}
	if readyAt == -1 {
		t.Fatalf("dispatch-ready latch never fired: %v", events)
	}
	kills, disposals := 0, 0
	for i, e := range events {
		isKill := strings.HasPrefix(e, "kill:")
		isWrite := strings.HasPrefix(e, "write:")
		if isKill {
			kills++
		}
		// A reconcile disposal write is a run-record mutation (dead-letter or delete),
		// distinct from the leader's schema-DDL preamble (CREATE ...), so the ordering
		// assertion is about reconciliation's own output, never just EnsureSchema.
		if isWrite && isReconcileDisposal(e) {
			disposals++
		}
		if (isKill || isWrite) && i > readyAt {
			t.Errorf("reconciliation action %q (index %d) ran AFTER the dispatch-ready latch (index %d); reconciliation must complete before any lane dispatch: %v", e, i, readyAt, events)
		}
	}
	// Reconciliation must have actually disposed of the leftover runs before the
	// latch (dead-letter the running run, delete the queued one) and killed the
	// survivor -- otherwise the ordering assertion is vacuous.
	if kills == 0 {
		t.Errorf("no kill happened before dispatch-ready; the same-host survivor must be SIGKILLed during reconciliation: %v", events)
	}
	if disposals == 0 {
		t.Errorf("no run-record disposal write happened before dispatch-ready; reconciliation must dispose of leftover runs (not just re-check the schema): %v", events)
	}
}

// isReconcileDisposal reports whether a "write:<sql>" event is a reconciliation run
// disposal -- the atomic dead-letter CTE (matched by its INSERT INTO dead_letters
// clause) or the queued delete (DELETE FROM runs) -- as opposed to the leader's
// schema-DDL preamble. The dead-letter is one CTE beginning "WITH updated AS ...", so
// it is matched by a substring, not a prefix; "INSERT INTO dead_letters" also
// excludes the "CREATE TABLE ... dead_letters" schema statement.
func isReconcileDisposal(event string) bool {
	sql := strings.TrimPrefix(event, "write:")
	return strings.Contains(sql, "INSERT INTO dead_letters") ||
		strings.HasPrefix(sql, "DELETE FROM runs")
}

// TestReconcileBeforeDispatch proves the leader completes startup reconciliation
// before dispatching any lane, using identical logic on cold start and failover
// (crash recovery: leader runs startup reconciliation before any lane; cold start
// and failover identical). The dispatch-ready latch -- the seam the E05 lane
// dispatcher waits on -- fires only after reconciliation's kills and disposal
// writes complete; and a cold-start leader and a failover-promoted leader produce
// the identical action sequence on identical fixtures.
func TestReconcileBeforeDispatch(t *testing.T) {
	t.Run("reconcile-before-dispatch", func(t *testing.T) {
		t.Run("cold start: reconciliation completes before the dispatch-ready latch", func(t *testing.T) {
			set := storetest.NewLockSet()
			p := newReconcileProbe(t, set.New())
			t.Cleanup(func() { p.cancel(); <-p.done })

			events := p.awaitLeader(t)
			assertReconcileBeforeReady(t, events)
		})

		t.Run("failover uses the identical logic as cold start", func(t *testing.T) {
			// Cold start: a single candidate wins the lock immediately and reconciles.
			coldSet := storetest.NewLockSet()
			cold := newReconcileProbe(t, coldSet.New())
			t.Cleanup(func() { cold.cancel(); <-cold.done })
			coldEvents := cold.awaitLeader(t)
			assertReconcileBeforeReady(t, coldEvents)

			// Failover: two candidates contend; the first leads, then departs, and the
			// standby is promoted and runs the same lead() path over the same fixtures.
			failSet := storetest.NewLockSet()
			first := newReconcileProbe(t, failSet.New())
			second := newReconcileProbe(t, failSet.New())
			t.Cleanup(func() {
				first.cancel()
				<-first.done
				second.cancel()
				<-second.done
			})
			// Wait for one of them to lead, depose it, and let the other take over.
			if !pollUntil(func() bool {
				return first.role.Role() == api.RoleLeader || second.role.Role() == api.RoleLeader
			}) {
				t.Fatalf("no candidate led in the failover set")
			}
			leader, standby := first, second
			if second.role.Role() == api.RoleLeader {
				leader, standby = second, first
			}
			leader.cancel()
			<-leader.done
			if !pollUntil(func() bool { return standby.role.Role() == api.RoleLeader }) {
				t.Fatalf("the standby was not promoted after the leader departed")
			}
			failEvents := standby.log.snapshot()
			assertReconcileBeforeReady(t, failEvents)

			// Identical logic: the cold-start leader and the failover-promoted leader
			// run the identical lead() path over identical fixtures, so their full event
			// logs (schema re-check, kills, disposals, ready) must match exactly.
			got, want := failEvents, coldEvents
			if len(got) != len(want) {
				t.Fatalf("failover action count %d != cold-start %d:\n failover=%v\n cold=%v", len(got), len(want), got, want)
			}
			for i := range want {
				if got[i] != want[i] {
					t.Errorf("action[%d]: failover=%q, cold=%q (cold start and failover must run identical reconciliation)", i, got[i], want[i])
				}
			}
		})
	})
}
