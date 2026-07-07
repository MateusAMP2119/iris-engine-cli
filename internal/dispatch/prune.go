package dispatch

// This file is the pure count-based retention decision (specification section 6.2):
// which run rows the dispatcher's opportunistic post-pass prune targets. It is pure
// -- no I/O, no meta access -- so retention is a function of the run ids alone: keep
// the newest `retain` runs per pipeline and prune the rest, sparing any run still
// held by an outstanding dead_letters entry. The write that archives and deletes a
// pruned run is store.PruneRun; this file owns only the decision the dispatcher feeds
// it.

import "sort"

// RetentionRun is one run as the count-based retention decision sees it: its id and
// the pipeline it belongs to (specification section 6.2). Retention is count-based
// and clockless, so the run id -- meta's monotonic identity, never a clock -- orders
// a pipeline's runs newest-first, and the pipeline groups them. Nothing else about a
// run bears on whether it is prunable: no timestamp, and -- deliberately -- no
// consumer watermark, since the gate reads only an upstream's latest run and never
// pins an older one against retention (specification section 6.2: consumption and
// retention are unlinked).
type RetentionRun struct {
	// RunID is the run's meta identity (runs.id), the newest-first ordering key.
	RunID int64
	// Pipeline is the run's pipeline (runs.pipeline), the retention grouping key.
	Pipeline string
}

// SelectPrunable returns the run ids the count-based clockless retention policy
// prunes, ascending (specification section 6.2): per pipeline, every run beyond the
// newest `retain` (default 1000, resolved via --retain / IRIS_RETAIN / iris.toml at
// E01.3), EXCEPT any run still held by an outstanding dead_letters entry, which is
// spared until replay, supersession, or drain releases it. It is pure over the given
// runs: no clock is read (the run id orders newest-first, so the result is
// independent of the order runs are supplied in) and no consumer watermark is
// consulted (a run is prunable regardless of whether any downstream consumed it), so
// retention is count only.
//
// A retain of zero or less keeps no run (every run is a candidate); a pipeline with
// `retain` or fewer runs prunes none of them.
func SelectPrunable(runs []RetentionRun, retain int, outstandingDeadLetters []int64) []int64 {
	held := make(map[int64]bool, len(outstandingDeadLetters))
	for _, id := range outstandingDeadLetters {
		held[id] = true
	}

	// Group the runs by pipeline: retention keeps the newest `retain` per pipeline,
	// so each pipeline is trimmed on its own count.
	byPipeline := make(map[string][]int64)
	for _, r := range runs {
		byPipeline[r.Pipeline] = append(byPipeline[r.Pipeline], r.RunID)
	}

	keep := retain
	if keep < 0 {
		keep = 0 // a negative retain keeps nothing; every run is a candidate.
	}

	var prunable []int64
	for _, ids := range byPipeline {
		// Newest-first by run id -- meta's monotonic identity, never a clock -- so
		// the newest `keep` survive and the rest are prune candidates. Ordering by
		// id makes the decision independent of the order runs were supplied in.
		sort.Slice(ids, func(i, j int) bool { return ids[i] > ids[j] })
		if keep >= len(ids) {
			continue // pipeline within retain: nothing beyond the newest `keep`.
		}
		for _, id := range ids[keep:] {
			if held[id] {
				continue // spared: an outstanding dead_letters entry still holds it.
			}
			prunable = append(prunable, id)
		}
	}
	sort.Slice(prunable, func(i, j int) bool { return prunable[i] < prunable[j] })
	return prunable
}
