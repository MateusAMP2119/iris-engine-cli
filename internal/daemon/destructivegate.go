package daemon

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/MateusAMP2119/iris-lakehouse/internal/dispatch"
	"github.com/MateusAMP2119/iris-lakehouse/internal/store"
)

// This file is the daemon-side destructive-op gate: the wiring that makes the
// pure --yes/--force semantics (dispatch.EvaluateSoftBlocks + DecideDestructive)
// bite on the leader's destroy, wipe, and drain paths. The CLI enforces the
// confirmation surface (typed name, y/N, --yes/--force); this layer enforces the
// second tier -- the soft-blocks that stop a CONFIRMED op the operator did not
// realise was unsafe. --yes honors every soft-block (refuses with guidance);
// --force overrides them, cancelling the in-flight runs it overrode.

// destructiveGate evaluates the soft-block gate for one destructive operation
// over a live run snapshot, and carries out a --force override's cancellations:
// a running run's process group is killed through the shared in-flight registry
// and the run is dead-lettered stopped; a queued never-started run is deleted
// (queued runs consumed nothing -- the same disposal crash reconciliation uses).
// A nil reader disables the gate (the shape-test compositions that wire no run
// reader), mirroring how a nil reader skips startup reconciliation.
type destructiveGate struct {
	reader   store.Reader
	inflight *inflightRuns // nil: no process kill (queued-only cancellation still applies)
	submit   dispatch.Submitter
}

// enforce evaluates the soft-blocks for op on scope and applies the --yes/--force
// decision: a refusal returns an error naming each block and its remedy; a --force
// override cancels the in-flight runs it overrode before returning nil. unpromoted
// is the count of un-promoted disposable journal entries under the scope
// (teardowns only; pass 0 for the dev-loop ops, whose evaluation ignores it).
func (g destructiveGate) enforce(ctx context.Context, op dispatch.DestructiveOp, scope dispatch.GateScope, force bool, unpromoted int) error {
	if g.reader == nil || g.submit == nil {
		return nil // gate not wired (shape-test composition)
	}
	runs, err := g.reader.Runs(ctx, store.RunFilter{})
	if err != nil {
		return fmt.Errorf("%s: read runs for the destructive gate: %w", op, err)
	}
	blocks := dispatch.EvaluateSoftBlocks(op, runs, scope, unpromoted)
	mode := dispatch.ConfirmYes
	if force {
		mode = dispatch.ConfirmForce
	}
	decision := dispatch.DecideDestructive(mode, blocks)
	if !decision.Proceed {
		return refusalError(op, decision.Refusals)
	}
	for _, id := range decision.CancelRuns {
		if err := g.cancelRun(ctx, op, id); err != nil {
			return err
		}
	}
	return nil
}

// cancelRun carries out one --force cancellation: the process group dies first
// (running runs are tracked in the shared in-flight registry; a queued run has no
// process), then the run record is disposed of through the single writer -- the
// dead-letter is guarded on the running state and the delete on the queued state,
// so exactly one takes effect and a run that reached terminal in the meantime is
// left alone.
func (g destructiveGate) cancelRun(ctx context.Context, op dispatch.DestructiveOp, id string) error {
	if g.inflight != nil {
		g.inflight.kill(id)
	}
	if err := g.submit.Submit(ctx, func(w *store.Writer) error {
		if err := w.DeadLetterRun(ctx, id, store.ReasonStopped, fmt.Sprintf("run cancelled by --force %s", op)); err != nil {
			return err
		}
		return w.DeleteQueuedRun(ctx, id)
	}); err != nil {
		return fmt.Errorf("%s: cancel in-flight run %s: %w", op, id, err)
	}
	return nil
}

// refusalError renders a --yes refusal: one line per soft-block naming what
// blocks and the remedy, so the CLI prints actionable guidance.
func refusalError(op dispatch.DestructiveOp, refusals []dispatch.SoftBlock) error {
	var b strings.Builder
	fmt.Fprintf(&b, "%s refused:", op)
	for _, r := range refusals {
		fmt.Fprintf(&b, "\n  - %s\n    %s", r.Detail, r.Guidance)
	}
	return errors.New(b.String())
}

// destroyBlocker builds the destroyer's HARD blocker over the candidate's live
// meta readers: dispatch.DestroyBlockReasons over the declared depends_on edges,
// the run_inputs consumption ledger (with pruned upstreams resolved through their
// archival summaries), and the outstanding dead-letter worklist. A destroy
// refuses while any predicate holds; no flag overrides a dangling-reference
// refusal. It returns nil -- leaving the destroyer's open default -- when the
// readers are absent (the shape-test compositions).
func (c *Candidate) destroyBlocker() dispatch.DestroyBlockerFunc {
	if c.registry == nil || c.reader == nil || c.deadletters == nil {
		return nil
	}
	return func(ctx context.Context, pipeline string) (bool, string, error) {
		deps, err := c.registry.DependencyEdges(ctx)
		if err != nil {
			return false, "", fmt.Errorf("read depends_on edges: %w", err)
		}
		dependsOn := make(map[string][]string, len(deps))
		for _, e := range deps {
			dependsOn[e.From] = append(dependsOn[e.From], e.To)
		}

		// One lineage read supplies both blocker inputs: the run -> pipeline
		// attribution (live runs plus pruned runs' summaries, so a downstream
		// run_inputs row naming an already-pruned target run still blocks) and the
		// consumption edges themselves.
		lineage, err := c.reader.ProvenanceLineage(ctx)
		if err != nil {
			return false, "", fmt.Errorf("read run lineage: %w", err)
		}
		runPipeline := make(map[int64]string, len(lineage.Runs)+len(lineage.Summaries))
		for _, r := range lineage.Runs {
			runPipeline[r.RunID] = r.Pipeline
		}
		for _, s := range lineage.Summaries {
			runPipeline[s.RunID] = s.Pipeline
		}
		edges := make([]dispatch.RunInputEdge, len(lineage.Inputs))
		for i, in := range lineage.Inputs {
			edges[i] = dispatch.RunInputEdge{RunID: in.RunID, InputRunID: in.UpstreamRunID}
		}

		entries, _, err := c.deadletters.worklist(ctx)
		if err != nil {
			return false, "", err
		}

		reasons := dispatch.DestroyBlockReasons(pipeline, dependsOn, edges, entries, runPipeline)
		if len(reasons) == 0 {
			return false, "", nil
		}
		return true, strings.Join(reasons, "; "), nil
	}
}
