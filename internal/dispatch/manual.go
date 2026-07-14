package dispatch

import (
	"context"
	"fmt"
	"strings"

	"github.com/MateusAMP2119/iris-engine-cli/internal/store"
)

// This file is the manual pipeline-run path: iris pipeline run <name>. A manual run
// applies the depends_on gate EXACTLY like a loop pass -- the same
// Gate.Evaluate/Decide from gate.go, no manual-only relaxation -- and, when the gate
// opens, mints a run cause=manual that consumes the upstream successes it ran
// against, one run_inputs row per edge (1:1). An ineligible gate mints no run and the
// CLI exits 4 with the reason; an awaited-upstream-dead-lettered gate poisons
// (failure propagates, exit 5).
//
// Routing follows lane membership ("queued as lane's next run at current run
// boundary ... own-lane: immediate"). A lane member's run is QUEUED as its lane's next
// run so same-lane serialization holds -- the lane runner starts it in turn rather
// than the manual path starting it out of band -- while an own-lane pipeline (its own
// anonymous lane, no same-lane member to serialize against) runs immediately.
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
// resolved upstreams 1:1), the ineligibility reason when the gate did not open (for
// the CLI's exit-4 message), and the per-edge gate ledger (gate.go's EdgeVerdict
// read surface). Record is the zero RunRecord unless Disposition is ManualRunnable.
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

// EvaluateManual applies the depends_on gate to a manual run of pipeline for one
// pass, exactly like a loop pass: it resolves the gate over edges plus the
// run_inputs already-consumed check with Gate.Evaluate -- the same decision, no
// mutable cursor -- then classifies the result for the manual path. A reader error
// aborts before any classification, so a manual run never decides on a half-read
// consumed check.
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
// ledger, so exit 4 carries an actionable reason (ineligible exit 4 + reason).
// Pending edges (awaiting an upstream success) are named first, since they are the
// actionable blocker; failing that, up-to-date edges (nothing new since the dependent
// last consumed) explain the skip. An empty ledger cannot reach here (an ungated
// pipeline always runs), so it falls back to a generic reason rather than panicking.
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

// ManualRunState is the terminal outcome of a manual `iris pipeline run`, mapped by the
// CLI to an exit code. It is the single result the CLI reads to pick 0, 4, or 5.
type ManualRunState int

const (
	// ManualRunIneligible is a manual run whose depends_on gate did not open: no run was
	// minted. The CLI exits 4 with the accompanying reason.
	ManualRunIneligible ManualRunState = iota
	// ManualRunQueued is a lane-member manual run enqueued as its lane's next run at the
	// current run boundary (same-lane serialization preserved). The CLI exits 0.
	ManualRunQueued
	// ManualRunSucceeded is an own-lane manual run that ran immediately and succeeded.
	// The CLI exits 0.
	ManualRunSucceeded
	// ManualRunDeadLettered is a manual run that ran (or propagated) and dead-lettered.
	// The CLI exits 5.
	ManualRunDeadLettered
)

// String names the state, for diagnostics.
func (s ManualRunState) String() string {
	switch s {
	case ManualRunIneligible:
		return "ineligible"
	case ManualRunQueued:
		return "queued"
	case ManualRunSucceeded:
		return "succeeded"
	case ManualRunDeadLettered:
		return "dead_lettered"
	default:
		return "unknown"
	}
}

// EdgeReader resolves a pipeline's depends_on edges for the manual-run gate: its
// upstreams, each upstream's latest run disposition and id, and the awaited-from
// baseline (gate.go's Edge). The daemon's manual plane supplies the meta-backed
// implementation (dependency edges joined to each upstream's latest run); a fake
// satisfies it in tests.
type EdgeReader interface {
	// Edges returns pipeline's depends_on edges, resolved against each upstream's most
	// recent run. A pipeline with no depends_on edges returns none (ungated).
	Edges(ctx context.Context, pipeline string) ([]Edge, error)
}

// LaneReader reads the persisted lane rows the manual router decides membership from
// (lanes holds pipeline names). A meta-backed implementation and a fake both satisfy
// it.
type LaneReader interface {
	// LaneRows returns every persisted (lane, pipeline, pos) row.
	LaneRows(ctx context.Context) ([]LaneRow, error)
}

// RunQueue enqueues a manual run as a lane's next run at the current run boundary so
// same-lane serialization holds: the lane runner starts it in turn rather than the
// manual path starting it out of band. A meta-backed implementation (a queued run row
// the lane runner picks up) and a fake both satisfy it.
type RunQueue interface {
	// Enqueue records rec (cause=manual) as lane's next run to start at the current run
	// boundary.
	Enqueue(ctx context.Context, lane string, rec store.RunRecord) error
}

// ImmediateRunner mints and runs an own-lane manual run at once, returning its
// terminal disposition, since no same-lane member needs serializing (own-lane runs
// immediately). A meta+exec-backed implementation and a fake both satisfy it.
type ImmediateRunner interface {
	// RunNow mints rec (cause=manual), starts the run, and blocks until it reaches a
	// terminal state, returning that disposition.
	RunNow(ctx context.Context, rec store.RunRecord) (RunOutcome, error)
}

// ManualRunner is the manual `iris pipeline run` op: it applies the depends_on gate
// exactly like a loop pass, then routes an eligible run by lane membership -- a lane
// member is queued as its lane's next run (same-lane serial), an own-lane pipeline runs
// immediately. It holds only seams, so it is composed with a fake or the real meta+exec
// stack alike.
type ManualRunner struct {
	gate  *Gate
	edges EdgeReader
	lanes LaneReader
	queue RunQueue
	now   ImmediateRunner
}

// NewManualRunner builds a manual runner over the depends_on gate and the edge/lane read
// seams plus the queue (lane members) and immediate (own-lane) run seams.
func NewManualRunner(gate *Gate, edges EdgeReader, lanes LaneReader, queue RunQueue, now ImmediateRunner) *ManualRunner {
	return &ManualRunner{gate: gate, edges: edges, lanes: lanes, queue: queue, now: now}
}

// Run performs one manual `iris pipeline run` of pipeline and returns the terminal state
// the CLI maps to an exit code, plus an ineligibility reason when the gate did not open.
// It applies the depends_on gate exactly like a loop pass; an ineligible gate mints no
// run (exit 4 + reason) and a poisoned gate dead-letters by propagation (exit 5). An
// open gate routes by lane membership: a lane member is queued as its lane's next run at
// the current run boundary (same-lane serialization holds), an own-lane pipeline runs
// immediately. Any seam error is returned unwrapped-of-outcome so the caller never acts
// on a half-resolved run.
func (r *ManualRunner) Run(ctx context.Context, pipeline string) (ManualRunState, string, error) {
	edges, err := r.edges.Edges(ctx, pipeline)
	if err != nil {
		return ManualRunIneligible, "", fmt.Errorf("dispatch: manual run %q: resolve edges: %w", pipeline, err)
	}
	mg, err := r.gate.EvaluateManual(ctx, pipeline, edges)
	if err != nil {
		return ManualRunIneligible, "", fmt.Errorf("dispatch: manual run %q: evaluate gate: %w", pipeline, err)
	}

	switch mg.Disposition {
	case ManualIneligible:
		// The gate did not open: no run minted, the CLI exits 4 with the reason.
		return ManualRunIneligible, mg.Reason, nil
	case ManualPoisoned:
		// An awaited upstream is dead-lettered: failure propagates, so the manual run
		// dead-letters. The propagation write rides the immediate run path, which the
		// daemon composes over the real propagation seam; here the state alone is the
		// CLI's exit-5 signal.
		return ManualRunDeadLettered, "", nil
	case ManualRunnable:
		return r.route(ctx, pipeline, mg.Record)
	default:
		return ManualRunIneligible, "", fmt.Errorf("dispatch: manual run %q: unknown gate disposition %v", pipeline, mg.Disposition)
	}
}

// route dispatches an eligible manual run by lane membership: a lane member (named by a
// persisted lane row) is queued as its lane's next run so same-lane serialization holds;
// an own-lane pipeline (named by no row) runs immediately.
func (r *ManualRunner) route(ctx context.Context, pipeline string, rec store.RunRecord) (ManualRunState, string, error) {
	rows, err := r.lanes.LaneRows(ctx)
	if err != nil {
		return ManualRunIneligible, "", fmt.Errorf("dispatch: manual run %q: read lanes: %w", pipeline, err)
	}
	if lane, member := laneMembership(rows, pipeline); member {
		if err := r.queue.Enqueue(ctx, lane, rec); err != nil {
			return ManualRunIneligible, "", fmt.Errorf("dispatch: manual run %q: enqueue on lane %q: %w", pipeline, lane, err)
		}
		return ManualRunQueued, "", nil
	}

	outcome, err := r.now.RunNow(ctx, rec)
	if err != nil {
		return ManualRunIneligible, "", fmt.Errorf("dispatch: manual run %q: run: %w", pipeline, err)
	}
	if outcome == RunDeadLettered {
		return ManualRunDeadLettered, "", nil
	}
	return ManualRunSucceeded, "", nil
}

// laneMembership reports the lane a manual run of pipeline joins and whether pipeline
// is a lane member. A pipeline named by a persisted lane row is that lane's member (a
// composed lane persists rows only for two or more members, so a placed pipeline is
// genuinely serialized against a peer); a pipeline named by no row is its own
// anonymous lane, and its own lane's name is the pipeline's. It reads the lane rows
// directly, consuming lane.go's LaneRow without rewriting BuildWalk.
func laneMembership(rows []LaneRow, pipeline string) (lane string, member bool) {
	for _, row := range rows {
		if row.Pipeline == pipeline {
			return row.Lane, true
		}
	}
	return pipeline, false
}
