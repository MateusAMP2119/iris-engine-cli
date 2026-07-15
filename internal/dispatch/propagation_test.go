package dispatch_test

import (
	"reflect"
	"testing"

	"github.com/MateusAMP2119/iris-lakehouse/internal/dispatch"
	"github.com/MateusAMP2119/iris-lakehouse/internal/store"
)

// deadLettered builds an edge whose upstream's most recent run is an awaited
// dead-lettered run (AwaitedFrom left at zero, so the run is awaited): the
// disposition the gate resolves to poisoned and propagation consumes.
func deadLettered(upstream string, runID int64) dispatch.Edge {
	return dispatch.Edge{Upstream: upstream, Latest: dispatch.UpstreamDeadLettered, LatestRunID: runID}
}

// TestPropagationThroughDependsOn proves an upstream failure propagates through a
// depends_on edge to the downstream: B's gate resolves poisoned on an awaited
// dead-lettered upstream run, and the plan derived from that decision propagates the
// rejection to B, naming the depends_on upstream and recording its dead-lettered run.
// The dead-lettering this drives is the upstream_dead_lettered reason on the single
// non-success terminal state.
func TestPropagationThroughDependsOn(t *testing.T) {
	// B depends_on extract_orders; extract_orders' most recent run (7) is
	// dead-lettered and awaited.
	edges := []dispatch.Edge{deadLettered("extract_orders", 7)}
	d := dispatch.Decide(edges, notConsumed(edges))
	if !d.Poisoned {
		t.Fatalf("awaited dead-lettered upstream did not poison the gate: %+v", d)
	}

	plan := dispatch.PlanPropagation(d)
	if !plan.Propagate {
		t.Fatalf("a poisoned gate decision produced no propagation plan: %+v", plan)
	}
	if plan.FailedUpstream != "extract_orders" {
		t.Errorf("failed upstream = %q, want the depends_on upstream extract_orders", plan.FailedUpstream)
	}
	if want := []int64{7}; !reflect.DeepEqual(plan.PoisonedUpstreamRunIDs, want) {
		t.Errorf("poisoned upstream runs = %v, want the awaited dead-lettered run %v", plan.PoisonedUpstreamRunIDs, want)
	}

	// The propagation dead-letters the downstream as upstream_dead_lettered: the
	// non-success ending folds onto the single dead_lettered terminal state.
	state, reason, err := store.ClassifyEnding(store.EndingUpstreamDeadLettered)
	if err != nil {
		t.Fatalf("ClassifyEnding(upstream_dead_lettered): %v", err)
	}
	if state != store.RunDeadLettered {
		t.Errorf("propagated ending state = %q, want dead_lettered", state)
	}
	if reason != store.ReasonUpstreamDeadLettered {
		t.Errorf("propagated ending reason = %q, want upstream_dead_lettered", reason)
	}
}

// TestPropagationDependsEdgesOnly proves failure propagates ONLY along depends_on
// edges, dispatcher-computed lazily at the dependent's consumption time (a rejected
// promise), never eagerly from the upstream's failure. With A dead-lettered: B, which
// declares depends_on A, is propagated to; C, composer-ordered after A but with no
// depends_on edge, and D, wholly independent, both carry no gate edge and are
// untouched (composer order is not a gate input, so it can carry no propagation).
func TestPropagationDependsEdgesOnly(t *testing.T) {
	// B depends_on A; A's most recent run (7) is dead-lettered and awaited: the gate
	// poisons and the rejection propagates along the depends_on edge.
	bEdges := []dispatch.Edge{deadLettered("A", 7)}
	bPlan := dispatch.PlanPropagation(dispatch.Decide(bEdges, notConsumed(bEdges)))
	if !bPlan.Propagate || bPlan.FailedUpstream != "A" {
		t.Fatalf("depends_on downstream B was not propagated to: %+v", bPlan)
	}

	// C is composer-ordered after A but does NOT depends_on it. Composer order is not
	// a gate input, so C carries no depends_on edge: its gate is ungated (runs on
	// composer order alone), never poisoned, so nothing propagates to it.
	cPlan := dispatch.PlanPropagation(dispatch.Decide(nil, nil))
	if cPlan.Propagate {
		t.Errorf("composer-ordered pipeline C was propagated to despite no depends_on edge: %+v", cPlan)
	}

	// D is wholly independent: no edge, no order. Identically untouched.
	dPlan := dispatch.PlanPropagation(dispatch.Decide(nil, nil))
	if dPlan.Propagate {
		t.Errorf("independent pipeline D was propagated to: %+v", dPlan)
	}

	// Lazy (a rejected promise): propagation is derived from the dependent's OWN gate
	// decision at its turn, not eagerly from A's failure. A dead-lettered upstream run
	// that predates the edge is history (pending, not awaited): the gate is not
	// poisoned, so no propagation is computed until the dependent actually awaits and
	// consumes the rejection.
	historical := []dispatch.Edge{{Upstream: "A", Latest: dispatch.UpstreamDeadLettered, LatestRunID: 7, AwaitedFrom: 7}}
	lazy := dispatch.Decide(historical, notConsumed(historical))
	if lazy.Poisoned {
		t.Fatalf("a pre-edge (historical) dead-letter poisoned the gate: %+v", lazy)
	}
	if dispatch.PlanPropagation(lazy).Propagate {
		t.Error("propagation was computed for a not-yet-awaited rejection; it must be lazy at consumption time")
	}
}

// TestTransitivePropagationImmediate proves propagation is transitive edge by edge and
// records the IMMEDIATE upstream, not the root: C awaits B's dead-lettered run (B was
// itself dead-lettered by propagation from A), so C's gate poisons on the B edge and
// the plan names B, the immediate upstream, recording B's run. The gate only ever sees
// the immediate upstream, so the root A never enters C's attribution.
func TestTransitivePropagationImmediate(t *testing.T) {
	// A -> B -> C. B's most recent run (20) is dead-lettered (it was itself a
	// propagated rejection from A). C depends_on B and awaits run 20.
	cEdges := []dispatch.Edge{deadLettered("B", 20)}
	plan := dispatch.PlanPropagation(dispatch.Decide(cEdges, notConsumed(cEdges)))
	if !plan.Propagate {
		t.Fatalf("C awaiting B's dead-lettered run was not propagated to: %+v", plan)
	}
	if plan.FailedUpstream != "B" {
		t.Errorf("failed upstream = %q, want the immediate upstream B", plan.FailedUpstream)
	}
	if plan.FailedUpstream == "A" {
		t.Error("propagation recorded the root A instead of the immediate upstream B")
	}
	if want := []int64{20}; !reflect.DeepEqual(plan.PoisonedUpstreamRunIDs, want) {
		t.Errorf("poisoned upstream run = %v, want B's dead-lettered run %v (recorded in run_inputs)", plan.PoisonedUpstreamRunIDs, want)
	}
}
