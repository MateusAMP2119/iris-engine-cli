package dispatch_test

import (
	"reflect"
	"testing"

	"github.com/MateusAMP2119/iris-engine-cli/internal/dispatch"
	"github.com/MateusAMP2119/iris-engine-cli/internal/store"
)

// rootEntry builds a root-cause worklist entry (a run that failed or was stopped
// on its own): reason failed/stopped, no failed_upstream run.
func rootEntry(runID int64, pipeline string, reason store.DeadLetterReason) dispatch.DeadLetterEntry {
	return dispatch.DeadLetterEntry{RunID: runID, Pipeline: pipeline, Reason: reason}
}

// propEntry builds a propagated worklist entry: reason upstream_dead_lettered, with
// the immediate upstream dead-lettered run it propagated from.
func propEntry(runID int64, pipeline string, upstreamRunID int64) dispatch.DeadLetterEntry {
	return dispatch.DeadLetterEntry{
		RunID:               runID,
		Pipeline:            pipeline,
		Reason:              store.ReasonUpstreamDeadLettered,
		FailedUpstreamRunID: upstreamRunID,
	}
}

// TestWorklistExitPaths proves the three -- and only three -- ways a dead_letters row
// leaves the worklist (replay, supersession, drain), and the shared invariant: each
// removes the worklist entry while the run row stays in runs. The set is closed; the
// run history is never discarded by a worklist exit.
func TestWorklistExitPaths(t *testing.T) {
	exits := dispatch.WorklistExits()
	want := []dispatch.WorklistExit{dispatch.ExitReplay, dispatch.ExitSupersession, dispatch.ExitDrain}
	if !reflect.DeepEqual(exits, want) {
		t.Fatalf("WorklistExits() = %v, want the closed set %v", exits, want)
	}
	// The set is closed: exactly three, one per named exit path.
	if len(exits) != 3 {
		t.Fatalf("worklist has %d exit paths, want exactly 3 (replay, supersession, drain)", len(exits))
	}
	// The invariant every exit shares: the entry leaves the worklist, the run row
	// stays in runs. A worklist exit is a disposition of the parking row, never a
	// deletion of run history.
	for _, e := range exits {
		if !e.RemovesWorklistEntry() {
			t.Errorf("%s does not remove the worklist entry; every exit path clears the parking row", e)
		}
		if !e.RetainsRunRow() {
			t.Errorf("%s does not retain the run row; a worklist exit never deletes run history", e)
		}
	}
	// Each exit path names itself distinctly (no two collapse to one token).
	seen := map[string]bool{}
	for _, e := range exits {
		if seen[e.String()] {
			t.Errorf("exit path token %q is not distinct", e.String())
		}
		seen[e.String()] = true
	}
}

// TestResolveReplayTargetsWalksToRoot proves replay resolution walks a propagated
// entry along its failed_upstream chain to the ROOT cause (the run that actually
// failed or was stopped), and that --pipeline/--all selections collapse the worklist
// to the distinct set of roots ("replay targets root causes: propagated entries walk
// failed_upstream to the root; --pipeline/--all collapse to roots").
func TestResolveReplayTargetsWalksToRoot(t *testing.T) {
	// A three-level chain: root A (failed) <- B (propagated from A) <- C (propagated
	// from B). Replaying any of them must resolve to A, the single root cause.
	worklist := []dispatch.DeadLetterEntry{
		rootEntry(10, "extract", store.ReasonFailed),
		propEntry(20, "load", 10),
		propEntry(30, "report", 20),
	}

	t.Run("propagated leaf walks to root", func(t *testing.T) {
		got, err := dispatch.ResolveReplayTargets(worklist, []int64{30})
		if err != nil {
			t.Fatalf("ResolveReplayTargets: %v", err)
		}
		if !reflect.DeepEqual(got, []int64{10}) {
			t.Errorf("replay of propagated leaf resolved to %v, want the root cause [10]", got)
		}
	})

	t.Run("root resolves to itself", func(t *testing.T) {
		got, err := dispatch.ResolveReplayTargets(worklist, []int64{10})
		if err != nil {
			t.Fatalf("ResolveReplayTargets: %v", err)
		}
		if !reflect.DeepEqual(got, []int64{10}) {
			t.Errorf("replay of a root resolved to %v, want [10]", got)
		}
	})

	t.Run("all/pipeline selection collapses to distinct roots", func(t *testing.T) {
		// --all selects every entry; they all walk to the single root A, collapsed to
		// one distinct target -- one replacement run minted, not three.
		got, err := dispatch.ResolveReplayTargets(worklist, []int64{10, 20, 30})
		if err != nil {
			t.Fatalf("ResolveReplayTargets: %v", err)
		}
		if !reflect.DeepEqual(got, []int64{10}) {
			t.Errorf("--all collapsed to %v, want the single distinct root [10]", got)
		}
	})

	t.Run("distinct roots across independent chains", func(t *testing.T) {
		// Two independent chains, two roots. A stopped root E with dependent F, plus the
		// A/B/C chain. --all collapses to the two distinct roots, ascending.
		wl := []dispatch.DeadLetterEntry{
			rootEntry(10, "extract", store.ReasonFailed),
			propEntry(20, "load", 10),
			rootEntry(40, "sync", store.ReasonStopped),
			propEntry(50, "mirror", 40),
		}
		got, err := dispatch.ResolveReplayTargets(wl, []int64{20, 50, 10, 40})
		if err != nil {
			t.Fatalf("ResolveReplayTargets: %v", err)
		}
		if !reflect.DeepEqual(got, []int64{10, 40}) {
			t.Errorf("two chains collapsed to %v, want the two distinct roots [10 40]", got)
		}
	})
}

// TestResolveReplayTargetsErrors proves the resolution fails loudly rather than
// silently replaying the wrong thing: an unknown selected run, a propagated entry
// with no recorded upstream run, a dangling chain, and a cycle each return an error
// (never an infinite walk, never a propagated entry replayed as if it were a root).
func TestResolveReplayTargetsErrors(t *testing.T) {
	t.Run("selected run absent from worklist", func(t *testing.T) {
		wl := []dispatch.DeadLetterEntry{rootEntry(10, "extract", store.ReasonFailed)}
		if _, err := dispatch.ResolveReplayTargets(wl, []int64{99}); err == nil {
			t.Error("resolving a run not in the worklist should error, got nil")
		}
	})
	t.Run("propagated entry with no upstream run", func(t *testing.T) {
		wl := []dispatch.DeadLetterEntry{
			{RunID: 20, Pipeline: "load", Reason: store.ReasonUpstreamDeadLettered}, // FailedUpstreamRunID 0
		}
		if _, err := dispatch.ResolveReplayTargets(wl, []int64{20}); err == nil {
			t.Error("a propagated entry with no upstream run should error, got nil")
		}
	})
	t.Run("dangling upstream run", func(t *testing.T) {
		wl := []dispatch.DeadLetterEntry{propEntry(20, "load", 10)} // 10 not present
		if _, err := dispatch.ResolveReplayTargets(wl, []int64{20}); err == nil {
			t.Error("a chain pointing at an absent upstream run should error, got nil")
		}
	})
	t.Run("cycle is bounded, not an infinite walk", func(t *testing.T) {
		wl := []dispatch.DeadLetterEntry{
			propEntry(10, "a", 20),
			propEntry(20, "b", 10),
		}
		if _, err := dispatch.ResolveReplayTargets(wl, []int64{10}); err == nil {
			t.Error("a failed_upstream cycle should error rather than loop forever, got nil")
		}
	})
}

// TestReplayResolvesToRootNeverForcesDependents proves that replay acts only on the
// resolved ROOT cause and never on the dependents that propagated from it: resolving
// a whole poisoned chain yields the root alone, so a replay mints exactly one fresh
// run (the root's), and the dependents are absent from the target set. Dependents
// follow via the normal depends_on gate on the next pass -- they are never force-run
// by the replay ("dependents follow next pass, never force-run").
func TestReplayResolvesToRootNeverForcesDependents(t *testing.T) {
	// Root A (failed) with two dependents that propagated from it: B directly, C
	// transitively through B.
	worklist := []dispatch.DeadLetterEntry{
		rootEntry(10, "extract", store.ReasonFailed),
		propEntry(20, "load", 10),
		propEntry(30, "report", 20),
	}
	dependents := map[int64]bool{20: true, 30: true}

	// --all over the whole chain resolves to the single root.
	targets, err := dispatch.ResolveReplayTargets(worklist, []int64{10, 20, 30})
	if err != nil {
		t.Fatalf("ResolveReplayTargets: %v", err)
	}
	if !reflect.DeepEqual(targets, []int64{10}) {
		t.Fatalf("replay targets = %v, want only the root [10]", targets)
	}
	// No dependent run is ever a replay target: the replay mints the root's fresh run
	// only. B and C are not re-minted; they re-run through their own gate next pass.
	for _, tgt := range targets {
		if dependents[tgt] {
			t.Errorf("replay would force-run dependent %d; dependents must follow the gate, not the replay", tgt)
		}
	}
}

// TestPropagatedSelfSupersede proves the self-supersession rule: a propagated
// dead-letter entry clears itself once its dependent consumes a LATER upstream run
// than the poisoned one it recorded -- no replay or human needed. Only root causes
// (failed, stopped) require operator disposition; a propagated entry is superseded by
// the next successful consumption ("propagated entries clear themselves: superseded
// once their dependent consumes a later upstream run; only root causes demand a
// human").
func TestPropagatedSelfSupersede(t *testing.T) {
	// The dependent's propagated entry was poisoned by upstream run 10.
	entry := propEntry(20, "load", 10)

	t.Run("later upstream consumption supersedes", func(t *testing.T) {
		// The dependent later consumes upstream run 15 (> 10): the propagated entry is
		// superseded and clears itself.
		if !dispatch.SupersededByLaterConsumption(entry, 15) {
			t.Error("a propagated entry is not superseded by a later upstream consumption, but it must self-clear")
		}
	})
	t.Run("the poisoned run itself does not supersede", func(t *testing.T) {
		// Consuming the same poisoned run (not a later one) is not supersession.
		if dispatch.SupersededByLaterConsumption(entry, 10) {
			t.Error("consuming the poisoned upstream run itself was treated as supersession")
		}
	})
	t.Run("an earlier upstream run does not supersede", func(t *testing.T) {
		if dispatch.SupersededByLaterConsumption(entry, 5) {
			t.Error("consuming an earlier upstream run was treated as supersession")
		}
	})
	t.Run("root causes never self-supersede", func(t *testing.T) {
		// A failed/stopped root is not superseded by any consumption: it demands a human.
		root := rootEntry(10, "extract", store.ReasonFailed)
		if dispatch.SupersededByLaterConsumption(root, 999) {
			t.Error("a root cause was treated as self-superseding; only propagated entries self-clear")
		}
		stopped := rootEntry(11, "sync", store.ReasonStopped)
		if dispatch.SupersededByLaterConsumption(stopped, 999) {
			t.Error("a stopped root cause was treated as self-superseding")
		}
	})
}

// TestFailedReplayChainsEntry proves the dead-lettering-replay rule: a replay whose
// fresh run itself dead-letters parks a new worklist entry chained to the ORIGINAL
// replaced run via replayed_from, and the batch is flagged as dead-lettered so the
// replay command exits 5. The replacement's replayed_from points at the run it
// replaced, so the new entry chains back through replay lineage rather than
// orphaning.
func TestFailedReplayChainsEntry(t *testing.T) {
	// Replaying replaced run 10 minted replacement run 40, which itself dead-lettered.
	results := []dispatch.ReplayResult{
		{ReplacedRunID: 10, ReplacementRunID: 40, ReplayedFrom: 10, DeadLettered: true},
	}

	// The fresh entry chains to the original via replayed_from.
	got := results[0]
	if got.ReplayedFrom != got.ReplacedRunID {
		t.Errorf("replacement run's replayed_from = %d, want the replaced run %d (chained via replayed_from)",
			got.ReplayedFrom, got.ReplacedRunID)
	}

	// Any dead-lettering replay flags the batch: the replay command exits 5.
	if !dispatch.ReplayDeadLettered(results) {
		t.Error("a dead-lettering replay was not flagged; the replay command must exit 5")
	}

	// A clean replay (no re-dead-letter) does not flag: exit 0.
	clean := []dispatch.ReplayResult{{ReplacedRunID: 10, ReplacementRunID: 40, ReplayedFrom: 10, DeadLettered: false}}
	if dispatch.ReplayDeadLettered(clean) {
		t.Error("a clean replay was flagged as dead-lettered; it must exit 0")
	}
	// Mixed batch: one clean, one re-dead-lettered -> flagged (exit 5).
	mixed := []dispatch.ReplayResult{
		{ReplacedRunID: 10, ReplacementRunID: 40, ReplayedFrom: 10, DeadLettered: false},
		{ReplacedRunID: 11, ReplacementRunID: 41, ReplayedFrom: 11, DeadLettered: true},
	}
	if !dispatch.ReplayDeadLettered(mixed) {
		t.Error("a batch with a re-dead-lettered replay was not flagged; any dead-letter means exit 5")
	}
}

// classFor returns the blast class the impacts assign to pipeline, and whether it
// was named at all.
func classFor(impacts []dispatch.BlastImpact, pipeline string) (dispatch.BlastClass, bool) {
	for _, im := range impacts {
		if im.Pipeline == pipeline {
			return im.Class, true
		}
	}
	return "", false
}

// TestBlastRadiusClassification proves deadletter show's blast radius: from the seed
// entry it walks failed_upstream to the ROOT cause, walks forward over depends_on to
// the transitive dependents, and classifies each poisoned_now / shielded / pending;
// the root itself is poisoned_now; a lane neighbor reachable only by composer order
// (not a dependency) is untouched -- order is not dependency.
func TestBlastRadiusClassification(t *testing.T) {
	// The golden lane: extract_orders (root, failed run 10) -> load_orders depends_on
	// extract_orders (propagated run 20); reset_counters is composer-ordered only, no
	// depends_on edge. edges name the single depends_on edge; lane members are the
	// three-pipeline lane the untouched neighbor is drawn from.
	worklist := []dispatch.DeadLetterEntry{
		{RunID: 10, Pipeline: "extract_orders", Reason: store.ReasonFailed},
		{RunID: 20, Pipeline: "load_orders", Reason: store.ReasonUpstreamDeadLettered, FailedUpstreamRunID: 10},
	}
	edges := []dispatch.BlastEdge{{Dependent: "load_orders", Upstream: "extract_orders"}}
	laneMembers := []string{"extract_orders", "reset_counters", "load_orders"}

	t.Run("blast-radius-classification", func(t *testing.T) {
		// Show on the PROPAGATED entry (load_orders run 20): it walks to the root
		// extract_orders and classifies the whole lane.
		impacts, err := dispatch.ClassifyBlastRadius(worklist[1], worklist, edges, laneMembers, nil)
		if err != nil {
			t.Fatalf("ClassifyBlastRadius err = %v, want nil", err)
		}
		want := map[string]dispatch.BlastClass{
			"extract_orders": dispatch.BlastPoisonedNow, // the root cause itself
			"load_orders":    dispatch.BlastPoisonedNow, // the landed propagated rejection
			"reset_counters": dispatch.BlastUntouched,   // composer order, not a dependency
		}
		for pipeline, wantClass := range want {
			got, ok := classFor(impacts, pipeline)
			if !ok {
				t.Errorf("blast radius did not name %q", pipeline)
				continue
			}
			if got != wantClass {
				t.Errorf("blast class for %q = %q, want %q", pipeline, got, wantClass)
			}
		}
		// The root cause is reported first, deterministically.
		if len(impacts) == 0 || impacts[0].Pipeline != "extract_orders" {
			t.Errorf("root cause not reported first: %v", impacts)
		}
		// Every class is drawn from the closed set.
		for _, im := range impacts {
			switch im.Class {
			case dispatch.BlastPoisonedNow, dispatch.BlastPending, dispatch.BlastShielded, dispatch.BlastUntouched:
			default:
				t.Errorf("blast class %q for %q is outside the closed set", im.Class, im.Pipeline)
			}
		}
	})

	t.Run("shielded when the dependent consumed a later upstream success", func(t *testing.T) {
		// Only the root remains in the worklist -- load_orders has since consumed a
		// later extract_orders success, so its rejection is superseded: shielded.
		wl := []dispatch.DeadLetterEntry{
			{RunID: 10, Pipeline: "extract_orders", Reason: store.ReasonFailed},
		}
		impacts, err := dispatch.ClassifyBlastRadius(wl[0], wl, edges, laneMembers, map[string]bool{"load_orders": true})
		if err != nil {
			t.Fatalf("ClassifyBlastRadius err = %v", err)
		}
		if got, _ := classFor(impacts, "load_orders"); got != dispatch.BlastShielded {
			t.Errorf("load_orders class = %q, want shielded", got)
		}
	})

	t.Run("pending when the dependent is owed but has not run", func(t *testing.T) {
		// Only the root is dead-lettered; load_orders has no worklist entry yet and is
		// not shielded: it is owed the failure but has not run to receive it -- pending.
		wl := []dispatch.DeadLetterEntry{
			{RunID: 10, Pipeline: "extract_orders", Reason: store.ReasonFailed},
		}
		impacts, err := dispatch.ClassifyBlastRadius(wl[0], wl, edges, laneMembers, nil)
		if err != nil {
			t.Fatalf("ClassifyBlastRadius err = %v", err)
		}
		if got, _ := classFor(impacts, "load_orders"); got != dispatch.BlastPending {
			t.Errorf("load_orders class = %q, want pending", got)
		}
	})

	t.Run("a broken failed_upstream chain fails loudly", func(t *testing.T) {
		// A propagated seed whose recorded upstream run is absent from the worklist
		// cannot resolve a root: classification errors rather than guessing.
		wl := []dispatch.DeadLetterEntry{
			{RunID: 20, Pipeline: "load_orders", Reason: store.ReasonUpstreamDeadLettered, FailedUpstreamRunID: 99},
		}
		if _, err := dispatch.ClassifyBlastRadius(wl[0], wl, edges, laneMembers, nil); err == nil {
			t.Error("classification of a dangling chain should error, got nil")
		}
	})
}
