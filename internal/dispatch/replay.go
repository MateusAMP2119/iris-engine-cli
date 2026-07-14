package dispatch

// This file is the pure dead-letter replay logic: the worklist-exit model, the
// failed_upstream root walk, the self-supersession rule, and the dead-lettering
// replay signal. It is pure -- no I/O, no meta access -- so replay's decisions are a
// function of the worklist alone: which entry is a root cause, which propagated entry
// walks to which root, which propagated entry has already superseded itself, and
// whether a replay batch re-dead-lettered (the condition the replay command maps to
// exit 5). The write that mints the fresh replacement run and removes the replaced
// entry is store.ReplayRun; this file owns only the decision the dispatcher feeds it.

import (
	"fmt"
	"sort"

	"github.com/MateusAMP2119/iris-engine-cli/internal/store"
)

// WorklistExit is one of the three -- and only three -- ways a dead_letters row
// leaves the worklist: a replay that mints a replacement run, a supersession, or a
// drain. Every exit disposes of the parking row while the run row stays in runs; the
// set is closed, so a fourth disposition is a bug, never a default. Drain is
// delivered by E05.8; the model names it here so the closed exit set is asserted
// whole.
type WorklistExit int

// The worklist exit paths (a closed set).
const (
	// ExitReplay is a replay: a fresh run is minted on current data and the replaced
	// entry is removed (the replacement mints, the worklist exits).
	ExitReplay WorklistExit = iota
	// ExitSupersession is a propagated entry clearing itself once its dependent
	// consumes a later upstream run (no replay, no human).
	ExitSupersession
	// ExitDrain is a pure discard: the entry is removed, nothing re-runs, the run
	// becomes prunable (delivered by E05.8).
	ExitDrain
)

// WorklistExits returns the closed set of worklist exit paths in canonical order
// (replay, supersession, drain). It is the enumeration the exit-path invariant is
// asserted over.
func WorklistExits() []WorklistExit {
	return []WorklistExit{ExitReplay, ExitSupersession, ExitDrain}
}

// valid reports whether e is one of the three defined exit paths.
func (e WorklistExit) valid() bool {
	return e == ExitReplay || e == ExitSupersession || e == ExitDrain
}

// String renders the exit path as its worklist-disposition token.
func (e WorklistExit) String() string {
	switch e {
	case ExitReplay:
		return "replay"
	case ExitSupersession:
		return "supersession"
	case ExitDrain:
		return "drain"
	default:
		return "unknown"
	}
}

// RemovesWorklistEntry reports that this exit path clears the run's dead_letters
// row. Every exit path removes the entry: a worklist exit is a disposition of the
// parking row.
func (e WorklistExit) RemovesWorklistEntry() bool { return e.valid() }

// RetainsRunRow reports that this exit path leaves the run row in runs. Every exit
// path retains it: disposing of a worklist entry never deletes run history (the run
// stays in runs, its summary outlives pruning).
func (e WorklistExit) RetainsRunRow() bool { return e.valid() }

// DeadLetterEntry is one worklist row as replay resolution sees it: the dead-lettered
// run, its pipeline, its reason, and -- for a propagated entry -- the immediate
// upstream dead-lettered run it propagated from (derived from run_inputs; zero for a
// root cause). It carries only what the root walk and the supersession rule need; the
// write path reads the full rows.
type DeadLetterEntry struct {
	// RunID is the dead-lettered run parked by this entry (dead_letters.run_id).
	RunID int64
	// Pipeline is the run's pipeline (runs.pipeline).
	Pipeline string
	// Reason is why the run dead-lettered: failed or stopped (a root cause) or
	// upstream_dead_lettered (a propagated entry).
	Reason store.DeadLetterReason
	// FailedUpstreamRunID is the immediate upstream dead-lettered run this entry
	// propagated from, for a propagated entry (the poisoned run recorded in
	// run_inputs). It is zero for a root cause, which propagated from nothing.
	FailedUpstreamRunID int64
}

// IsRootCause reports whether the entry is a root cause -- a run that failed or was
// stopped on its own -- and so demands operator disposition. Only root causes are
// replay targets; a propagated entry walks to one.
func (e DeadLetterEntry) IsRootCause() bool {
	return e.Reason == store.ReasonFailed || e.Reason == store.ReasonStopped
}

// IsPropagated reports whether the entry is a propagated rejection
// (upstream_dead_lettered): it walks failed_upstream to a root and self-supersedes
// once its dependent consumes a later upstream run.
func (e DeadLetterEntry) IsPropagated() bool {
	return e.Reason == store.ReasonUpstreamDeadLettered
}

// ResolveReplayTargets walks each selected worklist entry along its failed_upstream
// chain to its root cause and returns the distinct root run ids to replay, ascending
// (replay targets root causes; propagated entries walk failed_upstream to the root;
// --pipeline/--all collapse to roots). selected is the set of dead-lettered run ids
// the scope named: one for <run>, a pipeline's entries for --pipeline, the whole
// worklist for --all. It is pure over the worklist.
//
// It fails loudly rather than replaying the wrong thing: a selected run absent from
// the worklist, a propagated entry with no recorded upstream run, a chain pointing at
// an absent upstream run, or a failed_upstream cycle each return an error, so a
// propagated entry is never replayed as though it were a root and a malformed chain
// never loops forever.
func ResolveReplayTargets(worklist []DeadLetterEntry, selected []int64) ([]int64, error) {
	byRun := make(map[int64]DeadLetterEntry, len(worklist))
	for _, e := range worklist {
		byRun[e.RunID] = e
	}

	roots := make(map[int64]bool)
	for _, sel := range selected {
		root, err := walkToRoot(byRun, sel)
		if err != nil {
			return nil, err
		}
		roots[root] = true
	}

	out := make([]int64, 0, len(roots))
	for r := range roots {
		out = append(out, r)
	}
	sort.Slice(out, func(i, j int) bool { return out[i] < out[j] })
	return out, nil
}

// walkToRoot follows a worklist entry's failed_upstream chain to the run that
// actually failed or was stopped. A visited set bounds the walk so a malformed cycle
// errors rather than looping; a dangling or missing link errors rather than silently
// treating a propagated entry as a root.
func walkToRoot(byRun map[int64]DeadLetterEntry, start int64) (int64, error) {
	visited := make(map[int64]bool)
	cur := start
	for {
		if visited[cur] {
			return 0, fmt.Errorf("dispatch: replay resolve: failed_upstream cycle at run %d", cur)
		}
		visited[cur] = true

		entry, ok := byRun[cur]
		if !ok {
			return 0, fmt.Errorf("dispatch: replay resolve: run %d has no worklist entry", cur)
		}
		if entry.IsRootCause() {
			return entry.RunID, nil
		}
		// A propagated entry must name the immediate upstream run it propagated from,
		// and that run must itself be parked (it is dead-lettered): otherwise the chain
		// is broken and we cannot honestly resolve a root.
		if entry.FailedUpstreamRunID == 0 {
			return 0, fmt.Errorf("dispatch: replay resolve: propagated run %d records no failed_upstream run", cur)
		}
		cur = entry.FailedUpstreamRunID
	}
}

// SupersededByLaterConsumption reports whether a propagated dead-letter entry has
// superseded itself: its dependent has consumed an upstream run strictly later than
// the poisoned upstream run the entry recorded, so the entry clears itself with no
// replay and no human ("propagated entries clear themselves: superseded once their
// dependent consumes a later upstream run; only root causes demand a human"). A root
// cause (failed, stopped) never self-supersedes: it always requires operator
// disposition, so this returns false for it regardless of consumption.
func SupersededByLaterConsumption(entry DeadLetterEntry, consumedUpstreamRunID int64) bool {
	if !entry.IsPropagated() {
		return false
	}
	return consumedUpstreamRunID > entry.FailedUpstreamRunID
}

// ReplayResult is the outcome of replaying one resolved root cause: the replaced
// dead-lettered run (the replay ticket), the fresh replacement run minted on current
// data, the replacement's replayed_from (the replaced run -- replay lineage, never
// parenthood), and whether the replacement itself dead-lettered again. A
// dead-lettering replacement parks a fresh worklist entry on the replacement run,
// still chained to the original through replayed_from.
type ReplayResult struct {
	// ReplacedRunID is the dead-lettered run this replay replaced (its worklist entry
	// was removed when the replacement minted).
	ReplacedRunID int64
	// ReplacementRunID is the fresh run minted on current data (cause replay).
	ReplacementRunID int64
	// ReplayedFrom is the replacement's runs.replayed_from: the replaced run, so a
	// re-dead-lettering replacement's new entry chains back through replay lineage.
	ReplayedFrom int64
	// DeadLettered reports whether the replacement run itself dead-lettered again.
	DeadLettered bool
}

// ReplayDeadLettered reports whether any replay in the batch re-dead-lettered: the
// condition the replay command maps to exit 5 (a dead-lettering replay parks a fresh
// entry chained via replayed_from and exits 5). A batch with no re-dead-letter is a
// clean replay (exit 0).
func ReplayDeadLettered(results []ReplayResult) bool {
	for _, r := range results {
		if r.DeadLettered {
			return true
		}
	}
	return false
}

// BlastClass is a downstream's classification in the blast radius of a dead-lettered
// run (for `iris deadletter show`). Closed set.
type BlastClass string

const (
	BlastPoisonedNow BlastClass = "poisoned_now"
	BlastPending     BlastClass = "pending"
	BlastShielded    BlastClass = "shielded"
	BlastUntouched   BlastClass = "untouched"
)

// BlastImpact is one pipeline's classification under a dead letter's blast.
type BlastImpact struct {
	Pipeline string
	Class    BlastClass
}

// BlastEdge is one depends_on edge as blast classification reads it: the dependent
// pipeline (the one that declares depends_on) and the upstream it depends on. It is
// the reverse of the gate's Edge -- the gate resolves a dependent against its own
// upstreams, while the blast radius walks FORWARD from a root cause to its transitive
// dependents, so it needs both endpoints named. Composer `order` is not a dependency
// and mints no BlastEdge: a lane neighbor reachable only by composer order is
// untouched.
type BlastEdge struct {
	// Dependent is the pipeline that declares depends_on (dependencies.from_pipeline).
	Dependent string
	// Upstream is the pipeline it depends on (dependencies.to_pipeline).
	Upstream string
}

// ClassifyBlastRadius classifies the blast radius of a dead-lettered run for `iris
// deadletter show` ("one entry: reason, error, failed_upstream, blast radius (root
// cause; poisoned_now / pending / shielded)").
//
// It walks the seed entry along failed_upstream to its ROOT cause, then walks FORWARD
// over the depends_on edges to every transitive dependent of the root, and classifies
// each:
//
//   - poisoned_now: the dependent currently carries a propagated worklist entry that
//     itself walks failed_upstream back to this root (the rejection has landed).
//   - shielded: the dependent has since consumed a later upstream success, so its
//     propagated entry is superseded (shielded[pipeline] is true).
//   - pending: the dependent is owed the failure but has not yet run to receive it --
//     no worklist entry yet and not shielded.
//
// The root cause itself is reported poisoned_now (its own run is dead-lettered). Lane
// members reachable only by composer order -- no depends_on path from the root -- are
// untouched: order is not dependency. Impacts are returned deterministically: the root
// first, then dependents in name order, then untouched neighbors in name order; each
// pipeline appears once. It is pure over its inputs (no I/O): the caller supplies the
// worklist, the edges, the root's lane members, and the shielded set.
func ClassifyBlastRadius(seed DeadLetterEntry, worklist []DeadLetterEntry, edges []BlastEdge, laneMembers []string, shielded map[string]bool) ([]BlastImpact, error) {
	by := make(map[int64]DeadLetterEntry, len(worklist))
	for _, e := range worklist {
		by[e.RunID] = e
	}
	rootRunID, err := walkToRoot(by, seed.RunID)
	if err != nil {
		return nil, fmt.Errorf("dispatch: blast radius: %w", err)
	}
	rootPipeline := by[rootRunID].Pipeline

	// Forward adjacency: upstream -> its immediate dependents.
	dependentsOf := make(map[string][]string, len(edges))
	for _, e := range edges {
		dependentsOf[e.Upstream] = append(dependentsOf[e.Upstream], e.Dependent)
	}

	// BFS forward from the root over depends_on to the transitive dependents.
	downstream := make(map[string]bool)
	queue := []string{rootPipeline}
	for len(queue) > 0 {
		cur := queue[0]
		queue = queue[1:]
		for _, dep := range dependentsOf[cur] {
			if dep == rootPipeline || downstream[dep] {
				continue
			}
			downstream[dep] = true
			queue = append(queue, dep)
		}
	}

	// A pipeline is poisoned_now when it carries a propagated worklist entry that walks
	// failed_upstream back to this exact root.
	poisonedNow := make(map[string]bool)
	for _, e := range worklist {
		if !e.IsPropagated() {
			continue
		}
		if r, werr := walkToRoot(by, e.RunID); werr == nil && r == rootRunID {
			poisonedNow[e.Pipeline] = true
		}
	}

	seen := make(map[string]bool)
	out := make([]BlastImpact, 0, len(downstream)+len(laneMembers)+1)

	// The root cause first: its own run is dead-lettered, so poisoned_now.
	out = append(out, BlastImpact{Pipeline: rootPipeline, Class: BlastPoisonedNow})
	seen[rootPipeline] = true

	// Transitive dependents, in name order.
	deps := make([]string, 0, len(downstream))
	for d := range downstream {
		deps = append(deps, d)
	}
	sort.Strings(deps)
	for _, d := range deps {
		var class BlastClass
		switch {
		case poisonedNow[d]:
			class = BlastPoisonedNow
		case shielded[d]:
			class = BlastShielded
		default:
			class = BlastPending
		}
		out = append(out, BlastImpact{Pipeline: d, Class: class})
		seen[d] = true
	}

	// Lane neighbors reachable only by composer order (not dependents): untouched.
	untouched := make([]string, 0, len(laneMembers))
	for _, m := range laneMembers {
		if !seen[m] {
			untouched = append(untouched, m)
		}
	}
	sort.Strings(untouched)
	for _, m := range untouched {
		if seen[m] {
			continue
		}
		out = append(out, BlastImpact{Pipeline: m, Class: BlastUntouched})
		seen[m] = true
	}

	return out, nil
}
