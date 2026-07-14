package dispatch

// This file is the pure gate and blocker predicate logic behind the destructive
// operation gates: the --yes versus --force semantics (confirmation satisfied versus
// soft-block override), the two soft-block refusals (an in-flight run on the affected
// scope; un-promoted disposable data on teardowns), and the declare-destroy
// downstream-blocker predicates (a dependent's depends_on, a downstream run_inputs
// row, an outstanding dead-letter entry naming the target as failed_upstream).
// Everything here is a decision over snapshots -- no I/O, no daemon -- mirroring
// drain.go and gate.go: the predicates decide, and a caller reads the snapshots and
// acts on them.
//
// The confirmation gate is enforced in the CLI (internal/cli: a typed target
// name on a TTY, or --yes/--force), which forwards the confirm/force flags to
// the daemon's control connection; the daemon's destructive gate
// (internal/daemon/destructivegate.go) evaluates the soft-blocks below on the
// destroy, wipe, and drain paths, and the destroyer's blocker seam consults
// DestroyBlockReasons over live meta snapshots.
//
// The tier split: hard blockers versus soft-blocks. The destroy
// downstream blockers are HARD -- destroy refuses while they hold, naming them
// (drop or drain first), and no flag overrides a dangling-reference refusal. The
// soft-blocks are operational hazards -- --yes honors them (confirms, then refuses
// with guidance) and --force overrides them, cancelling the in-flight runs it
// overrode (dead-lettered stopped, the cancel semantic run.go owns).

import (
	"context"
	"fmt"
	"sort"
	"strings"

	"github.com/MateusAMP2119/iris-engine-cli/internal/store"
)

// DestructiveOp identifies one of the gated destructive operations. Engine-managed
// role teardown rides destroy and uninstall rather than gating separately, so it
// carries no op of its own.
type DestructiveOp int

// The gated destructive operations.
const (
	// OpDeclareDestroy is `iris declare destroy <path>`: single-declaration
	// teardown, an irreversible teardown.
	OpDeclareDestroy DestructiveOp = iota
	// OpEngineUninstall is `iris engine uninstall`: full engine teardown, an
	// irreversible teardown.
	OpEngineUninstall
	// OpWorkloadWipe is `iris workload wipe [<pipeline>]`: a dev-loop op reverting
	// un-promoted disposable data.
	OpWorkloadWipe
	// OpDeadletterDrain is `iris deadletter drain`: a dev-loop op discarding
	// outstanding dead-letter entries.
	OpDeadletterDrain
)

// String names the operation as its command line reads.
func (op DestructiveOp) String() string {
	switch op {
	case OpDeclareDestroy:
		return "declare destroy"
	case OpEngineUninstall:
		return "engine uninstall"
	case OpWorkloadWipe:
		return "workload wipe"
	case OpDeadletterDrain:
		return "deadletter drain"
	default:
		return "unknown"
	}
}

// Teardown reports whether op is an irreversible teardown (engine uninstall,
// declare destroy) -- the severity tier the un-promoted-data soft-block applies
// to. The dev-loop ops (workload wipe, deadletter drain) are not teardowns:
// disposing of un-promoted data is exactly what they exist to do, so its
// existence never soft-blocks them.
func (op DestructiveOp) Teardown() bool {
	return op == OpDeclareDestroy || op == OpEngineUninstall
}

// ConfirmMode is how a non-interactive invocation satisfied the confirmation gate
// (--yes/--force on destructive commands). The interactive prompt flows (typed
// target name, y/N) belong to the CLI, which resolves either form to one of these
// modes before the decision here runs. The zero value is ConfirmYes, the safe mode:
// it never overrides a soft-block.
type ConfirmMode int

// The confirmation modes.
const (
	// ConfirmYes is --yes: the confirmation prompt is satisfied, but every
	// soft-block is honored -- a soft-blocked operation refuses with guidance.
	ConfirmYes ConfirmMode = iota
	// ConfirmForce is --force: confirmation satisfied AND soft-blocks overridden;
	// in-flight runs on the affected scope are cancelled (dead-lettered stopped).
	ConfirmForce
)

// GateScope is the affected scope of a destructive operation: one pipeline
// (declare destroy <path>, workload wipe <pipeline>, deadletter drain
// --pipeline) or, with the zero value, engine-wide (engine uninstall, bare
// workload wipe, deadletter drain --all).
type GateScope struct {
	// Pipeline is the one affected pipeline; empty means engine-wide.
	Pipeline string
}

// String names the scope for guidance text.
func (s GateScope) String() string {
	if s.Pipeline == "" {
		return "the engine"
	}
	return fmt.Sprintf("pipeline %q", s.Pipeline)
}

// covers reports whether a run on pipeline is inside the scope.
func (s GateScope) covers(pipeline string) bool {
	return s.Pipeline == "" || s.Pipeline == pipeline
}

// SoftBlockKind is the closed set of soft-blocks the confirmation gate defines:
// exactly two.
type SoftBlockKind int

// The soft-block kinds.
const (
	// SoftBlockInFlightRun is a run queued or running on the affected scope.
	SoftBlockInFlightRun SoftBlockKind = iota
	// SoftBlockUnpromotedData is un-promoted disposable data existing under a
	// teardown's scope (a non-empty wipe scope).
	SoftBlockUnpromotedData
)

// String names the soft-block kind, for diagnostics.
func (k SoftBlockKind) String() string {
	switch k {
	case SoftBlockInFlightRun:
		return "in-flight run"
	case SoftBlockUnpromotedData:
		return "un-promoted disposable data"
	default:
		return "unknown"
	}
}

// SoftBlock is one outstanding soft-block: what blocks (Detail) and the remedy
// the --yes refusal prints (Guidance). An in-flight block additionally names the
// run ids --force would cancel.
type SoftBlock struct {
	// Kind is which of the two soft-blocks this is.
	Kind SoftBlockKind
	// Runs are the in-flight run ids on the affected scope, ascending; set only
	// for SoftBlockInFlightRun. They are what --force cancels.
	Runs []string
	// Detail is the human sentence naming what blocks.
	Detail string
	// Guidance is the remedy line a --yes refusal prints.
	Guidance string
}

// InFlightRuns returns the runs queued or running on the affected scope, in
// snapshot order. A queued run counts as in flight for gating: it is admitted work
// whose start would race the destructive op, so it soft-blocks -- and is
// cancelled by --force -- exactly like a running one. Terminal runs (succeeded,
// dead-lettered) never block.
func InFlightRuns(runs []store.Run, scope GateScope) []store.Run {
	var out []store.Run
	for _, r := range runs {
		if !scope.covers(r.Pipeline) {
			continue
		}
		if r.State == store.RunQueued || r.State == store.RunRunning {
			out = append(out, r)
		}
	}
	return out
}

// EvaluateSoftBlocks evaluates the two soft-blocks for one destructive
// operation: an in-flight run on the affected scope (every op),
// and un-promoted disposable data (teardowns only). runs is the current run
// snapshot; unpromoted is the number of un-promoted disposable journal entries
// in the op's scope -- the wipe scope's size, len(pg.WipeScope(...)), which the
// wiring computes -- where any positive count blocks a teardown. The result is
// what DecideDestructive weighs against the confirmation mode.
func EvaluateSoftBlocks(op DestructiveOp, runs []store.Run, scope GateScope, unpromoted int) []SoftBlock {
	var blocks []SoftBlock
	if inflight := InFlightRuns(runs, scope); len(inflight) > 0 {
		ids := make([]string, len(inflight))
		for i, r := range inflight {
			ids[i] = r.ID
		}
		blocks = append(blocks, SoftBlock{
			Kind:   SoftBlockInFlightRun,
			Runs:   ids,
			Detail: fmt.Sprintf("run(s) %s in flight on %s", strings.Join(ids, ", "), scope),
			Guidance: fmt.Sprintf("wait for the run(s) to finish or `iris run cancel <run>` them, then retry; or re-run %q with --force to cancel them (dead-lettered stopped)",
				op),
		})
	}
	if op.Teardown() && unpromoted > 0 {
		blocks = append(blocks, SoftBlock{
			Kind:   SoftBlockUnpromotedData,
			Detail: fmt.Sprintf("%d un-promoted disposable journal entr%s under %s would be lost", unpromoted, plural(unpromoted, "y", "ies"), scope),
			Guidance: fmt.Sprintf("`iris pipeline promote` what must survive or `iris workload wipe` the rest, then retry; or re-run %q with --force to discard it",
				op),
		})
	}
	return blocks
}

// plural picks the singular or plural suffix for a count.
func plural(n int, one, many string) string {
	if n == 1 {
		return one
	}
	return many
}

// GateDecision is the outcome of the destructive-op gate for a non-interactive
// invocation: proceed or refuse, with the refusals' guidance or the override's
// cancellations.
type GateDecision struct {
	// Proceed reports whether the operation may run.
	Proceed bool
	// Refusals are the soft-blocks that stopped a --yes invocation, each carrying
	// its guidance; empty when Proceed is true.
	Refusals []SoftBlock
	// CancelRuns are the in-flight run ids a --force override must cancel
	// (dead-lettered stopped) before the operation runs; empty under --yes.
	CancelRuns []string
}

// DecideDestructive applies the --yes/--force semantics to the evaluated soft-blocks:
// --yes satisfies the confirmation prompt but honors every soft-block, refusing with
// guidance while any holds; --force overrides them all, proceeding and naming the
// in-flight runs the override cancels. With no soft-blocks both modes proceed --
// confirmation was the only gate and the mode supplied it.
func DecideDestructive(mode ConfirmMode, blocks []SoftBlock) GateDecision {
	if len(blocks) == 0 {
		return GateDecision{Proceed: true}
	}
	if mode == ConfirmForce {
		var cancels []string
		for _, b := range blocks {
			cancels = append(cancels, b.Runs...)
		}
		return GateDecision{Proceed: true, CancelRuns: cancels}
	}
	return GateDecision{Refusals: blocks}
}

// RunInputEdge is one run_inputs row as the destroy blocker reads it: the
// consumer run and the upstream run it consumed (the consumption ledger, which
// feeds the second destroy blocker).
type RunInputEdge struct {
	// RunID is the consuming run.
	RunID int64
	// InputRunID is the upstream run it consumed.
	InputRunID int64
}

// DestroyBlockReasons scans the three downstream-blocker predicates for target
// and returns one human reason per hit, each naming the
// blocker and its remedy (drop or drain first); an empty result means the target
// is destroyable. The predicates, over snapshots so the decision is pure:
//
//   - dependsOn maps each registered pipeline to its declared depends_on list; a
//     pipeline declaring the target blocks (destroy the dependent first), else its
//     gate would silently ungate.
//   - edges are the run_inputs rows and runPipeline resolves a run id to its
//     pipeline; a row whose consumer belongs to ANOTHER pipeline and whose input
//     is a target run blocks (the consumer's lineage would dangle). The target's
//     own rows never block: the destroy retires them itself.
//   - worklist is the outstanding dead-letter worklist; an entry of another
//     pipeline naming a target run as failed_upstream blocks (drain or replay it
//     first), else the worklist row would dangle. The target's own entries are
//     retired by the destroy and never block.
//
// These are hard blockers: no flag overrides them, unlike the soft-blocks
// EvaluateSoftBlocks reports.
func DestroyBlockReasons(target string, dependsOn map[string][]string, edges []RunInputEdge, worklist []DeadLetterEntry, runPipeline map[int64]string) []string {
	var reasons []string

	// Predicate 1: a registered pipeline declares depends_on on the target.
	var dependents []string
	for pipeline, ups := range dependsOn {
		if pipeline == target {
			continue
		}
		for _, up := range ups {
			if up == target {
				dependents = append(dependents, pipeline)
				break
			}
		}
	}
	sort.Strings(dependents)
	for _, dep := range dependents {
		reasons = append(reasons, fmt.Sprintf("pipeline %q declares depends_on on %q: destroy %q first", dep, target, dep))
	}

	// Predicate 2: a downstream run_inputs row names one of the target's runs.
	for _, e := range edges {
		consumer, input := runPipeline[e.RunID], runPipeline[e.InputRunID]
		if input != target || consumer == target || consumer == "" {
			continue
		}
		reasons = append(reasons, fmt.Sprintf("run %d of pipeline %q consumed run %d of %q (run_inputs): destroy %q first", e.RunID, consumer, e.InputRunID, target, consumer))
	}

	// Predicate 3: an outstanding dead-letter entry names the target as
	// failed_upstream.
	for _, entry := range worklist {
		if entry.Pipeline == target || entry.FailedUpstreamRunID == 0 {
			continue
		}
		if runPipeline[entry.FailedUpstreamRunID] != target {
			continue
		}
		reasons = append(reasons, fmt.Sprintf("dead-letter entry for run %d of pipeline %q names run %d of %q as failed_upstream: replay or drain it first", entry.RunID, entry.Pipeline, entry.FailedUpstreamRunID, target))
	}

	return reasons
}

// DestroyBlockerFunc adapts a function to the DestroyBlocker seam, so a caller can
// feed DestroyBlockReasons over live snapshots into a Destroyer without a named
// type. The daemon's leader wiring does exactly that (Candidate.destroyBlocker).
type DestroyBlockerFunc func(ctx context.Context, pipeline string) (blocked bool, reason string, err error)

// Blocked consults the adapted function.
func (f DestroyBlockerFunc) Blocked(ctx context.Context, pipeline string) (bool, string, error) {
	return f(ctx, pipeline)
}
