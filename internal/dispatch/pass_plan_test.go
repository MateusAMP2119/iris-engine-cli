package dispatch_test

import (
	"reflect"
	"testing"

	"github.com/MateusAMP2119/iris-engine-cli/internal/dispatch"
	"github.com/MateusAMP2119/iris-engine-cli/internal/store"
)

// TestPassFreshRunNoRetry proves each pass starts every open-gated pipeline as a FRESH
// run on current data (cause=loop, no replayed_from), that the plan is a function of the
// walk and the gate alone -- so it is stable pass to pass with no backoff state -- and
// that a closed gate mints no run (specification sections 1 and 6.3). A failed run is
// never retried here: the plan takes no prior-run outcome, and the run it starts is fresh
// (cause=loop), never a re-dispatch of a specific earlier run.
func TestPassFreshRunNoRetry(t *testing.T) {
	t.Run("S06.3/pass-fresh-run-no-retry", func(t *testing.T) {
		members := []string{"extract", "skipme", "transform"}
		decide := map[string]dispatch.Decision{
			"extract":   {Run: true},                      // ungated: runs every pass
			"skipme":    {Run: false},                     // closed gate: nothing new to consume
			"transform": {Run: true, Consume: []int64{7}}, // open gate: consumes upstream run 7
		}

		got := dispatch.PlanFreshRuns(members, decide)

		want := []store.RunRecord{
			{Pipeline: "extract", Cause: store.CauseLoop},
			{Pipeline: "transform", Cause: store.CauseLoop, ConsumedUpstreamRunIDs: []int64{7}},
		}
		if !reflect.DeepEqual(got, want) {
			t.Fatalf("PlanFreshRuns = %+v, want %+v (open-gated only, in composer order, fresh)", got, want)
		}

		// Every started run is FRESH, never a retry: cause=loop and no replayed_from.
		for _, r := range got {
			if r.Cause != store.CauseLoop {
				t.Errorf("run for %q has cause %q, want %q (a loop run is always fresh)", r.Pipeline, r.Cause, store.CauseLoop)
			}
			if r.ReplayedFrom != nil {
				t.Errorf("run for %q carries replayed_from %v, want nil (a fresh run is not a retry)", r.Pipeline, *r.ReplayedFrom)
			}
		}

		// No backoff state: the same walk and gate yield the same plan on the next pass.
		again := dispatch.PlanFreshRuns(members, decide)
		if !reflect.DeepEqual(again, got) {
			t.Fatalf("second pass plan = %+v, want %+v (each pass is fresh, no backoff or retry queue)", again, got)
		}

		// A pipeline absent from the gate map is ungated: it still starts a fresh run
		// every pass on current data, even if its own previous run had failed -- a fresh
		// run, never a retry.
		solo := dispatch.PlanFreshRuns([]string{"loader"}, nil)
		wantSolo := []store.RunRecord{{Pipeline: "loader", Cause: store.CauseLoop}}
		if !reflect.DeepEqual(solo, wantSolo) {
			t.Fatalf("ungated plan = %+v, want %+v (fresh cause=loop run every pass)", solo, wantSolo)
		}
	})
}

// TestNoAutoRetry proves the engine never automatically retries a failed run: a
// dead-lettered upstream poisons the dependent's gate, so the pass plan starts no run for
// it (failure propagates as post-pass bookkeeping, not a fresh loop run), and the plan
// never emits a replay -- re-executing a dead-lettered run is only ever an explicit
// operator replay (specification section 1).
func TestNoAutoRetry(t *testing.T) {
	t.Run("S01/no-auto-retry", func(t *testing.T) {
		members := []string{"root", "dependent", "consumed"}
		poisoned := dispatch.Decision{
			Poisoned: true,
			Ledger:   []dispatch.EdgeVerdict{{Upstream: "root", Verdict: dispatch.VerdictPoisoned, LatestRunID: 42}},
		}
		decide := map[string]dispatch.Decision{
			"root":      {Run: true},  // the ungated root runs fresh
			"dependent": poisoned,     // awaited upstream dead-lettered: gate poisoned
			"consumed":  {Run: false}, // already consumed the latest success: nothing new
		}

		got := dispatch.PlanFreshRuns(members, decide)

		// The poisoned dependent is NOT auto-re-dispatched, and the already-consumed
		// pipeline is not re-run: only the ungated root starts a fresh run.
		want := []store.RunRecord{{Pipeline: "root", Cause: store.CauseLoop}}
		if !reflect.DeepEqual(got, want) {
			t.Fatalf("PlanFreshRuns = %+v, want %+v (no auto-retry of the poisoned or dead-lettered)", got, want)
		}

		// Re-execution is only ever an explicit replay: the loop plan never mints a
		// replay run on its own (cause is always loop, never replay).
		for _, r := range got {
			if r.Cause == store.CauseReplay {
				t.Errorf("loop plan minted a replay run for %q; re-execution must require an operator replay", r.Pipeline)
			}
		}

		// A dead-lettered run held in the worklist is not re-dispatched without operator
		// action: with only a poisoned/closed gate, the plan is empty -- nothing re-runs.
		none := dispatch.PlanFreshRuns([]string{"dependent"}, map[string]dispatch.Decision{"dependent": poisoned})
		if len(none) != 0 {
			t.Fatalf("poisoned-only plan = %+v, want empty (a dead-lettered run is never re-dispatched automatically)", none)
		}
	})
}
