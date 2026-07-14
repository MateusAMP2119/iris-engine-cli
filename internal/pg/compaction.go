package pg

import "sort"

// This file is the pure model of the seal-time journal compaction rule: the
// clockless, count-based transformation a seal applies to a partition's rows before
// it checkpoints them. CompactJournalRange
// (live.go) is the SQL realization of exactly this rule against a sealed id range;
// CompactEntries is the same rule expressed as pure Go so the collapse contract is
// unit-testable without a live database.
//
// The rule has two halves, and one invariant:
//
//   - Pre-images past undo eligibility are nulled. An entry keeps its pre-image
//     only while it is still undo-open (a wipe could still replay it); once past
//     undo (promoted, wiped, or skipped) the pre-image can never be replayed, so it
//     is dropped. A sealed partition is immutable by construction and holds no
//     undo-open entries, so the SQL realization nulls every pre-image in the sealed
//     range -- a special case of this rule.
//   - Duplicate stamps per (schema, table, row_pk, run_id) collapse to the latest
//     op: within one such key, only the highest-id entry survives (the last write
//     that run made to that row), carrying its op.
//   - Invariant: each run's exact write set survives. Every distinct
//     (schema, table, row_pk, run_id) key that had any entry still has exactly one
//     after compaction, so provenance returns the same per-run write set -- lineage
//     is never lost, only redundant intermediate stamps and un-replayable
//     pre-images are dropped.

// CompactEntries applies the seal-time compaction collapse rule to entries and
// returns the compacted set in id order: pre-images past undo eligibility nulled,
// duplicate stamps per (schema, table, row_pk, run_id) folded to the latest op
// (highest id), with each key's exact surviving write preserved. It does not mutate
// the input slice, so a caller may reuse it.
func CompactEntries(entries []JournalEntry) []JournalEntry {
	type key struct {
		schema, table, rowPK string
		runID                int64
	}
	// Keep the highest-id entry per provenance key: the latest op that run made to
	// that row (the surviving stamp).
	latest := make(map[key]JournalEntry, len(entries))
	for _, e := range entries {
		k := key{e.Schema, e.Table, e.RowPK, e.RunID}
		if cur, ok := latest[k]; !ok || e.ID > cur.ID {
			latest[k] = e
		}
	}

	out := make([]JournalEntry, 0, len(latest))
	for _, e := range latest {
		// Pre-images past undo eligibility are un-replayable and are dropped; an
		// entry still undo-open keeps its image.
		if e.Undo != UndoOpen {
			e.PreImage = ""
		}
		out = append(out, e)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out
}
