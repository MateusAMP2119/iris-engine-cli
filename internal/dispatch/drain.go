package dispatch

// This file is the pure dead-letter drain logic (specification sections 6.2 and
// 12): scope resolution only, no I/O. Drain is a pure discard -- it disposes of
// exactly the entries the operator's scope names, and it never walks
// failed_upstream to a root the way replay does: a propagated entry is discarded as
// itself, not collapsed to the cause it propagated from (replay targets root
// causes; drain targets whatever the scope names). The write that deletes the
// resolved entries' dead_letters rows is store.DrainDeadLetters; this file owns
// only the decision of which run ids that write targets.

import (
	"fmt"
	"sort"
)

// DrainScope is the operator scope drain resolves against the worklist
// (specification sections 6.2, 8, and 12): exactly one of a single run, one
// pipeline's outstanding entries, or every outstanding entry. The CLI refuses a
// bare invocation before a scope ever reaches resolution
// (S12/drain-requires-explicit-scope, S06.2/replay-drain-bare-usage-error); the
// zero value is not a valid scope here either -- resolution refuses it rather than
// defaulting to --all.
type DrainScope struct {
	// Run is the single dead-lettered run to drain, for the <run> form. Zero
	// unless that form was named.
	Run int64
	// Pipeline scopes to one pipeline's outstanding entries (--pipeline).
	Pipeline string
	// All scopes to every outstanding entry (--all).
	All bool
}

// ResolveDrainTargets resolves scope against worklist to the exact set of
// dead_letters run ids drain will discard, ascending (specification sections 6.2
// and 12): <run> resolves to that one entry, --pipeline to every outstanding entry
// for that pipeline, --all to every outstanding entry -- scoped-only, no others
// touched. Unlike ResolveReplayTargets, it never walks failed_upstream to a root: a
// propagated entry named by <run> is itself the drain target, discarded as named,
// never collapsed to the cause it propagated from. A --pipeline naming no
// outstanding entries resolves to none (a legitimate empty scope, not an error); a
// <run> naming an entry absent from the worklist fails loudly instead of silently
// draining nothing, and a scope naming none of <run>/--pipeline/--all fails loudly
// instead of silently draining everything.
func ResolveDrainTargets(worklist []DeadLetterEntry, scope DrainScope) ([]int64, error) {
	switch {
	case scope.All:
		return allRunIDs(worklist), nil
	case scope.Pipeline != "":
		return runIDsForPipeline(worklist, scope.Pipeline), nil
	case scope.Run != 0:
		for _, e := range worklist {
			if e.RunID == scope.Run {
				return []int64{scope.Run}, nil
			}
		}
		return nil, fmt.Errorf("dispatch: drain resolve: run %d has no worklist entry", scope.Run)
	default:
		return nil, fmt.Errorf("dispatch: drain resolve: scope names none of <run>, --pipeline, or --all")
	}
}

// allRunIDs returns every worklist entry's run id, ascending.
func allRunIDs(worklist []DeadLetterEntry) []int64 {
	out := make([]int64, 0, len(worklist))
	for _, e := range worklist {
		out = append(out, e.RunID)
	}
	sort.Slice(out, func(i, j int) bool { return out[i] < out[j] })
	return out
}

// runIDsForPipeline returns the run ids of worklist entries belonging to pipeline,
// ascending, and no others: a --pipeline scope never reaches beyond its own
// pipeline's outstanding entries.
func runIDsForPipeline(worklist []DeadLetterEntry, pipeline string) []int64 {
	var out []int64
	for _, e := range worklist {
		if e.Pipeline == pipeline {
			out = append(out, e.RunID)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i] < out[j] })
	return out
}

// RemoveDrained returns worklist with the drained run ids' entries removed,
// preserving the order of what remains: the pure-logic shadow of the atomic delete
// store.DrainDeadLetters performs. It exists so a post-drain worklist can be fed
// back into ResolveReplayTargets, proving that a drained run's entry -- its only
// replay ticket -- is gone, so it can never be replayed again (specification
// sections 6.2 and 12: "drained runs can never be replayed -- the entry is the
// replay ticket, deliberately").
func RemoveDrained(worklist []DeadLetterEntry, drained []int64) []DeadLetterEntry {
	if len(drained) == 0 {
		return worklist
	}
	gone := make(map[int64]bool, len(drained))
	for _, id := range drained {
		gone[id] = true
	}
	out := make([]DeadLetterEntry, 0, len(worklist))
	for _, e := range worklist {
		if !gone[e.RunID] {
			out = append(out, e)
		}
	}
	return out
}
