package store_test

import (
	"context"
	"reflect"
	"strings"
	"testing"

	"github.com/MateusAMP2119/iris-engine-cli/internal/store"
	"github.com/MateusAMP2119/iris-engine-cli/internal/store/storetest"
)

// These tests prove the stats rollup composition of specification section 11
// against the meta-store fake: the engine-wide, per-lane, and per-pipeline
// rollups BuildStats derives from plain meta reads plus the leader-held pass
// counts. Everything is a current count or a last-value -- the composition takes
// no clock and stores no history.

// terminalWithExit transitions a run to state carrying an exit code, failing the
// test on error.
func terminalWithExit(t *testing.T, f *storetest.StatsFake, id string, state store.RunState, exit int) {
	t.Helper()
	if _, err := f.SetRunState(context.Background(), id, state, store.WithExitCode(exit)); err != nil {
		t.Fatalf("set run %s state: %v", id, err)
	}
}

// mustCreate creates a run on the fake, failing the test on error.
func mustCreate(t *testing.T, f *storetest.StatsFake, pipeline, lane string) store.Run {
	t.Helper()
	r, err := f.CreateRun(context.Background(), store.RunSpec{Pipeline: pipeline, Lane: lane})
	if err != nil {
		t.Fatalf("create run for %s: %v", pipeline, err)
	}
	return r
}

// TestStatsEngineRollup proves the engine-wide rollup: dead-letter worklist depth
// and counts by reason, running runs, capture counters, the wipe-eligible slice,
// total journal size, and the lifecycle readout (hot rows, sealed and archived
// partition counts, checkpoint chain head) -- all current counts and last-values
// sourced from the meta reads (specification sections 11 and 14).
func TestStatsEngineRollup(t *testing.T) {
	t.Run("S11/stats-engine-rollup", func(t *testing.T) {
		f := storetest.NewStats()
		f.RegisterPipeline("extract")
		f.AddLaneMember("ingest", "extract")

		// One running run, one queued, one dead-lettered.
		running := mustCreate(t, f, "extract", "ingest")
		if _, err := f.SetRunState(context.Background(), running.ID, store.RunRunning); err != nil {
			t.Fatalf("set running: %v", err)
		}
		mustCreate(t, f, "extract", "ingest")
		dead := mustCreate(t, f, "extract", "ingest")
		terminalWithExit(t, f, dead.ID, store.RunDeadLettered, 3)

		// The outstanding dead-letter worklist: two failed, one stopped.
		f.AddDeadLetter(store.DeadLetterEntry{RunID: dead.ID, Reason: store.ReasonFailed})
		f.AddDeadLetter(store.DeadLetterEntry{RunID: "run-90", Reason: store.ReasonFailed})
		f.AddDeadLetter(store.DeadLetterEntry{RunID: "run-91", Reason: store.ReasonStopped})

		// Journal counters (capture counters, wipe-eligible slice, size, hot rows).
		f.SetJournal(store.JournalStats{
			CapturedWrites:   120,
			WipeEligibleRows: 40,
			TotalRows:        200,
			HotRows:          150,
		})

		// The checkpoint chain: three sealed partitions, one archived; the head is
		// the highest-seq checkpoint.
		f.AddCheckpoint(store.Checkpoint{Seq: 1, Digest: "d1", Location: "archived"})
		f.AddCheckpoint(store.Checkpoint{Seq: 2, Digest: "d2", Location: "resident"})
		f.AddCheckpoint(store.Checkpoint{Seq: 3, Digest: "d3", Location: "resident"})

		rollup, err := store.BuildStats(context.Background(), f, nil)
		if err != nil {
			t.Fatalf("BuildStats: %v", err)
		}
		e := rollup.Engine
		if e.DeadLetterDepth != 3 {
			t.Errorf("dead-letter depth = %d, want 3", e.DeadLetterDepth)
		}
		wantReasons := map[string]int64{"failed": 2, "stopped": 1}
		if !reflect.DeepEqual(e.DeadLettersByReason, wantReasons) {
			t.Errorf("dead letters by reason = %v, want %v", e.DeadLettersByReason, wantReasons)
		}
		if e.RunningRuns != 1 {
			t.Errorf("running runs = %d, want 1", e.RunningRuns)
		}
		if e.CapturedWrites != 120 || e.WipeEligibleRows != 40 || e.JournalRows != 200 || e.HotRows != 150 {
			t.Errorf("journal counters = (captured %d, wipe-eligible %d, rows %d, hot %d), want (120, 40, 200, 150)",
				e.CapturedWrites, e.WipeEligibleRows, e.JournalRows, e.HotRows)
		}
		if e.SealedPartitions != 3 || e.ArchivedPartitions != 1 {
			t.Errorf("partitions = (sealed %d, archived %d), want (3, 1)", e.SealedPartitions, e.ArchivedPartitions)
		}
		if e.ChainHead == nil {
			t.Fatal("chain head = nil, want the highest-seq checkpoint")
		}
		if e.ChainHead.Seq != 3 || e.ChainHead.Digest != "d3" || e.ChainHead.Location != "resident" {
			t.Errorf("chain head = %+v, want seq 3 digest d3 resident", *e.ChainHead)
		}
	})

	t.Run("empty engine rolls up to zeros and an absent chain head", func(t *testing.T) {
		// spec: S11/stats-engine-rollup
		rollup, err := store.BuildStats(context.Background(), storetest.NewStats(), nil)
		if err != nil {
			t.Fatalf("BuildStats over empty state: %v", err)
		}
		e := rollup.Engine
		if e.DeadLetterDepth != 0 || e.RunningRuns != 0 || e.SealedPartitions != 0 || e.ArchivedPartitions != 0 {
			t.Errorf("empty engine rollup carries nonzero counts: %+v", e)
		}
		if e.DeadLettersByReason == nil {
			t.Error("dead letters by reason = nil, want an empty (non-nil) map")
		}
		if e.ChainHead != nil {
			t.Errorf("chain head = %+v, want nil with no checkpoint rows (explicit absence)", *e.ChainHead)
		}
	})
}

// TestStatsLaneRollup proves the per-lane rollup: pipeline count, queued/running
// count, and loop passes completed since daemon start -- the leader-held runtime
// count handed in, never a clock (specification section 11).
func TestStatsLaneRollup(t *testing.T) {
	t.Run("S11/stats-lane-rollup", func(t *testing.T) {
		f := storetest.NewStats()
		for _, p := range []string{"extract", "reset", "load", "solo"} {
			f.RegisterPipeline(p)
		}
		f.AddLaneMember("ingest", "extract")
		f.AddLaneMember("ingest", "reset")
		f.AddLaneMember("ingest", "load")
		f.AddLaneMember("side", "solo")

		// ingest: one queued (extract), one running (load), one terminal (reset).
		mustCreate(t, f, "extract", "ingest")
		done := mustCreate(t, f, "reset", "ingest")
		terminalWithExit(t, f, done.ID, store.RunSucceeded, 0)
		run := mustCreate(t, f, "load", "ingest")
		if _, err := f.SetRunState(context.Background(), run.ID, store.RunRunning); err != nil {
			t.Fatalf("set running: %v", err)
		}

		rollup, err := store.BuildStats(context.Background(), f, map[string]int64{"ingest": 12})
		if err != nil {
			t.Fatalf("BuildStats: %v", err)
		}
		if len(rollup.Lanes) != 2 {
			t.Fatalf("lane rollups = %d lanes, want 2 (ingest, side)", len(rollup.Lanes))
		}
		byName := map[string]store.LaneRollup{}
		for _, l := range rollup.Lanes {
			byName[l.Lane] = l
		}
		ingest := byName["ingest"]
		if ingest.Pipelines != 3 {
			t.Errorf("ingest pipeline count = %d, want 3", ingest.Pipelines)
		}
		if ingest.Queued != 1 || ingest.Running != 1 {
			t.Errorf("ingest queued/running = %d/%d, want 1/1", ingest.Queued, ingest.Running)
		}
		if ingest.Passes != 12 {
			t.Errorf("ingest passes = %d, want 12 (the leader-held count since daemon start)", ingest.Passes)
		}
		side := byName["side"]
		if side.Pipelines != 1 || side.Queued != 0 || side.Running != 0 || side.Passes != 0 {
			t.Errorf("side rollup = %+v, want 1 pipeline, no activity, zero passes", side)
		}
	})
}

// TestStatsPipelineRollup proves the per-pipeline rollup: latest run state, run
// counts by state, last exit code, and last run id -- last-values from the run
// history's ordering identity, never a clock (specification section 11).
func TestStatsPipelineRollup(t *testing.T) {
	t.Run("S11/stats-pipeline-rollup", func(t *testing.T) {
		f := storetest.NewStats()
		f.RegisterPipeline("extract")
		f.RegisterPipeline("idle")

		// extract's history, in identity order: succeeded (exit 0), dead-lettered
		// (exit 3), then a queued run with no exit code yet.
		first := mustCreate(t, f, "extract", "ingest")
		terminalWithExit(t, f, first.ID, store.RunSucceeded, 0)
		second := mustCreate(t, f, "extract", "ingest")
		terminalWithExit(t, f, second.ID, store.RunDeadLettered, 3)
		third := mustCreate(t, f, "extract", "ingest")

		rollup, err := store.BuildStats(context.Background(), f, nil)
		if err != nil {
			t.Fatalf("BuildStats: %v", err)
		}
		byName := map[string]store.PipelineRollup{}
		for _, p := range rollup.Pipelines {
			byName[p.Pipeline] = p
		}

		extract, ok := byName["extract"]
		if !ok {
			t.Fatal("extract missing from the pipeline rollup")
		}
		if extract.LatestRunState != string(store.RunQueued) {
			t.Errorf("latest run state = %q, want queued (the most recent run by identity order)", extract.LatestRunState)
		}
		if extract.LastRunID != third.ID {
			t.Errorf("last run id = %q, want %q", extract.LastRunID, third.ID)
		}
		if extract.LastExitCode == nil || *extract.LastExitCode != 3 {
			t.Errorf("last exit code = %v, want 3 (the most recent run carrying one)", extract.LastExitCode)
		}
		wantCounts := map[string]int64{"queued": 1, "succeeded": 1, "dead_lettered": 1}
		if !reflect.DeepEqual(extract.RunsByState, wantCounts) {
			t.Errorf("runs by state = %v, want %v", extract.RunsByState, wantCounts)
		}

		// A registered pipeline with no runs still appears, with empty last-values.
		idle, ok := byName["idle"]
		if !ok {
			t.Fatal("idle (registered, never run) missing from the pipeline rollup")
		}
		if idle.LatestRunState != "" || idle.LastRunID != "" || idle.LastExitCode != nil {
			t.Errorf("idle rollup carries last-values with no history: %+v", idle)
		}
		if len(idle.RunsByState) != 0 {
			t.Errorf("idle runs by state = %v, want empty", idle.RunsByState)
		}
	})
}

// TestCheckpointsNeverPruned proves the unit contract that journal_checkpoints
// rows are never pruned by any retention policy.
//
// spec: S14/checkpoints-never-pruned
func TestCheckpointsNeverPruned(t *testing.T) {
	t.Run("S14/checkpoints-never-pruned", func(t *testing.T) {
		rec := storetest.NewWriteRecorder()
		w := store.NewWriter(rec)
		_ = w.EnsureSchema(context.Background())
		for _, s := range rec.Statements() {
			u := strings.ToUpper(s.SQL)
			if strings.Contains(u, "JOURNAL_CHECKPOINTS") && (strings.Contains(u, "DELETE") || strings.Contains(u, "UPDATE")) {
				t.Errorf("pruned: %s", s.SQL)
			}
		}
		cp := store.CheckpointRow{IDFrom: 1, IDTo: 2, Digest: []byte("d"), Location: "resident", RecordedAt: "t"}
		if err := w.InsertCheckpoint(context.Background(), cp); err != nil {
			t.Fatalf("insert: %v", err)
		}
	})
}
