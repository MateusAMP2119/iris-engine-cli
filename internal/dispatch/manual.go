package dispatch

import (
	"context"
	"strings"

	"github.com/MateusAMP2119/iris-engine-cli/internal/store"
)

// This file is the manual pipeline-run path: iris pipeline run <name> (specification
// section 8). A manual run applies the depends_on gate EXACTLY like a loop pass -- the
// same Gate.Evaluate/Decide (E05.5), no manual-only relaxation -- and, when the gate
// opens, mints a run cause=manual that consumes the upstream successes it ran against,
// one run_inputs row per edge (1:1). An ineligible gate mints no run and the CLI exits 4
// with the reason; an awaited-upstream-dead-lettered gate poisons (failure propagates,
// exit 5).
//
// Routing follows lane membership (specification section 8: "queued as lane's next run
// at current run boundary ... own-lane: immediate"). A lane member's run is QUEUED as
// its lane's next run so same-lane serialization holds -- the lane runner starts it in
// turn rather than the manual path starting it out of band -- while an own-lane pipeline
// (its own anonymous lane, no same-lane member to serialize against) runs immediately.
//
// The gate decision and record shape (classifyManual) are pure and unit-tested; the
// routing over the queue/immediate seams is integration-tested with fakes. The daemon
// composes the real seams onto this op.

// ManualDisposition is how the depends_on gate classifies a manual run for one pass. It
// mirrors the loop-pass gate decision (Decide) but names the manual-path outcomes the
// caller acts on: run now (or queue), report ineligible, or propagate a poisoned edge.
type ManualDisposition int

const (
	// ManualIneligible is a manual run whose gate did not open: some edge is pending
	// (the upstream has produced no awaited success yet) or every edge is up to date
	// (nothing new to consume). No run is minted; the CLI exits 4 with the reason.
	ManualIneligible ManualDisposition = iota
	// ManualRunnable is a manual run whose gate is open, or a pipeline with no
	// depends_on edges (ungated, always eligible). A run is minted cause=manual,
	// consuming the upstream runs the gate resolved 1:1.
	ManualRunnable
	// ManualPoisoned is a manual run one of whose awaited upstream runs is dead-lettered:
	// failure propagates along the depends_on edge, so the manual run dead-letters by
	// propagation rather than executing (the CLI exits 5).
	ManualPoisoned
)

// String names the disposition, for diagnostics and logs.
func (d ManualDisposition) String() string {
	switch d {
	case ManualIneligible:
		return "ineligible"
	case ManualRunnable:
		return "runnable"
	case ManualPoisoned:
		return "poisoned"
	default:
		return "unknown"
	}
}

// ManualGate is the result of applying the depends_on gate to a manual run: its
// disposition, the run record to mint when runnable (cause=manual, consuming the
// resolved upstreams 1:1), the ineligibility reason when the gate did not open (for the
// CLI's exit-4 message), and the per-edge gate ledger (the E05.5 read surface). Record
// is the zero RunRecord unless Disposition is ManualRunnable.
type ManualGate struct {
	// Disposition is the classified gate outcome.
	Disposition ManualDisposition
	// Record is the run to mint when Disposition is ManualRunnable; zero otherwise.
	Record store.RunRecord
	// Reason explains ineligibility when Disposition is ManualIneligible; empty otherwise.
	Reason string
	// Ledger is the per-edge gate ledger, in edge order.
	Ledger []EdgeVerdict
}

// EvaluateManual applies the depends_on gate to a manual run of pipeline for one pass,
// exactly like a loop pass: it resolves the gate over edges plus the run_inputs
// already-consumed check with Gate.Evaluate -- the same decision, no mutable cursor
// (E05.5) -- then classifies the result for the manual path. A reader error aborts
// before any classification, so a manual run never decides on a half-read consumed
// check.
func (g *Gate) EvaluateManual(ctx context.Context, pipeline string, edges []Edge) (ManualGate, error) {
	d, err := g.Evaluate(ctx, pipeline, edges)
	if err != nil {
		return ManualGate{}, err
	}
	return classifyManual(pipeline, d), nil
}

// classifyManual turns a loop-pass gate Decision into the manual-run outcome for
// pipeline. It is pure and total over the decision's closed states: a poisoned decision
// propagates, an open (Run) decision mints a run cause=manual consuming exactly the
// upstreams the gate resolved (Decision.Consume, 1:1), and anything else is ineligible
// with a reason drawn from the ledger. It reads the decision alone, so the manual gate
// is identical to the loop-pass gate -- only the cause and the routing differ.
func classifyManual(pipeline string, d Decision) ManualGate {
	switch {
	case d.Poisoned:
		return ManualGate{Disposition: ManualPoisoned, Ledger: d.Ledger}
	case d.Run:
		return ManualGate{
			Disposition: ManualRunnable,
			Record: store.RunRecord{
				Pipeline:               pipeline,
				Cause:                  store.CauseManual,
				ConsumedUpstreamRunIDs: d.Consume,
			},
			Ledger: d.Ledger,
		}
	default:
		return ManualGate{
			Disposition: ManualIneligible,
			Reason:      ineligibilityReason(d.Ledger),
			Ledger:      d.Ledger,
		}
	}
}

// ineligibilityReason renders why a manual run's gate did not open, from the gate
// ledger, so exit 4 carries an actionable reason (specification section 8: ineligible
// exit 4 + reason). Pending edges (awaiting an upstream success) are named first, since
// they are the actionable blocker; failing that, up-to-date edges (nothing new since the
// dependent last consumed) explain the skip. An empty ledger cannot reach here (an
// ungated pipeline always runs), so it falls back to a generic reason rather than
// panicking.
func ineligibilityReason(ledger []EdgeVerdict) string {
	var pending, upToDate []string
	for _, ev := range ledger {
		switch ev.Verdict {
		case VerdictPending:
			pending = append(pending, ev.Upstream)
		case VerdictUpToDate:
			upToDate = append(upToDate, ev.Upstream)
		}
	}
	switch {
	case len(pending) > 0:
		return "depends_on gate not satisfied: awaiting a success from " + strings.Join(pending, ", ")
	case len(upToDate) > 0:
		return "depends_on gate up to date: nothing new to consume from " + strings.Join(upToDate, ", ")
	default:
		return "depends_on gate not satisfied"
	}
}
