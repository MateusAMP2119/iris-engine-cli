package dispatch

import (
	"context"
	"fmt"
)

// This file is the depends_on eligibility gate and its consumption decision
// (specification sections 1, 3, 6.1 and 6.2). depends_on is a data gate, not an
// order: it makes a downstream pipeline eligible only on an upstream's output and
// never sequences the two -- ordering is the composer's job alone (section 6.1). The
// gate here is a pure decision at the dependent's composer-assigned turn: for each
// depends_on edge it reads the upstream's most recent run and the run_inputs
// already-consumed check, produces the per-edge verdict from a closed set, and
// composes the pass decision (run, consuming one upstream run per edge, or skip,
// minting no run row). It reads only the upstream's LATEST run -- older successes are
// superseded, not queued -- holds no cursor or watermark of its own, and carries no
// walk-position or lane field, so it can neither reorder the walk nor turn a
// cross-lane reference into a serial ordering.
//
// The consumption write itself is not here: on an open gate the dispatcher feeds the
// decision's Consume list to store.Writer.CreateRun, which records one run_inputs row
// per consumed upstream in the same atomic run-create transaction, written once at
// run start and never mutated (specification section 4). This file owns the decision
// and the 1:1 recording rule; CreateRun owns the write.

// UpstreamState is the disposition of an upstream's most recent run -- the only run
// the gate reads (specification section 6.2: "the gate reads only the upstream's
// latest run"). Older runs are superseded, never buffered, so this single latest
// disposition, not a backlog, is all an edge carries.
type UpstreamState int

// The upstream-latest-run dispositions.
const (
	// UpstreamNone is an upstream that has produced no run yet.
	UpstreamNone UpstreamState = iota
	// UpstreamPending is an upstream whose most recent run is queued or running.
	UpstreamPending
	// UpstreamSucceeded is an upstream whose most recent run succeeded.
	UpstreamSucceeded
	// UpstreamDeadLettered is an upstream whose most recent run is dead-lettered.
	UpstreamDeadLettered
)

// String names the upstream state, for diagnostics.
func (s UpstreamState) String() string {
	switch s {
	case UpstreamNone:
		return "none"
	case UpstreamPending:
		return "pending"
	case UpstreamSucceeded:
		return "succeeded"
	case UpstreamDeadLettered:
		return "dead_lettered"
	default:
		return "unknown"
	}
}

// Verdict is one edge's gate disposition: the closed set the gate ledger renders in
// iris pipeline show and scripts read through --json (specification section 6.2:
// open, up_to_date, pending, poisoned). The set is closed: a value outside it is a
// bug, never a default.
type Verdict int

// The gate verdicts (the closed set of specification section 6.2).
const (
	// VerdictPending is an edge awaiting an upstream success: the upstream has no run
	// yet, its most recent run is still in flight, or -- for a newly added edge -- its
	// most recent success predates the edge and is history the edge never awaited, so
	// the edge awaits the upstream's next success (specification section 6.2: "new
	// edges await the next success from pass one, never history"). It is the zero
	// value: an unresolved edge defaults to awaiting.
	VerdictPending Verdict = iota
	// VerdictOpen is an edge whose upstream's most recent run is an awaited success the
	// dependent has not yet consumed: the gate opens and the dependent records exactly
	// that run in run_inputs, 1:1.
	VerdictOpen
	// VerdictUpToDate is an edge whose awaited latest success the dependent has already
	// consumed: nothing new since it last consumed, so this edge mints no run on its
	// own.
	VerdictUpToDate
	// VerdictPoisoned is an edge whose awaited upstream run is dead-lettered: the
	// rejection propagates and the dispatcher writes the dependent a dead-lettered run
	// that pass (the propagation write belongs to the dead-letter path, not the gate).
	VerdictPoisoned
)

// String renders the verdict as the ledger/--json token (specification section 6.2).
func (v Verdict) String() string {
	switch v {
	case VerdictPending:
		return "pending"
	case VerdictOpen:
		return "open"
	case VerdictUpToDate:
		return "up_to_date"
	case VerdictPoisoned:
		return "poisoned"
	default:
		return "unknown"
	}
}

// Edge is one depends_on edge (dependent B -> upstream A) as the gate resolves it at
// the dependent's turn. It carries the upstream's name, the disposition and id of the
// upstream's most recent run -- the single latest run, never a backlog slice, so the
// gate cannot buffer or head-of-line-block -- and the awaited-from baseline that
// separates awaited runs from the history a newly added edge never consumes.
//
// An Edge has no lane, order, or walk-position field: the gate decision is a function
// of upstream output alone, so a cross-lane edge gates data without imposing any
// serial ordering between the lanes (specification sections 3 and 6.1).
type Edge struct {
	// Upstream is the upstream pipeline's name (the depends_on target A).
	Upstream string
	// Latest is the disposition of the upstream's most recent run.
	Latest UpstreamState
	// LatestRunID is the id of the upstream's most recent run (0 when Latest is
	// UpstreamNone). It is the run the gate consumes 1:1 when the gate opens.
	LatestRunID int64
	// AwaitedFrom is the greatest upstream run id this edge does NOT await: the
	// upstream's run tip at the moment the edge was established. A run with id greater
	// than AwaitedFrom was minted while the edge existed and is awaited; a run with id
	// at or below it predates the edge and is history the edge never awaited
	// (specification section 6.2: "new edges await the next success from pass one,
	// never history"). It is zero for an edge established before the upstream produced
	// any run -- then every run is awaited. It is a per-pass, per-edge input set once
	// at edge establishment and never advanced, not a mutable consumer cursor: what
	// advances is the run_inputs already-consumed check, derived on read, not stored.
	AwaitedFrom int64
}

// awaited reports whether the upstream's most recent run was minted while this edge
// existed (id strictly past the edge's establishment baseline).
func (e Edge) awaited() bool { return e.LatestRunID > e.AwaitedFrom }

// EdgeVerdict is one row of the gate ledger: the upstream, its resolved verdict, and
// the id of the upstream's most recent run the verdict was computed against
// (specification section 6.2 read surface -- per edge, the upstream's latest run, the
// already-consumed check, and a verdict).
type EdgeVerdict struct {
	// Upstream is the upstream pipeline's name.
	Upstream string
	// Verdict is the edge's resolved gate disposition.
	Verdict Verdict
	// LatestRunID is the upstream's most recent run id the verdict resolved against.
	LatestRunID int64
}

// Decision is the dependent's gate outcome for one composer pass: whether it runs and,
// when it does, the upstream runs it consumes -- one per edge, fed to CreateRun and
// recorded 1:1 in run_inputs at run start. A skip mints no run row: absence is the
// record, explained by the per-edge Ledger (specification section 6.2).
//
// Decision deliberately carries no walk-position, order, or lane field. The gate
// decides run-or-skip at the pipeline's composer-assigned turn and returns only that:
// it never reorders the walk and never turns a cross-lane edge into serial ordering
// (specification sections 6.1 and 3).
type Decision struct {
	// Run reports whether the dependent runs this pass.
	Run bool
	// Consume is the upstream run ids the run consumes, one per edge in edge order,
	// each recorded as one run_inputs row at run start. It is non-nil only when Run.
	Consume []int64
	// Poisoned reports that an awaited upstream run is dead-lettered, so the dependent
	// is dead-lettered this pass by failure propagation rather than run or skip. The
	// propagation write itself is the dead-letter path's, not the gate's.
	Poisoned bool
	// Ledger is the per-edge verdict list, in edge order: the gate's read surface.
	Ledger []EdgeVerdict
}

// ConsumedReader answers the gate's already-consumed check against run_inputs
// (specification sections 4 and 6.2): has the dependent already consumed a given
// upstream run? It is a run_inputs lookup, never a mutable cursor -- the gate holds no
// watermark state, and this read is where "has the latest success been consumed" is
// resolved on each pass. A meta-backed implementation and a fake both satisfy it.
type ConsumedReader interface {
	// Consumed reports whether dependent has a run_inputs row recording upstreamRunID
	// as one of its consumed upstream runs.
	Consumed(ctx context.Context, dependent string, upstreamRunID int64) (bool, error)
}

// Gate resolves a dependent's depends_on gate by pairing the pure decision with the
// run_inputs already-consumed check. It holds only the ConsumedReader seam -- no
// cursor, watermark, or per-consumer position of its own -- so the sole state the
// consumed check consults is run_inputs (specification section 6.2: no mutable
// cursor).
type Gate struct {
	reader ConsumedReader
}

// NewGate builds a gate over the run_inputs already-consumed read seam.
func NewGate(reader ConsumedReader) *Gate { return &Gate{reader: reader} }

// Evaluate resolves dependent's depends_on gate for one pass: for each edge whose
// upstream's most recent run is a success it queries run_inputs (never a stored
// cursor) for whether the dependent already consumed that run, then composes the pass
// decision with Decide. A reader error aborts before any decision, so the gate never
// decides on a half-read consumed check.
func (g *Gate) Evaluate(ctx context.Context, dependent string, edges []Edge) (Decision, error) {
	// The already-consumed check matters only for an upstream whose most recent run is
	// a success; for any other disposition the flag is unused. Query run_inputs for
	// exactly those edges -- never a stored cursor -- and abort on a read failure so
	// the gate never decides on a half-read check.
	consumed := make([]bool, len(edges))
	for i, e := range edges {
		if e.Latest != UpstreamSucceeded {
			continue
		}
		ok, err := g.reader.Consumed(ctx, dependent, e.LatestRunID)
		if err != nil {
			return Decision{}, fmt.Errorf("dispatch: gate consumed check for %s upstream run %d: %w", dependent, e.LatestRunID, err)
		}
		consumed[i] = ok
	}
	return Decide(edges, consumed), nil
}

// evaluateEdge resolves one edge's verdict from the closed set, given whether the
// dependent has already consumed the upstream's most recent run. It reads only that
// most recent run: an upstream with no run yet or one still in flight is pending; a
// most recent run that predates the edge (not awaited) is history, so the edge is
// pending on the upstream's next run; an awaited dead-lettered run poisons; and an
// awaited success is open when unconsumed, up_to_date once consumed.
func evaluateEdge(e Edge, consumed bool) Verdict {
	switch e.Latest {
	case UpstreamNone, UpstreamPending:
		return VerdictPending
	case UpstreamDeadLettered:
		if e.awaited() {
			return VerdictPoisoned
		}
		// A dead-letter that predates the edge is history: await the next run.
		return VerdictPending
	case UpstreamSucceeded:
		if !e.awaited() {
			// A success that predates the edge is history: await the next success.
			return VerdictPending
		}
		if consumed {
			return VerdictUpToDate
		}
		return VerdictOpen
	default:
		return VerdictPending
	}
}

// Decide is the pure gate core: it resolves each edge's verdict against the upstream's
// most recent run and the already-consumed flags, then composes the pass decision. It
// is pure -- no I/O -- and a function of its inputs alone, so the same edges yield the
// same decision at any turn, in any lane; it carries no walk position, so it can
// neither reorder the walk nor sequence lanes (specification sections 6.1, 3, 6.2).
//
// A pipeline with no depends_on edges is ungated: it runs every pass on composer order
// alone. Otherwise the dependent runs only when every edge resolves to an available
// success (no edge pending) and at least one edge is open (something new to consume);
// it then consumes one upstream run per edge, in edge order, recorded 1:1 in
// run_inputs. Any awaited dead-lettered upstream makes the decision poisoned (failure
// propagates that pass) rather than run or skip; consumed[i] is treated as false when
// absent, never panicking.
func Decide(edges []Edge, consumed []bool) Decision {
	// No depends_on edges: ungated, always eligible.
	if len(edges) == 0 {
		return Decision{Run: true}
	}

	ledger := make([]EdgeVerdict, len(edges))
	var anyPoisoned, anyPending, anyOpen bool
	for i, e := range edges {
		c := i < len(consumed) && consumed[i]
		v := evaluateEdge(e, c)
		ledger[i] = EdgeVerdict{Upstream: e.Upstream, Verdict: v, LatestRunID: e.LatestRunID}
		switch v {
		case VerdictPoisoned:
			anyPoisoned = true
		case VerdictPending:
			anyPending = true
		case VerdictOpen:
			anyOpen = true
		}
	}

	d := Decision{Ledger: ledger}
	switch {
	case anyPoisoned:
		// An awaited upstream is dead-lettered: the rejection propagates this pass.
		d.Poisoned = true
	case anyPending:
		// Some edge has not resolved to an available success: the dependent waits, no
		// run row this pass (absence is the record).
	case anyOpen:
		// Every edge resolved (all open or up_to_date) and at least one is new: the
		// dependent runs, consuming each edge's most recent success, one row per edge.
		d.Run = true
		d.Consume = make([]int64, len(edges))
		for i, e := range edges {
			d.Consume[i] = e.LatestRunID
		}
	default:
		// Every edge is up_to_date: nothing new since the dependent last consumed, so
		// no run row this pass.
	}
	return d
}
