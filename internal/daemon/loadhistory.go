package daemon

import (
	"context"
	"io"
	"log/slog"
	"os"
	"sync"
	"time"

	"github.com/MateusAMP2119/iris-lakehouse/internal/api"
	"github.com/MateusAMP2119/iris-lakehouse/internal/store"
)

// This file is the daemon's load collector: the continuous sampler behind the
// ps readout's load fields and its recorded history. Every tick it takes one
// host-load probe (loadprobe.go) and one plain-MVCC run snapshot, attributes
// the sampled process groups to the engine and to each running run's lane and
// pipeline, and pushes one slot into every entity's in-memory history ring.
// The rings live in daemon memory only -- no table, no file -- so the history
// survives any number of `iris ps` clients coming and going and dies with the
// daemon; that is deliberate (memory-first, persistence can follow if daemon
// restarts prove it worth a schema).
//
// Retention is tiered: a fine ring holds one slot per tick (minutes of recent
// detail), and a coarse ring holds one slot per aggregation bucket carrying
// the bucket's MAXIMUM fine sample (hours of history where a short spike stays
// visible instead of averaging away). Every live series is pushed in lockstep
// each tick, so series of different ages align from their ends. Sampling is
// best-effort by the probe's own contract: a failed probe records an absent
// slot, never a fabricated zero.

const (
	// loadSampleInterval is the collector's tick: one probe and one run
	// snapshot per interval. Coarser than the live view's 1s poll on purpose --
	// the collector runs for the daemon's whole life, attached clients or not.
	loadSampleInterval = 2 * time.Second
	// loadFineRingCap bounds the fine ring: at the 2s tick this holds 10
	// minutes of full-resolution history.
	loadFineRingCap = 300
	// loadCoarseBucketTicks is the aggregation bucket in ticks: 30 ticks at 2s
	// seal one 60-second bucket.
	loadCoarseBucketTicks = 30
	// loadCoarseRingCap bounds the coarse ring: at 60s buckets this holds 12
	// hours of history.
	loadCoarseRingCap = 720
)

// loadSeries is one entity's recorded history: the fine ring, the coarse ring,
// and the running partial bucket (the per-bucket maxima accumulated since the
// last seal). Slots with no sample carry api.PsHistoryNoSample CPU and zero
// RSS.
type loadSeries struct {
	cpu       []float64
	rss       []int64
	coarseCPU []float64
	coarseRSS []int64
	bucketCPU float64
	bucketRSS int64
}

// newLoadSeries builds an empty series with an all-absent partial bucket.
func newLoadSeries() *loadSeries {
	return &loadSeries{bucketCPU: api.PsHistoryNoSample}
}

// push appends one tick's slot (nil for no sample) to the fine ring and folds
// it into the partial bucket's maxima.
func (s *loadSeries) push(l *api.PsLoad) {
	cpu, rss := float64(api.PsHistoryNoSample), int64(0)
	if l != nil {
		cpu, rss = l.CPUPercent, l.RSSBytes
	}
	s.cpu = append(s.cpu, cpu)
	s.rss = append(s.rss, rss)
	if len(s.cpu) > loadFineRingCap {
		s.cpu = s.cpu[len(s.cpu)-loadFineRingCap:]
		s.rss = s.rss[len(s.rss)-loadFineRingCap:]
	}
	if l != nil {
		if s.bucketCPU == api.PsHistoryNoSample || cpu > s.bucketCPU {
			s.bucketCPU = cpu
		}
		if rss > s.bucketRSS {
			s.bucketRSS = rss
		}
	}
}

// seal closes the partial bucket into the coarse ring and starts a fresh one.
// A bucket that saw no sample seals as an absent slot.
func (s *loadSeries) seal() {
	s.coarseCPU = append(s.coarseCPU, s.bucketCPU)
	s.coarseRSS = append(s.coarseRSS, s.bucketRSS)
	if len(s.coarseCPU) > loadCoarseRingCap {
		s.coarseCPU = s.coarseCPU[len(s.coarseCPU)-loadCoarseRingCap:]
		s.coarseRSS = s.coarseRSS[len(s.coarseRSS)-loadCoarseRingCap:]
	}
	s.bucketCPU, s.bucketRSS = api.PsHistoryNoSample, 0
}

// dead reports whether the series holds no sample anywhere: fine ring, coarse
// ring, and partial bucket all absent. A dead series is an entity idle past
// the whole retention window; keeping it would grow the map forever.
func (s *loadSeries) dead() bool {
	for _, c := range s.cpu {
		if c != api.PsHistoryNoSample {
			return false
		}
	}
	for _, c := range s.coarseCPU {
		if c != api.PsHistoryNoSample {
			return false
		}
	}
	return s.bucketCPU == api.PsHistoryNoSample
}

// loadHistory is the collector: the probe and run-snapshot seams it samples
// through, and the mutex-guarded state the ps plane reads (the latest sample
// for the live load fields, the series map for ?history=1). The production
// collector runs as one goroutine for the daemon's life; tests drive sample()
// directly.
type loadHistory struct {
	probe     loadProber
	runs      RunSnapshotReader
	managedPG func() int
	logger    *slog.Logger
	pid       int

	mu          sync.Mutex
	tick        uint64
	bucketTicks int
	engineLoad  *api.PsLoad
	groupLoad   map[int]*api.PsLoad
	series      map[string]*loadSeries
}

// newLoadHistory builds the collector over the run reader and the managed
// postmaster locator (nil for none), probing through the production ps(1)
// probe. A nil logger discards output.
func newLoadHistory(runs RunSnapshotReader, managedPG func() int, logger *slog.Logger) *loadHistory {
	if logger == nil {
		logger = slog.New(slog.NewTextHandler(io.Discard, nil))
	}
	if managedPG == nil {
		managedPG = func() int { return 0 }
	}
	return &loadHistory{
		probe:     psProbe{},
		runs:      runs,
		managedPG: managedPG,
		logger:    logger,
		pid:       os.Getpid(),
		series:    map[string]*loadSeries{},
	}
}

// run is the collector's goroutine: an immediate first sample (the first
// readout should not wait a full interval), then one per tick until ctx ends.
func (h *loadHistory) run(ctx context.Context) {
	h.sample(ctx)
	t := time.NewTicker(loadSampleInterval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			h.sample(ctx)
		}
	}
}

// sample takes one tick: probe the host, snapshot the runs, attribute the
// process groups (engine tree; running runs' groups summed under their lane
// and pipeline), and push one slot into every live series. Both reads are
// best-effort: a failed probe records the tick with absent slots everywhere, a
// failed run snapshot records the engine but no lane or pipeline attribution.
func (h *loadHistory) sample(ctx context.Context) {
	var engine *api.PsLoad
	groups := map[int]*api.PsLoad{}
	entity := map[string]*api.PsLoad{}
	samples, err := h.probe.Sample(ctx)
	if err != nil {
		h.logger.Debug("load collector host probe failed", "err", err)
	} else {
		for _, s := range samples {
			l := groups[s.PGID]
			if l == nil {
				l = &api.PsLoad{}
				groups[s.PGID] = l
			}
			l.CPUPercent += s.CPUPercent
			l.RSSBytes += s.RSSBytes
		}
		engine = sumTrees(samples, h.pid, h.managedPG())
		if runs, rerr := h.runs.Runs(ctx, store.RunFilter{}); rerr != nil {
			h.logger.Debug("load collector run snapshot failed", "err", rerr)
		} else {
			accumulate := func(key string, l *api.PsLoad) {
				e := entity[key]
				if e == nil {
					e = &api.PsLoad{}
					entity[key] = e
				}
				e.CPUPercent += l.CPUPercent
				e.RSSBytes += l.RSSBytes
			}
			for _, run := range runs {
				if run.State != store.RunRunning || run.Handle == 0 {
					continue
				}
				l := groups[run.Handle]
				if l == nil {
					continue
				}
				lane := run.Lane
				if lane == "" {
					lane = run.Pipeline
				}
				accumulate("lane:"+lane, l)
				accumulate("pipeline:"+run.Pipeline, l)
			}
		}
	}

	h.mu.Lock()
	defer h.mu.Unlock()
	h.tick++
	h.engineLoad = engine
	h.groupLoad = groups
	ensure := func(key string) {
		if h.series[key] == nil {
			h.series[key] = newLoadSeries()
		}
	}
	ensure("engine")
	for key := range entity {
		ensure(key)
	}
	// Lockstep: every live series takes exactly one slot per tick, so all
	// series end at this tick and align from their ends.
	for key, s := range h.series {
		if key == "engine" {
			s.push(engine)
			continue
		}
		s.push(entity[key])
	}
	h.bucketTicks++
	if h.bucketTicks >= loadCoarseBucketTicks {
		h.bucketTicks = 0
		for key, s := range h.series {
			s.seal()
			if key != "engine" && s.dead() {
				delete(h.series, key)
			}
		}
	}
}

// latest returns the newest sample: the tick counter, the engine load, and the
// per-process-group sums. The returned values are replaced wholesale each tick
// and never mutated after, so callers may hold and marshal them without
// copying -- read-only by contract. A nil collector reads as never sampled.
func (h *loadHistory) latest() (tick uint64, engine *api.PsLoad, groups map[int]*api.PsLoad) {
	if h == nil {
		return 0, nil, nil
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.tick, h.engineLoad, h.groupLoad
}

// snapshot renders the recorded history as the wire document: a deep copy of
// every series, the running partial bucket appended as the coarse ring's
// newest slot so the coarse grid reaches the present. Series order is
// unspecified (a map walk); clients key on Series.Key. A nil collector or one
// that never ticked reads as nil -- absent on the wire.
func (h *loadHistory) snapshot() *api.PsHistory {
	if h == nil {
		return nil
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.tick == 0 {
		return nil
	}
	doc := &api.PsHistory{
		FineIntervalSeconds:   int(loadSampleInterval / time.Second),
		CoarseIntervalSeconds: int(loadSampleInterval/time.Second) * loadCoarseBucketTicks,
	}
	for key, s := range h.series {
		series := api.PsSeries{
			Key:       key,
			CPU:       append([]float64(nil), s.cpu...),
			RSS:       append([]int64(nil), s.rss...),
			CoarseCPU: append([]float64(nil), s.coarseCPU...),
			CoarseRSS: append([]int64(nil), s.coarseRSS...),
		}
		if h.bucketTicks > 0 {
			series.CoarseCPU = append(series.CoarseCPU, s.bucketCPU)
			series.CoarseRSS = append(series.CoarseRSS, s.bucketRSS)
		}
		doc.Series = append(doc.Series, series)
	}
	return doc
}
