package daemon

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/MateusAMP2119/iris-lakehouse/internal/api"
	"github.com/MateusAMP2119/iris-lakehouse/internal/pg"
	"github.com/MateusAMP2119/iris-lakehouse/internal/store"
)

// fakeLoadPersister is an in-memory loadPersister recording writes and prunes
// and serving a scripted seed read.
type fakeLoadPersister struct {
	writes [][]pg.LoadBucket
	prunes []int64
	seed   []pg.LoadBucket
	err    error
}

func (f *fakeLoadPersister) WriteLoadBuckets(_ context.Context, _ string, buckets []pg.LoadBucket) error {
	f.writes = append(f.writes, buckets)
	return f.err
}

func (f *fakeLoadPersister) ReadLoadHistory(context.Context, string, int64) ([]pg.LoadBucket, error) {
	return f.seed, f.err
}

func (f *fakeLoadPersister) PruneLoadHistory(_ context.Context, before int64) error {
	f.prunes = append(f.prunes, before)
	return f.err
}

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

// TestLoadHistoryPersistsSeals proves the persistence path: a full bucket's
// seal writes one row per live series (values for the sampled, NULL-shaped
// absence for the unsampled), nothing writes between seals, and the retention
// prune fires on its own sparser cadence.
func TestLoadHistoryPersistsSeals(t *testing.T) {
	t.Run("load-history-persists", func(t *testing.T) {
		runs, probe := loadTestFixture()
		p := &fakeLoadPersister{}
		h := newLoadHistory(runs, func() int { return 200 }, p, nil)
		h.probe = probe
		h.pid = 100

		for range loadCoarseBucketTicks - 1 {
			h.sample(context.Background())
		}
		if len(p.writes) != 0 {
			t.Fatalf("wrote %d batches before the bucket sealed, want none", len(p.writes))
		}
		h.runs = fakeRunReader{} // the run ends just before the seal
		h.sample(context.Background())
		if len(p.writes) != 1 {
			t.Fatalf("wrote %d batches after the seal, want exactly one", len(p.writes))
		}
		byKey := map[string]pg.LoadBucket{}
		for _, b := range p.writes[0] {
			byKey[b.Series] = b
		}
		if e := byKey["engine"]; !e.Sampled || e.CPUMax != 52.0 || e.RSSMax != 50<<20 || e.Bucket == 0 {
			t.Errorf("engine bucket = %+v, want the bucket's sampled maxima with a seal time", e)
		}
		if l := byKey["pipeline:load"]; !l.Sampled || l.CPUMax != 50 {
			t.Errorf("pipeline bucket = %+v, want the run's max before it ended", l)
		}
		if len(p.prunes) != 0 {
			t.Fatalf("pruned %d times after one seal, want none yet", len(p.prunes))
		}

		// Prune fires on the sparse cadence, cut at the retention window.
		for range loadCoarseBucketTicks * (loadPersistPruneSeals - 1) {
			h.sample(context.Background())
		}
		if len(p.prunes) != 1 {
			t.Fatalf("pruned %d times after %d seals, want exactly one", len(p.prunes), loadPersistPruneSeals)
		}
		if cut := time.Now().Add(-loadPersistRetention).Unix(); p.prunes[0] > cut+60 || p.prunes[0] < cut-60 {
			t.Errorf("prune cutoff = %d, want about now minus the retention window (%d)", p.prunes[0], cut)
		}
	})
}

// TestLoadHistorySeedsFromPersisted proves the seeding read: persisted buckets
// replay into the coarse rings in order, missing steps and the downtime gap to
// now fill as absent slots, and a failed read leaves the collector blank
// rather than faulting.
func TestLoadHistorySeedsFromPersisted(t *testing.T) {
	t.Run("load-history-seeds", func(t *testing.T) {
		now := time.Now().Unix()
		p := &fakeLoadPersister{seed: []pg.LoadBucket{
			// Three minutes ago, one minute's hole, then a NULL bucket two
			// buckets before a two-minute downtime gap to now.
			{Series: "engine", Bucket: now - 5*loadBucketSeconds, CPUMax: 40, RSSMax: 1 << 20, Sampled: true},
			{Series: "engine", Bucket: now - 3*loadBucketSeconds, Sampled: false},
			{Series: "pipeline:load", Bucket: now - 3*loadBucketSeconds, CPUMax: 70, RSSMax: 2 << 20, Sampled: true},
		}}
		h := newLoadHistory(fakeRunReader{}, nil, p, nil)
		h.probe = fakeProbe{}
		h.seed(context.Background())

		h.mu.Lock()
		defer h.mu.Unlock()
		eng := h.series["engine"]
		if eng == nil {
			t.Fatal("seed built no engine series")
		}
		// 40, hole, no-sample bucket, then the downtime gap (3 slots to now).
		want := []float64{40, api.PsHistoryNoSample, api.PsHistoryNoSample, api.PsHistoryNoSample, api.PsHistoryNoSample, api.PsHistoryNoSample}
		if len(eng.coarseCPU) != len(want) {
			t.Fatalf("engine coarse = %v, want %d slots (bucket, hole, null bucket, 3-slot gap)", eng.coarseCPU, len(want))
		}
		for i, w := range want {
			if eng.coarseCPU[i] != w {
				t.Fatalf("engine coarse = %v, want %v", eng.coarseCPU, want)
			}
		}
		if lo := h.series["pipeline:load"]; lo == nil || lo.coarseCPU[0] != 70 {
			t.Errorf("pipeline series = %+v, want its persisted bucket first", lo)
		}
		if len(eng.cpu) != 0 {
			t.Errorf("seed touched the fine ring: %v", eng.cpu)
		}
	})

	t.Run("load-history-seed-failure-stays-blank", func(t *testing.T) {
		p := &fakeLoadPersister{err: errors.New("data database down")}
		h := newLoadHistory(fakeRunReader{}, nil, p, nil)
		h.probe = fakeProbe{}
		h.seed(context.Background())
		h.mu.Lock()
		defer h.mu.Unlock()
		if len(h.series) != 0 {
			t.Errorf("a failed seed built series %v, want a blank collector", h.series)
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
