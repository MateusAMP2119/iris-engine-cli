package dispatch_test

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/MateusAMP2119/iris-engine-cli/internal/dispatch"
	"github.com/MateusAMP2119/iris-engine-cli/internal/store"
)

// gateRun builds a minimal run record for the in-flight soft-block scans.
func gateRun(id, pipeline string, state store.RunState) store.Run {
	return store.Run{ID: id, Pipeline: pipeline, Lane: "lane", State: state}
}

// TestYesHonorsSoftBlocks proves --yes satisfies the confirmation prompt but never
// overrides a soft-block: with no soft-blocks the decision proceeds (confirmation was
// the only gate and --yes supplied it); with any soft-block outstanding the decision
// refuses, carries every refusal so the caller can print its guidance, and cancels
// nothing -- refusing is not cancelling.
func TestYesHonorsSoftBlocks(t *testing.T) {
	t.Run("no soft-blocks: --yes proceeds", func(t *testing.T) {
		got := dispatch.DecideDestructive(dispatch.ConfirmYes, nil)
		if !got.Proceed {
			t.Errorf("Proceed = false, want true (--yes satisfies confirmation and nothing soft-blocks)")
		}
		if len(got.Refusals) != 0 || len(got.CancelRuns) != 0 {
			t.Errorf("clean --yes decision carries refusals %v / cancels %v, want none", got.Refusals, got.CancelRuns)
		}
	})

	t.Run("soft-blocked: --yes refuses with the blocks", func(t *testing.T) {
		blocks := dispatch.EvaluateSoftBlocks(
			dispatch.OpWorkloadWipe,
			[]store.Run{gateRun("7", "extract", store.RunRunning)},
			dispatch.GateScope{Pipeline: "extract"},
			0,
		)
		if len(blocks) == 0 {
			t.Fatalf("EvaluateSoftBlocks returned none, want the in-flight soft-block")
		}
		got := dispatch.DecideDestructive(dispatch.ConfirmYes, blocks)
		if got.Proceed {
			t.Errorf("Proceed = true, want false (--yes honors soft-blocks)")
		}
		if len(got.Refusals) != len(blocks) {
			t.Errorf("Refusals = %v, want every evaluated soft-block surfaced", got.Refusals)
		}
		if len(got.CancelRuns) != 0 {
			t.Errorf("CancelRuns = %v, want none (--yes refuses, it never cancels)", got.CancelRuns)
		}
	})
}

// TestForceOverridesSoftBlocks proves --force overrides soft-blocks and lets the
// operation proceed: the very same soft-blocks that make --yes refuse are overridden
// by --force, and the decision names each in-flight run the override must cancel
// (dead-lettered stopped) before the operation runs.
func TestForceOverridesSoftBlocks(t *testing.T) {
	runs := []store.Run{
		gateRun("7", "extract", store.RunRunning),
		gateRun("9", "extract", store.RunQueued),
	}
	blocks := dispatch.EvaluateSoftBlocks(dispatch.OpDeclareDestroy, runs, dispatch.GateScope{Pipeline: "extract"}, 3)
	if len(blocks) != 2 {
		t.Fatalf("EvaluateSoftBlocks = %v, want the in-flight and un-promoted-data blocks", blocks)
	}

	// Sanity: the same blocks refuse under --yes, so the override below is real.
	if dispatch.DecideDestructive(dispatch.ConfirmYes, blocks).Proceed {
		t.Fatalf("sanity check failed: --yes proceeded past soft-blocks")
	}

	got := dispatch.DecideDestructive(dispatch.ConfirmForce, blocks)
	if !got.Proceed {
		t.Errorf("Proceed = false, want true (--force overrides soft-blocks)")
	}
	if len(got.Refusals) != 0 {
		t.Errorf("Refusals = %v, want none under --force", got.Refusals)
	}
	wantCancels := []string{"7", "9"}
	if len(got.CancelRuns) != len(wantCancels) {
		t.Fatalf("CancelRuns = %v, want %v (--force cancels the in-flight runs it overrides)", got.CancelRuns, wantCancels)
	}
	for i, id := range wantCancels {
		if got.CancelRuns[i] != id {
			t.Errorf("CancelRuns[%d] = %q, want %q", i, got.CancelRuns[i], id)
		}
	}
}

// TestYesSoftBlockInFlightRun proves the in-flight-run soft-block scopes exactly:
// non-interactive --yes confirms the gate but still refuses with guidance while a run
// is queued or running on the affected scope. A run in flight on ANOTHER pipeline
// never blocks a scoped op; an engine-wide scope (uninstall, bare wipe) blocks on any
// in-flight run; terminal runs never block.
func TestYesSoftBlockInFlightRun(t *testing.T) {
	runs := []store.Run{
		gateRun("1", "extract", store.RunSucceeded),
		gateRun("2", "extract", store.RunDeadLettered),
		gateRun("3", "load", store.RunRunning),
		gateRun("4", "report", store.RunQueued),
	}

	cases := []struct {
		name      string
		op        dispatch.DestructiveOp
		scope     dispatch.GateScope
		wantRuns  []string
		wantBlock bool
	}{
		{"scoped op, run running on scope", dispatch.OpDeclareDestroy, dispatch.GateScope{Pipeline: "load"}, []string{"3"}, true},
		{"scoped op, run queued on scope", dispatch.OpWorkloadWipe, dispatch.GateScope{Pipeline: "report"}, []string{"4"}, true},
		{"scoped op, only terminal runs on scope", dispatch.OpDeclareDestroy, dispatch.GateScope{Pipeline: "extract"}, nil, false},
		{"engine-wide op blocks on any in-flight run", dispatch.OpEngineUninstall, dispatch.GateScope{}, []string{"3", "4"}, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			blocks := dispatch.EvaluateSoftBlocks(tc.op, runs, tc.scope, 0)
			var inflight *dispatch.SoftBlock
			for i := range blocks {
				if blocks[i].Kind == dispatch.SoftBlockInFlightRun {
					inflight = &blocks[i]
				}
			}
			if !tc.wantBlock {
				if inflight != nil {
					t.Fatalf("got in-flight soft-block %v, want none", *inflight)
				}
				if got := dispatch.DecideDestructive(dispatch.ConfirmYes, blocks); !got.Proceed {
					t.Errorf("--yes refused with no soft-block on scope: %v", got.Refusals)
				}
				return
			}
			if inflight == nil {
				t.Fatalf("EvaluateSoftBlocks = %v, want the in-flight soft-block", blocks)
			}
			if len(inflight.Runs) != len(tc.wantRuns) {
				t.Fatalf("in-flight block runs = %v, want %v", inflight.Runs, tc.wantRuns)
			}
			for i, id := range tc.wantRuns {
				if inflight.Runs[i] != id {
					t.Errorf("in-flight block runs[%d] = %q, want %q", i, inflight.Runs[i], id)
				}
			}
			got := dispatch.DecideDestructive(dispatch.ConfirmYes, blocks)
			if got.Proceed {
				t.Errorf("--yes proceeded past an in-flight run on the affected scope")
			}
			if len(got.Refusals) == 0 || got.Refusals[0].Guidance == "" {
				t.Errorf("refusal carries no guidance; the operator must be told the remedy")
			}
		})
	}
}

// TestYesSoftBlockUnpromotedData proves the un-promoted-data soft-block is a
// teardown-only refusal: non-interactive --yes on engine uninstall or declare destroy
// refuses with guidance while un-promoted disposable data exists (a non-empty wipe
// scope), while workload wipe and deadletter drain -- the dev-loop ops -- never raise
// it (wiping that data is the point of the op), and a clean scope raises nothing.
func TestYesSoftBlockUnpromotedData(t *testing.T) {
	cases := []struct {
		name       string
		op         dispatch.DestructiveOp
		unpromoted int
		wantBlock  bool
	}{
		{"engine uninstall with un-promoted data", dispatch.OpEngineUninstall, 4, true},
		{"declare destroy with un-promoted data", dispatch.OpDeclareDestroy, 1, true},
		{"workload wipe never raises it", dispatch.OpWorkloadWipe, 4, false},
		{"deadletter drain never raises it", dispatch.OpDeadletterDrain, 4, false},
		{"teardown with a clean scope", dispatch.OpEngineUninstall, 0, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			blocks := dispatch.EvaluateSoftBlocks(tc.op, nil, dispatch.GateScope{}, tc.unpromoted)
			var block *dispatch.SoftBlock
			for i := range blocks {
				if blocks[i].Kind == dispatch.SoftBlockUnpromotedData {
					block = &blocks[i]
				}
			}
			if !tc.wantBlock {
				if block != nil {
					t.Fatalf("got un-promoted-data soft-block %v, want none", *block)
				}
				if got := dispatch.DecideDestructive(dispatch.ConfirmYes, blocks); !got.Proceed {
					t.Errorf("--yes refused with nothing soft-blocking: %v", got.Refusals)
				}
				return
			}
			if block == nil {
				t.Fatalf("EvaluateSoftBlocks = %v, want the un-promoted-data soft-block", blocks)
			}
			got := dispatch.DecideDestructive(dispatch.ConfirmYes, blocks)
			if got.Proceed {
				t.Errorf("--yes proceeded past un-promoted disposable data on a teardown")
			}
			if block.Guidance == "" {
				t.Errorf("un-promoted-data refusal carries no guidance; the operator must be told the remedy")
			}
		})
	}
}

// TestDestroyDownstreamBlockers proves the declare-destroy downstream-blocker
// predicates: destroy refuses and NAMES the blockers -- drop or drain first -- while
// any registered pipeline declares depends_on on the target, any downstream
// run_inputs row names the target's runs, or any outstanding dead-letter entry names
// the target as failed_upstream. A target with none of the three is destroyable; the
// target's own rows (its self-inputs, its own worklist entries) never block, since
// the destroy retires them itself.
func TestDestroyDownstreamBlockers(t *testing.T) {
	runPipeline := map[int64]string{
		10: "extract", 11: "extract",
		20: "load",
		30: "report",
	}

	t.Run("each predicate blocks and is named", func(t *testing.T) {
		reasons := dispatch.DestroyBlockReasons(
			"extract",
			map[string][]string{"load": {"extract"}, "report": {"other"}},
			[]dispatch.RunInputEdge{{RunID: 20, InputRunID: 10}},
			[]dispatch.DeadLetterEntry{propEntry(30, "report", 11)},
			runPipeline,
		)
		if len(reasons) != 3 {
			t.Fatalf("DestroyBlockReasons = %v, want one reason per predicate (dependent, run_inputs, dead-letter)", reasons)
		}
		joined := strings.Join(reasons, "\n")
		for _, name := range []string{"load", "report"} {
			if !strings.Contains(joined, name) {
				t.Errorf("reasons %q never name blocker %q", joined, name)
			}
		}
		// The refusal must carry the remedy: drop (destroy) or drain first.
		if !strings.Contains(joined, "destroy") || !strings.Contains(joined, "drain") {
			t.Errorf("reasons %q carry no drop-or-drain-first remedy", joined)
		}
	})

	t.Run("unrelated rows and the target's own rows never block", func(t *testing.T) {
		reasons := dispatch.DestroyBlockReasons(
			"extract",
			map[string][]string{"report": {"other"}, "extract": {"other"}},
			[]dispatch.RunInputEdge{
				{RunID: 11, InputRunID: 10}, // the target consuming its own run
				{RunID: 30, InputRunID: 20}, // a downstream edge not touching the target
			},
			[]dispatch.DeadLetterEntry{
				rootEntry(10, "extract", store.ReasonFailed), // the target's own entry, retired by the destroy
				propEntry(30, "report", 20),                  // propagated from load, not from the target
			},
			runPipeline,
		)
		if len(reasons) != 0 {
			t.Errorf("DestroyBlockReasons = %v, want none (nothing downstream names the target)", reasons)
		}
	})

	t.Run("the destroy op itself refuses on the predicate", func(t *testing.T) {
		// Wire the pure predicate into the Destroyer's blocker seam: the refusal is a
		// BlockedError naming the blockers, and nothing is torn down.
		blocker := dispatch.DestroyBlockerFunc(func(_ context.Context, pipeline string) (bool, string, error) {
			reasons := dispatch.DestroyBlockReasons(
				pipeline,
				map[string][]string{"load": {"extract"}},
				nil, nil, runPipeline,
			)
			if len(reasons) == 0 {
				return false, "", nil
			}
			return true, strings.Join(reasons, "; "), nil
		})
		d := dispatch.NewDestroyer(nil, nil, dispatch.WithDestroyBlocker(blocker))
		err := d.DestroyPipeline(context.Background(), "extract")
		var blocked *dispatch.BlockedError
		if !errors.As(err, &blocked) {
			t.Fatalf("DestroyPipeline = %v, want a BlockedError refusal", err)
		}
		if !strings.Contains(blocked.Reason, "load") {
			t.Errorf("BlockedError reason %q never names the blocking dependent", blocked.Reason)
		}
	})
}
