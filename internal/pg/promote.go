package pg

import (
	"context"
	"fmt"
	"sort"
	"strings"

	"github.com/MateusAMP2119/iris-engine-cli/internal/declare"
)

// This file owns promotion: the model and the live journal flip behind
// `iris pipeline promote <name>` (specification sections 1, 5 and 12). Promotion
// is the other half of the undo marker lifecycle wipe.go owns, and it is
// marker-only by construction:
//
//   - It flips the pipeline's open journal entries to undo = 'promoted', so
//     subsequent wipes skip them (S05: wipe scope is exactly the open entries;
//     promoted is provenance memory, and nothing re-arms it). The released set
//     is exactly the pipeline's wipe scope -- PlanPromotion selects it through
//     WipeScope itself, so promotion releases precisely what a wipe would have
//     touched and the two rules can never drift.
//   - Its only pipeline-level effect is the data_mode flip to permanent (the
//     per-pipeline control truth in meta; the store layer owns that column's
//     write). Permanent's whole meaning is wipe eligibility: future writes are
//     born undo = 'promoted' (capture.go), never wipe-touched.
//   - It copies, moves, and deletes NOTHING: no table row, no journal entry.
//     One store -- disposable and permanent rows live in the declared tables
//     themselves -- so promotion has nowhere to move data TO. The plan type
//     carries only undo flips; the live executor emits exactly one guarded
//     marker UPDATE. Pre-images on released entries are retained: compaction
//     (downstream, once past undo eligibility) reclaims them, promotion never
//     does.
//   - Capture and provenance continue unchanged afterward: promotion touches no
//     trigger, no capture function, no declared table. New writes are still
//     stamped -- at stamp cost now (slim, born promoted; ClassifyPayloadTier) --
//     and a row's provenance stack reads continuously across the boundary.
//
// The durability gate (promote is refused while the pipeline is un-built,
// PermanentRequiresBuilt in payload.go) is enforced by the command layer before
// any of this runs; this file models what promotion DOES, not when it is
// allowed.
//
// Sealed partitions are out of reach here exactly as they are for wipe: a
// partition seals only with zero open entries (partition.go), so the flip's
// open-guarded scope always lives entirely in unsealed partitions.

// PromotionPlan is the decided outcome of one promotion: the undo marker flips
// and the pipeline's new data mode, and deliberately nothing else -- the type
// has no revert, copy, or delete component, so a promotion plan cannot express
// data movement.
type PromotionPlan struct {
	// Pipeline is the promoted pipeline.
	Pipeline string
	// Flips are the undo transitions, one per released entry, each retiring an
	// open entry to UndoPromoted, in ascending entry-id order.
	Flips []Retirement
	// Promoted counts the released entries (len(Flips)); the summary reports it.
	Promoted int
	// DataMode is the pipeline's data mode after promotion: always permanent.
	// It is the plan's only pipeline-level effect, and its whole meaning is that
	// future writes are not wipe-eligible (WipeEligible(DataPermanent) is
	// false); capture never consults it to decide WHETHER to stamp.
	DataMode declare.DataMode
}

// PlanPromotion plans one promotion over the journal: it selects the pipeline's
// wipe scope -- exactly the entries a wipe could still touch, via WipeScope, so
// promotion releases precisely what it ends the eligibility of -- and emits one
// open -> promoted flip per entry. The journal is read-only input; the returned
// plan is the promotion's entire effect. Entries already promoted, wiped, or
// skipped are provenance memory: never selected, never re-armed. runPipeline
// attributes entries to pipelines through the run that wrote them, exactly as a
// narrowed wipe does.
func PlanPromotion(journal []JournalEntry, pipeline string, runPipeline map[int64]string) PromotionPlan {
	plan := PromotionPlan{Pipeline: pipeline, DataMode: declare.DataPermanent}
	for _, e := range WipeScope(journal, WipeTarget{Pipeline: pipeline, RunPipeline: runPipeline}) {
		plan.Flips = append(plan.Flips, Retirement{EntryID: e.ID, Undo: UndoPromoted})
		plan.Promoted++
	}
	return plan
}

// ApplyPromotion returns a copy of the journal with the plan's undo flips
// applied: the model of the journal after the promotion's transaction commits.
// Only the undo column of the flipped entries changes -- every entry survives
// with its id, attribution, provenance key, op, and pre-image intact, and no
// entry is added, dropped, or reordered (S05: promotion moves no data; it
// narrows what wipe may touch and forgets nothing).
func ApplyPromotion(journal []JournalEntry, plan PromotionPlan) []JournalEntry {
	flip := make(map[int64]UndoState, len(plan.Flips))
	for _, f := range plan.Flips {
		flip[f.EntryID] = f.Undo
	}
	out := make([]JournalEntry, len(journal))
	copy(out, journal)
	for i := range out {
		if undo, ok := flip[out[i].ID]; ok {
			out[i].Undo = undo
		}
	}
	return out
}

// RenderPromotionFlip renders the live marker-only flip for the pipeline's
// runs: one guarded UPDATE that retires the runs' open journal entries to
// promoted. The undo = 'open' guard makes it exactly the plan's rule -- it can
// only ever release wipe-scope entries, never touch wiped or skipped provenance
// memory, and re-running it is a no-op (idempotent). Run ids are deduplicated
// and sorted so the emitted text is deterministic; they are engine-minted
// bigints, inlined as integer literals. It returns "" when there is no run to
// release.
func RenderPromotionFlip(runIDs []int64) string {
	ids := dedupeSortIDs(runIDs)
	if len(ids) == 0 {
		return ""
	}
	lits := make([]string, len(ids))
	for i, id := range ids {
		lits[i] = fmt.Sprintf("%d", id)
	}
	return fmt.Sprintf(
		"UPDATE %s SET undo = 'promoted' WHERE undo = 'open' AND run_id IN (%s);",
		JournalTable().Qualified(), strings.Join(lits, ", "))
}

// ExecutePromotionFlip issues the live promotion flip through the data-database
// seam: exactly one statement (RenderPromotionFlip), or none at all when the
// pipeline has no runs to release. It is the executor's whole data-database
// footprint -- promotion mutates undo markers and nothing else there; the
// data_mode flip is a meta write the store layer owns, and the two together are
// promotion's entire effect.
func ExecutePromotionFlip(ctx context.Context, db DB, runIDs []int64) error {
	stmt := RenderPromotionFlip(runIDs)
	if stmt == "" {
		return nil
	}
	if err := db.Exec(ctx, stmt); err != nil {
		return fmt.Errorf("pg: promote journal entries: %w", err)
	}
	return nil
}

// dedupeSortIDs returns ids deduplicated and in ascending order, so rendered
// statements are deterministic regardless of caller ordering.
func dedupeSortIDs(ids []int64) []int64 {
	if len(ids) == 0 {
		return nil
	}
	seen := make(map[int64]bool, len(ids))
	out := make([]int64, 0, len(ids))
	for _, id := range ids {
		if seen[id] {
			continue
		}
		seen[id] = true
		out = append(out, id)
	}
	sort.Slice(out, func(i, j int) bool { return out[i] < out[j] })
	return out
}
