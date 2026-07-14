package dispatch

// This file is failure propagation along depends_on edges: the pure step that turns
// the gate's poisoned verdict into a write plan the dispatcher hands to the single
// writer. Propagation flows ONLY along depends_on edges and is computed lazily at the
// dependent's consumption time -- a rejected promise. When an upstream fails, nothing
// is written eagerly; the rejection materializes only when the dependent's gate
// evaluates the edge at its own turn and resolves it poisoned (gate.go). This file
// consumes that poisoned decision; it never reads or writes meta itself.
//
// The gate (Decide) produces the poisoned verdict: for an awaited dead-lettered
// upstream run it flags Decision.Poisoned and records the poisoned edge in the
// per-edge Ledger. PlanPropagation reads that ledger and yields the propagation plan:
// which upstream is the immediate failed_upstream and which dead-lettered upstream
// run(s) the dependent records in run_inputs. The write itself -- a never-executed
// dead-lettered run (cause=propagated) plus its dead_letters and run_inputs rows --
// is store.Writer.DeadLetterPropagated; this file owns only the plan.
//
// Propagation is transitive edge by edge, and attribution is always the IMMEDIATE
// upstream, never the root: the gate resolves a dependent against its OWN depends_on
// edges alone, so a dependent C awaiting a dead-lettered upstream B (whose run was
// itself a propagated rejection from A) sees only B. C's plan names B and records B's
// run; the root A never enters C's edges, so it can never be attributed. The chain
// walks one edge per pass, each dependent poisoned by its own immediate upstream.

// PropagationPlan is the write plan for propagating an upstream failure to a
// dependent whose gate decision is poisoned: what store.Writer.DeadLetterPropagated
// stamps onto the dependent's never-executed dead-lettered run. It is derived purely
// from the gate decision (PlanPropagation), so it carries no pipeline identity, lane,
// or walk position of its own -- the dispatcher pairs it with the dependent's name and
// declaration checksum at write time.
type PropagationPlan struct {
	// Propagate reports that the dependent is dead-lettered this pass by failure
	// propagation: its gate decision is poisoned and at least one poisoned edge names
	// the upstream run that rejected it. When false, the other fields are zero and
	// there is nothing to write.
	Propagate bool
	// FailedUpstream is the immediate upstream pipeline whose dead-lettered run
	// propagated: the dead_letters.failed_upstream the dependent records. It is the
	// immediate upstream the gate resolved against, never the root of a transitive
	// chain (the gate never sees past the immediate edge). When several edges are
	// poisoned in one pass it is the first in edge order.
	FailedUpstream string
	// PoisonedUpstreamRunIDs are the dead-lettered upstream run ids the dependent
	// records in run_inputs, one per poisoned edge in edge order (complete lineage): the
	// run(s) the rejection propagated from, so the reverse run_inputs walk resolves the
	// dependent's dead-letter back to the upstream run that caused it.
	PoisonedUpstreamRunIDs []int64
}

// PlanPropagation derives the propagation plan from a dependent's gate decision. It
// composes the gate's poisoned verdict (Decide in gate.go): only a poisoned decision
// propagates, and the plan is read from the per-edge Ledger -- the immediate upstream
// is the first poisoned edge, and every poisoned edge's dead-lettered run is recorded
// in run_inputs for complete lineage. It is pure -- a function of the decision alone,
// no I/O -- so propagation is computed lazily at the dependent's consumption time from
// its own gate decision, never eagerly from the upstream's failure (a rejected
// promise). A non-poisoned decision, or a poisoned flag with no poisoned edge, yields
// an empty plan (Propagate false): there is nothing to propagate.
func PlanPropagation(d Decision) PropagationPlan {
	if !d.Poisoned {
		return PropagationPlan{}
	}
	var plan PropagationPlan
	for _, ev := range d.Ledger {
		if ev.Verdict != VerdictPoisoned {
			continue
		}
		if plan.FailedUpstream == "" {
			plan.FailedUpstream = ev.Upstream
		}
		plan.PoisonedUpstreamRunIDs = append(plan.PoisonedUpstreamRunIDs, ev.LatestRunID)
	}
	// Propagate only when a poisoned edge actually names an upstream run: a poisoned
	// flag with no poisoned edge (an inconsistent decision) writes nothing rather than
	// a run with no failed_upstream and no lineage.
	plan.Propagate = len(plan.PoisonedUpstreamRunIDs) > 0
	return plan
}
