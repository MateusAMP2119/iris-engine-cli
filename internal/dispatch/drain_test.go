package dispatch_test

import (
	"reflect"
	"testing"

	"github.com/MateusAMP2119/iris-lakehouse/internal/dispatch"
	"github.com/MateusAMP2119/iris-lakehouse/internal/store"
)

// TestResolveDrainTargetsScopedOnly proves drain resolves EXACTLY the entries the
// operator's scope names and no others: <run> resolves to that one entry, --pipeline
// to every outstanding entry for that pipeline (and none of another pipeline's),
// --all to every outstanding entry. Critically, and unlike replay, drain never walks
// failed_upstream to a root: a propagated entry is discarded as ITSELF, not collapsed
// to the cause it propagated from -- the same worklist resolved for replay walks to
// the root, resolved for drain stays put, proving the two are deliberately different
// operations.
func TestResolveDrainTargetsScopedOnly(t *testing.T) {
	// Two independent chains across two pipelines, so a --pipeline scope has
	// something else in the worklist it must NOT touch.
	worklist := []dispatch.DeadLetterEntry{
		rootEntry(10, "extract", store.ReasonFailed),
		propEntry(20, "load", 10),
		propEntry(30, "report", 20),
		rootEntry(40, "sync", store.ReasonStopped),
	}

	t.Run("<run> on a root resolves to itself", func(t *testing.T) {
		got, err := dispatch.ResolveDrainTargets(worklist, dispatch.DrainScope{Run: 10})
		if err != nil {
			t.Fatalf("ResolveDrainTargets: %v", err)
		}
		if !reflect.DeepEqual(got, []int64{10}) {
			t.Errorf("<run> 10 resolved to %v, want [10]", got)
		}
	})

	t.Run("<run> on a propagated entry stays itself -- no root walk", func(t *testing.T) {
		// Replaying run 30 would walk failed_upstream to root 10. Draining run 30
		// must NOT: the propagated entry itself is the drain target.
		got, err := dispatch.ResolveDrainTargets(worklist, dispatch.DrainScope{Run: 30})
		if err != nil {
			t.Fatalf("ResolveDrainTargets: %v", err)
		}
		if !reflect.DeepEqual(got, []int64{30}) {
			t.Errorf("<run> 30 resolved to %v, want [30] (drain never walks to root)", got)
		}

		// Contrast: the very same worklist, resolved for REPLAY, walks to the root.
		replayGot, err := dispatch.ResolveReplayTargets(worklist, []int64{30})
		if err != nil {
			t.Fatalf("ResolveReplayTargets: %v", err)
		}
		if !reflect.DeepEqual(replayGot, []int64{10}) {
			t.Fatalf("sanity check failed: replay of 30 resolved to %v, want the root [10]", replayGot)
		}
	})

	t.Run("--pipeline collects only that pipeline's entries", func(t *testing.T) {
		got, err := dispatch.ResolveDrainTargets(worklist, dispatch.DrainScope{Pipeline: "load"})
		if err != nil {
			t.Fatalf("ResolveDrainTargets: %v", err)
		}
		if !reflect.DeepEqual(got, []int64{20}) {
			t.Errorf("--pipeline load resolved to %v, want [20] (no others touched)", got)
		}
	})

	t.Run("--pipeline with no outstanding entries resolves to none, not an error", func(t *testing.T) {
		got, err := dispatch.ResolveDrainTargets(worklist, dispatch.DrainScope{Pipeline: "no_such_pipeline"})
		if err != nil {
			t.Fatalf("ResolveDrainTargets: %v", err)
		}
		if len(got) != 0 {
			t.Errorf("--pipeline with no entries resolved to %v, want none", got)
		}
	})

	t.Run("--all collects every outstanding entry", func(t *testing.T) {
		got, err := dispatch.ResolveDrainTargets(worklist, dispatch.DrainScope{All: true})
		if err != nil {
			t.Fatalf("ResolveDrainTargets: %v", err)
		}
		want := []int64{10, 20, 30, 40}
		if !reflect.DeepEqual(got, want) {
			t.Errorf("--all resolved to %v, want %v", got, want)
		}
	})

	t.Run("a named run absent from the worklist fails loudly", func(t *testing.T) {
		if _, err := dispatch.ResolveDrainTargets(worklist, dispatch.DrainScope{Run: 999}); err == nil {
			t.Error("draining a run not in the worklist should error, got nil")
		}
	})

	t.Run("an empty scope names none of <run>/--pipeline/--all and errors", func(t *testing.T) {
		if _, err := dispatch.ResolveDrainTargets(worklist, dispatch.DrainScope{}); err == nil {
			t.Error("an empty scope should error rather than silently draining nothing or everything")
		}
	})
}

// TestDrainedRunNeverReplayable proves the structural half of the "drained runs can
// never be replayed" rule: once drain resolves and discards a run's worklist entry,
// that entry -- the run's only replay ticket -- is gone from the worklist, so a
// subsequent replay resolution for the same run id fails loudly rather than minting a
// fresh run for it. The run row itself is untouched by this removal
// (WorklistExit.RetainsRunRow, proven in TestWorklistExitPaths), so it stays in runs,
// its only remaining fate release from the outstanding-dead_letters guard: prunable,
// never replayable.
func TestDrainedRunNeverReplayable(t *testing.T) {
	worklist := []dispatch.DeadLetterEntry{
		rootEntry(10, "extract", store.ReasonFailed),
		propEntry(20, "load", 10),
	}

	// Drain root 10 (the case that matters most: a bare root cause with a live
	// dependent still pointing at it).
	targets, err := dispatch.ResolveDrainTargets(worklist, dispatch.DrainScope{Run: 10})
	if err != nil {
		t.Fatalf("ResolveDrainTargets: %v", err)
	}
	postDrain := dispatch.RemoveDrained(worklist, targets)

	// The drained run's entry is gone from the worklist.
	for _, e := range postDrain {
		if e.RunID == 10 {
			t.Fatalf("run 10's worklist entry survives drain: %+v", e)
		}
	}
	// Every other entry survives: drain touched only the scoped run.
	if len(postDrain) != 1 || postDrain[0].RunID != 20 {
		t.Fatalf("drain of run 10 altered other entries: %+v, want only run 20 to survive", postDrain)
	}

	// The entry was the replay ticket: with it gone, replay resolution for the
	// drained run fails loudly instead of minting a replacement.
	if _, err := dispatch.ResolveReplayTargets(postDrain, []int64{10}); err == nil {
		t.Error("replay resolved a target for a drained run; a drained run must never be replayable again")
	}
}
