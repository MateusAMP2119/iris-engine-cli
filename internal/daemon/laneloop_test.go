package daemon_test

import (
	"context"
	"reflect"
	"sync"
	"testing"

	"github.com/MateusAMP2119/iris-engine-cli/internal/api"
	"github.com/MateusAMP2119/iris-engine-cli/internal/daemon"
	"github.com/MateusAMP2119/iris-engine-cli/internal/dispatch"
	"github.com/MateusAMP2119/iris-engine-cli/internal/store"
	"github.com/MateusAMP2119/iris-engine-cli/internal/store/storetest"
)

// loopEvents is an ordered, concurrency-safe record of the lane loop's run and post-pass
// events, so a test can prove the leader-driven loop runs post-pass bookkeeping only after
// a lane pass completes.
type loopEvents struct {
	mu     sync.Mutex
	events []string
}

func (e *loopEvents) add(s string) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.events = append(e.events, s)
}

func (e *loopEvents) has(s string) bool {
	e.mu.Lock()
	defer e.mu.Unlock()
	for _, ev := range e.events {
		if ev == s {
			return true
		}
	}
	return false
}

func (e *loopEvents) prefix(n int) []string {
	e.mu.Lock()
	defer e.mu.Unlock()
	if len(e.events) < n {
		return append([]string(nil), e.events...)
	}
	return append([]string(nil), e.events[:n]...)
}

// loopWalkFake is a fixed WalkReader: the leader-driven loop reads this walk at each pass
// start.
type loopWalkFake struct{ lanes []dispatch.Lane }

func (w loopWalkFake) Walk(context.Context) ([]dispatch.Lane, error) {
	return append([]dispatch.Lane(nil), w.lanes...), nil
}

// loopGateFake is an ungated PassGate: every member runs every pass.
type loopGateFake struct{}

func (loopGateFake) Eligible(context.Context, string) (dispatch.Decision, error) {
	return dispatch.Decision{Run: true}, nil
}

// loopRunnerFake records each fresh run and returns at once, so the leader-driven loop
// advances without a live subprocess.
type loopRunnerFake struct{ ev *loopEvents }

func (r loopRunnerFake) StartFresh(_ context.Context, rec store.RunRecord) (dispatch.RunOutcome, error) {
	r.ev.add("run:" + rec.Pipeline)
	return dispatch.RunSucceeded, nil
}

// loopPostFake records the post-pass bookkeeping call.
type loopPostFake struct{ ev *loopEvents }

func (p loopPostFake) AfterPass(_ context.Context, report dispatch.PassReport) error {
	p.ev.add("post:" + report.Lane)
	return nil
}

// TestLeaderDrivesLaneLoop proves the daemon leader drives the perpetual lane loop over
// its single dispatcher once leadership is won: the loop starts eligible members in
// composer order and runs the dispatcher-owned post-pass bookkeeping only after the pass
// completes, never mid-pass, and it stops cleanly when leadership ends (specification
// sections 6.1 and 6.3). This is the daemon-side wiring of the pass loop: lead() composes
// the loop over the dispatcher and binds it to leadership.
func TestLeaderDrivesLaneLoop(t *testing.T) {
	t.Run("S06.1/dispatcher-post-pass-only", func(t *testing.T) {
		set := storetest.NewLockSet()
		role := api.NewRoleState()
		ev := &loopEvents{}
		build := func(dispatch.Submitter) *dispatch.Loop {
			return dispatch.NewLoop(
				loopWalkFake{lanes: []dispatch.Lane{{Name: "etl", Pipelines: []string{"a", "b"}}}},
				loopGateFake{},
				loopRunnerFake{ev: ev},
				nil,
				dispatch.WithPostPass(loopPostFake{ev: ev}),
			)
		}
		cand := daemon.NewCandidate(set.New(), role, &leaderWriteConn{}, nil, daemon.WithLaneLoop(build))

		ctx, cancel := context.WithCancel(context.Background())
		done := make(chan struct{})
		go func() {
			defer close(done)
			_ = cand.Serve(ctx)
		}()
		defer func() { cancel(); <-done }()

		// The leader wins the lock and drives a lane pass followed by post-pass bookkeeping.
		if !pollUntil(func() bool { return ev.has("post:etl") }) {
			t.Fatal("leader did not drive a lane pass with post-pass bookkeeping")
		}
		if role.Role() != api.RoleLeader {
			t.Fatal("candidate driving the loop did not report the leader role")
		}

		// The pass's runs come first, in composer order, then the single post-pass event --
		// never interleaved mid-pass.
		if got, want := ev.prefix(3), []string{"run:a", "run:b", "post:etl"}; !reflect.DeepEqual(got, want) {
			t.Fatalf("leader-driven event order = %v, want %v (post-pass only after the pass)", got, want)
		}

		// Demotion stops the loop cleanly: cancelling leadership returns Serve promptly (the
		// deferred cancel + join runs here), so a deposed leader stops dispatching.
		cancel()
		<-done
		if role.Role() == api.RoleLeader {
			t.Fatal("a candidate whose leadership ended still reports the leader role")
		}
	})
}
