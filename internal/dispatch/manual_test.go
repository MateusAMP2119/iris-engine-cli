package dispatch_test

import (
	"context"
	"reflect"
	"testing"

	"github.com/MateusAMP2119/iris-engine-cli/internal/dispatch"
	"github.com/MateusAMP2119/iris-engine-cli/internal/store"
)

// TestManualRunAppliesGateLikeLoopPassAndConsumes proves the manual-run gate contract:
// a manual `iris pipeline run` applies the depends_on gate EXACTLY like a loop pass and,
// when eligible, consumes the upstream successes it ran against, one run_inputs row per
// edge (1:1). The proof is two-pronged: (1) for the same edges and already-consumed
// flags the manual decision opens exactly when the loop-pass gate (Decide) opens -- same
// mechanism, no manual-only relaxation; (2) an open manual gate mints a run record
// cause=manual whose consumed upstream ids are precisely the upstream runs the gate
// resolved (Decision.Consume), 1:1. Ineligible and poisoned gates mint no run.
//
// spec: S08/manual-run-gate-consumption
func TestManualRunAppliesGateLikeLoopPassAndConsumes(t *testing.T) {
	t.Run("S08/manual-run-gate-consumption", func(t *testing.T) {
		ctx := context.Background()

		t.Run("open gate mints cause=manual consuming the resolved upstreams 1:1", func(t *testing.T) {
			// Two upstream successes, neither yet consumed: the gate opens (a loop pass
			// would run here too), and the manual run consumes both, one run_inputs row
			// each, in edge order.
			edges := []dispatch.Edge{succeeded("extract_orders", 42), succeeded("load_customers", 7)}
			reader := newFakeConsumed() // consumed nothing yet
			gate := dispatch.NewGate(reader)

			mg, err := gate.EvaluateManual(ctx, "join_orders", edges)
			if err != nil {
				t.Fatalf("EvaluateManual: %v", err)
			}
			if mg.Disposition != dispatch.ManualRunnable {
				t.Fatalf("disposition = %v, want ManualRunnable", mg.Disposition)
			}

			// Applies the gate exactly like a loop pass: the manual decision opens
			// exactly when Decide (the loop-pass core) opens for the same inputs.
			loop := dispatch.Decide(edges, notConsumed(edges))
			if !loop.Run {
				t.Fatalf("loop-pass Decide did not open for the same edges: %+v", loop)
			}

			// cause=manual, and the consumed upstream ids are the gate-resolved ones 1:1.
			if mg.Record.Cause != store.CauseManual {
				t.Errorf("record cause = %q, want %q", mg.Record.Cause, store.CauseManual)
			}
			if mg.Record.Pipeline != "join_orders" {
				t.Errorf("record pipeline = %q, want join_orders", mg.Record.Pipeline)
			}
			if want := []int64{42, 7}; !reflect.DeepEqual(mg.Record.ConsumedUpstreamRunIDs, want) {
				t.Errorf("consumed upstream run ids = %v, want %v", mg.Record.ConsumedUpstreamRunIDs, want)
			}
			// The manual run consumes exactly what the loop-pass gate resolved.
			if !reflect.DeepEqual(mg.Record.ConsumedUpstreamRunIDs, loop.Consume) {
				t.Errorf("manual consumed %v != loop-pass Consume %v", mg.Record.ConsumedUpstreamRunIDs, loop.Consume)
			}
		})

		t.Run("ungated pipeline runs, consuming nothing", func(t *testing.T) {
			// No depends_on edges: like a loop pass, an ungated pipeline is always
			// eligible and consumes no upstream.
			gate := dispatch.NewGate(newFakeConsumed())
			mg, err := gate.EvaluateManual(ctx, "standalone", nil)
			if err != nil {
				t.Fatalf("EvaluateManual: %v", err)
			}
			if mg.Disposition != dispatch.ManualRunnable {
				t.Fatalf("ungated disposition = %v, want ManualRunnable", mg.Disposition)
			}
			if mg.Record.Cause != store.CauseManual {
				t.Errorf("record cause = %q, want manual", mg.Record.Cause)
			}
			if len(mg.Record.ConsumedUpstreamRunIDs) != 0 {
				t.Errorf("ungated run consumed %v, want nothing", mg.Record.ConsumedUpstreamRunIDs)
			}
		})

		t.Run("already-consumed latest success is ineligible with a reason, minting no run", func(t *testing.T) {
			// The upstream's latest success was already consumed: like a loop pass, the
			// gate does not open (nothing new). A manual run is ineligible and explains
			// why, and mints no run record.
			edges := []dispatch.Edge{succeeded("extract_orders", 42)}
			reader := newFakeConsumed(42) // 42 already consumed
			gate := dispatch.NewGate(reader)

			mg, err := gate.EvaluateManual(ctx, "join_orders", edges)
			if err != nil {
				t.Fatalf("EvaluateManual: %v", err)
			}
			if mg.Disposition != dispatch.ManualIneligible {
				t.Fatalf("disposition = %v, want ManualIneligible", mg.Disposition)
			}
			if mg.Reason == "" {
				t.Error("ineligible manual run carried no reason (spec: exit 4 + reason)")
			}
			if mg.Record.Cause != "" || mg.Record.Pipeline != "" {
				t.Errorf("ineligible manual run minted a record %+v, want none", mg.Record)
			}
			// The loop-pass gate agrees: it does not open either.
			if loop := dispatch.Decide(edges, []bool{true}); loop.Run {
				t.Errorf("loop-pass Decide opened for an already-consumed success: %+v", loop)
			}
		})

		t.Run("pending upstream is ineligible, minting no run", func(t *testing.T) {
			// The awaited upstream has no success yet: a loop pass would skip, so a manual
			// run is ineligible with a reason and no record.
			edges := []dispatch.Edge{{Upstream: "extract_orders", Latest: dispatch.UpstreamPending, LatestRunID: 9}}
			gate := dispatch.NewGate(newFakeConsumed())
			mg, err := gate.EvaluateManual(ctx, "join_orders", edges)
			if err != nil {
				t.Fatalf("EvaluateManual: %v", err)
			}
			if mg.Disposition != dispatch.ManualIneligible {
				t.Fatalf("disposition = %v, want ManualIneligible", mg.Disposition)
			}
			if mg.Reason == "" {
				t.Error("pending-upstream ineligible run carried no reason")
			}
		})

		t.Run("awaited dead-lettered upstream poisons the manual run", func(t *testing.T) {
			// An awaited upstream run is dead-lettered: failure propagates, so a loop pass
			// would write a propagated dead-lettered run. The manual gate classifies this
			// as poisoned (the CLI dead-letters, exit 5), minting no ordinary run record.
			edges := []dispatch.Edge{{Upstream: "extract_orders", Latest: dispatch.UpstreamDeadLettered, LatestRunID: 42}}
			gate := dispatch.NewGate(newFakeConsumed())
			mg, err := gate.EvaluateManual(ctx, "join_orders", edges)
			if err != nil {
				t.Fatalf("EvaluateManual: %v", err)
			}
			if mg.Disposition != dispatch.ManualPoisoned {
				t.Fatalf("disposition = %v, want ManualPoisoned", mg.Disposition)
			}
			if mg.Record.Cause != "" {
				t.Errorf("poisoned manual run minted a record %+v, want none", mg.Record)
			}
		})
	})
}
