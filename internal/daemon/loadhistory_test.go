package daemon

import (
	"context"
	"errors"
	"testing"

	"github.com/MateusAMP2119/iris-lakehouse/internal/api"
	"github.com/MateusAMP2119/iris-lakehouse/internal/store"
)

// loadTestFixture is the collector fixture: a running run (group 300) on the
// ingest lane, a queued run (no live group), and a host sample where the
// daemon (pid 100), the managed postmaster (pid 200, plus a backend), the
// run's group, and an unrelated process all appear.
func loadTestFixture() (fakeRunReader, fakeProbe) {
	runs := fakeRunReader{runs: []store.Run{
		{ID: "3", Pipeline: "load", Lane: "ingest", State: store.RunRunning, Handle: 300, Seq: 3},
		{ID: "4", Pipeline: "load", Lane: "ingest", State: store.RunQueued, Seq: 4},
	}}
	probe := fakeProbe{samples: []procSample{
		{PID: 100, PGID: 100, PPID: 1, CPUPercent: 1.0, RSSBytes: 10 << 20},
		{PID: 200, PGID: 200, PPID: 1, CPUPercent: 0.5, RSSBytes: 20 << 20},
		{PID: 201, PGID: 201, PPID: 200, CPUPercent: 0.5, RSSBytes: 10 << 20},
		{PID: 300, PGID: 300, PPID: 100, CPUPercent: 25, RSSBytes: 5 << 20},
		{PID: 301, PGID: 300, PPID: 300, CPUPercent: 25, RSSBytes: 5 << 20},
		{PID: 999, PGID: 999, PPID: 1, CPUPercent: 90, RSSBytes: 1 << 30},
	}}
	return runs, probe
}

// TestLoadHistoryCollects proves one collector tick: the tick counter
// advances, the latest sample carries the engine tree and the per-group sums,
// and the series map records the engine plus the running run's lane and
// pipeline attribution.
func TestLoadHistoryCollects(t *testing.T) {
	t.Run("load-history-collects", func(t *testing.T) {
		runs, probe := loadTestFixture()
		h := psTestLoads(runs, probe)

		tick, engine, groups := h.latest()
		if tick != 1 {
			t.Fatalf("tick = %d, want 1 after one sample", tick)
		}
		if engine == nil || engine.CPUPercent != 52.0 || engine.RSSBytes != 50<<20 {
			t.Fatalf("engine load = %+v, want the daemon + postmaster trees (cpu 52.0, rss 50MiB)", engine)
		}
		if g := groups[300]; g == nil || g.CPUPercent != 50 || g.RSSBytes != 10<<20 {
			t.Fatalf("group 300 = %+v, want the run group summed (cpu 50, rss 10MiB)", g)
		}

		doc := h.snapshot()
		if doc == nil || doc.FineIntervalSeconds <= 0 || doc.CoarseIntervalSeconds <= doc.FineIntervalSeconds {
			t.Fatalf("history doc = %+v, want intervals fine < coarse", doc)
		}
		byKey := map[string]api.PsSeries{}
		for _, s := range doc.Series {
			byKey[s.Key] = s
		}
		for key, wantCPU := range map[string]float64{"engine": 52.0, "lane:ingest": 50, "pipeline:load": 50} {
			s, ok := byKey[key]
			if !ok || len(s.CPU) != 1 || s.CPU[0] != wantCPU {
				t.Errorf("series %q = %+v, want one fine slot at %v", key, s.CPU, wantCPU)
			}
		}
		// One tick into the bucket: the partial rides as the coarse ring's
		// newest (and only) slot, so the coarse grid reaches the present.
		if s := byKey["engine"]; len(s.CoarseCPU) != 1 || s.CoarseCPU[0] != 52.0 {
			t.Errorf("engine coarse = %+v, want the partial bucket's max", s.CoarseCPU)
		}
	})
}

// TestLoadHistoryAbsenceAndLockstep proves the absence doctrine and the
// lockstep push: a failed probe advances the tick with absent slots, an entity
// whose run ended keeps taking absent slots (never a fabricated zero), and all
// live series stay end-aligned.
func TestLoadHistoryAbsenceAndLockstep(t *testing.T) {
	t.Run("load-history-absence", func(t *testing.T) {
		runs, probe := loadTestFixture()
		h := psTestLoads(runs, probe)

		// The run ends: the lane and pipeline series take absent slots.
		h.runs = fakeRunReader{}
		h.sample(context.Background())
		doc := h.snapshot()
		byKey := map[string]api.PsSeries{}
		for _, s := range doc.Series {
			byKey[s.Key] = s
		}
		if s := byKey["pipeline:load"]; len(s.CPU) != 2 || s.CPU[0] != 50 || s.CPU[1] != api.PsHistoryNoSample {
			t.Fatalf("pipeline series after the run ended = %+v, want [50, no-sample]", s.CPU)
		}
		if s := byKey["engine"]; len(s.CPU) != 2 || s.CPU[1] != 52.0 {
			t.Fatalf("engine series = %+v, want two live slots", s.CPU)
		}

		// The probe dies: every series takes an absent slot, the tick advances.
		h.probe = fakeProbe{err: errors.New("no ps binary")}
		h.sample(context.Background())
		tick, engine, groups := h.latest()
		if tick != 3 || engine != nil || len(groups) != 0 {
			t.Fatalf("after a failed probe: tick %d engine %+v groups %d, want tick 3 with no sample", tick, engine, len(groups))
		}
		for _, s := range h.snapshot().Series {
			if len(s.CPU) < 1 || s.CPU[len(s.CPU)-1] != api.PsHistoryNoSample {
				t.Errorf("series %q newest slot = %v, want no-sample on a failed probe", s.Key, s.CPU)
			}
		}
		// Lockstep: every series ends at the same tick, so the engine's length
		// (born first) bounds them all.
		if eng := byKey["engine"]; len(eng.CPU) == 0 {
			t.Fatal("engine series vanished")
		}
	})
}

// TestLoadHistoryCoarseSealAndEviction proves the tiered retention: a full
// bucket seals its per-bucket maxima into the coarse ring and resets the
// partial, and a series absent through a whole retention window is evicted
// while the engine's never is.
func TestLoadHistoryCoarseSealAndEviction(t *testing.T) {
	t.Run("load-history-coarse", func(t *testing.T) {
		runs, probe := loadTestFixture()
		h := psTestLoads(runs, probe)

		// Finish the first bucket with the run gone: 29 more absent-for-the-run
		// ticks. The engine keeps sampling.
		h.runs = fakeRunReader{}
		for range loadCoarseBucketTicks - 1 {
			h.sample(context.Background())
		}
		doc := h.snapshot()
		byKey := map[string]api.PsSeries{}
		for _, s := range doc.Series {
			byKey[s.Key] = s
		}
		// The bucket sealed exactly at the cadence: one coarse slot, no partial
		// riding on top (bucketTicks is back to zero).
		if s := byKey["engine"]; len(s.CoarseCPU) != 1 || s.CoarseCPU[0] != 52.0 {
			t.Fatalf("engine coarse after one full bucket = %+v, want [52.0]", s.CoarseCPU)
		}
		// The run sampled once in the bucket: its maximum survives the seal.
		if s := byKey["pipeline:load"]; len(s.CoarseCPU) != 1 || s.CoarseCPU[0] != 50 {
			t.Fatalf("pipeline coarse = %+v, want the bucket's max 50", s.CoarseCPU)
		}

		// Idle through the whole retention window: fine ring all absent, coarse
		// ring all absent -- the run's series evict, the engine's stays.
		ticks := loadFineRingCap + loadCoarseBucketTicks*loadCoarseRingCap
		for range ticks {
			h.sample(context.Background())
		}
		byKey = map[string]api.PsSeries{}
		for _, s := range h.snapshot().Series {
			byKey[s.Key] = s
		}
		if _, ok := byKey["pipeline:load"]; ok {
			t.Error("a series absent past the whole retention window must evict")
		}
		if _, ok := byKey["lane:ingest"]; ok {
			t.Error("the lane series must evict with its pipeline")
		}
		eng, ok := byKey["engine"]
		if !ok {
			t.Fatal("the engine series must never evict")
		}
		if len(eng.CPU) != loadFineRingCap {
			t.Errorf("engine fine ring = %d slots, want capped at %d", len(eng.CPU), loadFineRingCap)
		}
		if len(eng.CoarseCPU) != loadCoarseRingCap {
			t.Errorf("engine coarse ring = %d slots, want capped at %d", len(eng.CoarseCPU), loadCoarseRingCap)
		}
	})
}
