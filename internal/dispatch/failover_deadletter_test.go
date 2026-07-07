package dispatch_test

// This file is the E11.3 failover cost test (specification section 15): "stopped
// runs dead-letter and poison dependents' next consumption; unsticking a chain is
// an explicit `iris deadletter replay`". It composes, over the meta-store fake and
// with no live Postgres, the exact production pieces a failover exercises in
// order: the new leader's startup reconciliation (the failover kill's disposal
// path -- the same Reconciler cold start uses), the depends_on gate and its
// propagation plan (the E05.6 poisoning mechanism -- this test proves failover-
// killed runs feed that same path), and the pure replay resolution (E05.7). The
// chain is stuck for as many passes as no one replays, and only the explicit
// replay -- never time, never a retry -- unsticks it.

import (
	"context"
	"testing"

	"github.com/MateusAMP2119/iris-engine-cli/internal/dispatch"
	"github.com/MateusAMP2119/iris-engine-cli/internal/store"
	"github.com/MateusAMP2119/iris-engine-cli/internal/store/storetest"
)

// latestUpstreamEdge derives a dependent's depends_on edge to upstream from the
// fake meta's run records, exactly as the production edge readers do: the
// upstream's LATEST run (never a backlog), mapped to the gate's upstream-state
// enum, with the run's ordering identity as the edge's run id. AwaitedFrom is
// zero: the edge predates every seeded run, so every run is awaited.
func latestUpstreamEdge(t *testing.T, f *storetest.Fake, upstream string) dispatch.Edge {
	t.Helper()
	runs, err := f.ListRuns(context.Background(), store.RunFilter{Pipeline: upstream})
	if err != nil {
		t.Fatalf("ListRuns(%s): %v", upstream, err)
	}
	if len(runs) == 0 {
		return dispatch.Edge{Upstream: upstream, Latest: dispatch.UpstreamNone}
	}
	last := runs[len(runs)-1]
	var latest dispatch.UpstreamState
	switch last.State {
	case store.RunQueued, store.RunRunning:
		latest = dispatch.UpstreamPending
	case store.RunSucceeded:
		latest = dispatch.UpstreamSucceeded
	case store.RunDeadLettered:
		latest = dispatch.UpstreamDeadLettered
	default:
		latest = dispatch.UpstreamNone
	}
	return dispatch.Edge{Upstream: upstream, Latest: latest, LatestRunID: last.Seq}
}

// TestFailoverStoppedRunsDeadLetter proves the accepted failover cost end to end
// (specification section 15): a run stopped by a failover is dead-lettered
// (stopped, daemon-terminated) by the new leader's startup reconciliation, that
// dead-letter poisons its dependent's next consumption -- and every consumption
// after that -- and the chain unsticks only through the explicit
// `iris deadletter replay` resolution, never on its own.
func TestFailoverStoppedRunsDeadLetter(t *testing.T) {
	t.Run("S15/failover-stopped-runs-dead-letter", func(t *testing.T) {
		ctx := context.Background()
		f := storetest.New()

		// The upstream's run was in flight when the leader died: the leftover record
		// the failover-promoted leader finds.
		up, err := f.CreateRun(ctx, store.RunSpec{Pipeline: "extract", Lane: "l"})
		if err != nil {
			t.Fatalf("seed CreateRun: %v", err)
		}
		if _, err := f.SetRunState(ctx, up.ID, store.RunRunning, store.WithHandle(9001)); err != nil {
			t.Fatalf("seed SetRunState running: %v", err)
		}

		// Phase 1 -- the failover kill's disposal: the new leader's startup
		// reconciliation (cross-host, so it kills nothing) dead-letters the run as
		// stopped, daemon-terminated.
		log := &actionLog{}
		conn := &recordingWriteConnArgs{log: log}
		d := dispatch.New(conn)
		d.Start(ctx)
		defer d.Stop()
		killer := &recordingKiller{log: log}
		crossHost := dispatch.HostMatcher(func(store.Run) bool { return false })
		rec := dispatch.NewReconciler(f, d, killer, crossHost, nil)
		if err := rec.Reconcile(ctx); err != nil {
			t.Fatalf("failover reconciliation: %v", err)
		}
		dl, ok := findDeadLetter(conn.recorded(), up.ID)
		if !ok {
			t.Fatalf("failover reconciliation did not dead-letter the stopped run %s: %v", up.ID, conn.recorded())
		}
		if len(dl.args) != 5 || dl.args[3] != store.ReasonStopped || dl.args[4] != dispatch.DaemonTerminatedDetail {
			t.Fatalf("failover dead-letter args = %v, want reason %q and detail %q",
				dl.args, store.ReasonStopped, dispatch.DaemonTerminatedDetail)
		}
		// Mirror the recorded disposal into the fake meta -- state and reason exactly
		// as the recorded write's arguments say -- so the gate reads what the write
		// would have left in meta.
		if _, err := f.SetRunState(ctx, up.ID, store.RunDeadLettered,
			store.WithReason(string(dl.args[3].(store.DeadLetterReason)))); err != nil {
			t.Fatalf("mirror dead-letter into fake meta: %v", err)
		}

		// Phase 2 -- the poison: the dependent's next consumption resolves the edge
		// against the upstream's latest (dead-lettered) run and is poisoned, and the
		// propagation plan names exactly that run as the rejection's source.
		gate := dispatch.NewGate(newFakeConsumed())
		edge := latestUpstreamEdge(t, f, "extract")
		if edge.Latest != dispatch.UpstreamDeadLettered {
			t.Fatalf("upstream latest = %v, want dead_lettered after the failover disposal", edge.Latest)
		}
		decision, err := gate.Evaluate(ctx, "load", []dispatch.Edge{edge})
		if err != nil {
			t.Fatalf("gate.Evaluate: %v", err)
		}
		if !decision.Poisoned || decision.Run {
			t.Fatalf("dependent decision = %+v, want poisoned (the failover dead-letter poisons the next consumption)", decision)
		}
		plan := dispatch.PlanPropagation(decision)
		if !plan.Propagate || plan.FailedUpstream != "extract" {
			t.Fatalf("propagation plan = %+v, want propagation from %q", plan, "extract")
		}
		if len(plan.PoisonedUpstreamRunIDs) != 1 || plan.PoisonedUpstreamRunIDs[0] != up.Seq {
			t.Errorf("poisoned upstream run ids = %v, want [%d] (the failover-stopped run)", plan.PoisonedUpstreamRunIDs, up.Seq)
		}

		// Phase 3 -- the poison holds: with nothing replayed, a later pass resolves
		// the identical poisoned decision. Nothing auto-replays (specification
		// section 2: "Nothing auto-replays"); only the explicit command unsticks.
		again, err := gate.Evaluate(ctx, "load", []dispatch.Edge{latestUpstreamEdge(t, f, "extract")})
		if err != nil {
			t.Fatalf("gate.Evaluate (later pass): %v", err)
		}
		if !again.Poisoned {
			t.Fatalf("a later pass was not poisoned (%+v); the dead-letter must poison every consumption until an explicit replay", again)
		}

		// Phase 4 -- the explicit replay: `iris deadletter replay <run>` resolves the
		// stopped run as its own root cause (stopped is a root, not a propagated
		// entry), mints a fresh replacement run on current data, and the dependent's
		// next consumption opens on the replacement's success.
		worklist := []dispatch.DeadLetterEntry{{RunID: up.Seq, Pipeline: "extract", Reason: store.ReasonStopped}}
		targets, err := dispatch.ResolveReplayTargets(worklist, []int64{up.Seq})
		if err != nil {
			t.Fatalf("ResolveReplayTargets: %v", err)
		}
		if len(targets) != 1 || targets[0] != up.Seq {
			t.Fatalf("replay targets = %v, want [%d] (a failover-stopped run is a root cause)", targets, up.Seq)
		}
		replacement, err := f.CreateRun(ctx, store.RunSpec{Pipeline: "extract", Lane: "l"})
		if err != nil {
			t.Fatalf("mint replacement run: %v", err)
		}
		if _, err := f.SetRunState(ctx, replacement.ID, store.RunRunning, store.WithHandle(9002)); err != nil {
			t.Fatalf("replacement running: %v", err)
		}
		if _, err := f.SetRunState(ctx, replacement.ID, store.RunSucceeded); err != nil {
			t.Fatalf("replacement succeeded: %v", err)
		}

		unstuck, err := gate.Evaluate(ctx, "load", []dispatch.Edge{latestUpstreamEdge(t, f, "extract")})
		if err != nil {
			t.Fatalf("gate.Evaluate (after replay): %v", err)
		}
		if !unstuck.Run || unstuck.Poisoned {
			t.Fatalf("post-replay decision = %+v, want an open gate (the explicit replay unsticks the chain)", unstuck)
		}
		if len(unstuck.Consume) != 1 || unstuck.Consume[0] != replacement.Seq {
			t.Errorf("post-replay consumption = %v, want [%d] (the replacement run)", unstuck.Consume, replacement.Seq)
		}
	})
}
