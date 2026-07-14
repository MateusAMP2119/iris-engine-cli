package pg_test

import (
	"context"
	"reflect"
	"strings"
	"testing"

	"github.com/MateusAMP2119/iris-engine-cli/internal/declare"
	"github.com/MateusAMP2119/iris-engine-cli/internal/pg"
	"github.com/MateusAMP2119/iris-engine-cli/internal/pg/pgtest"
)

// The promotion tests exercise the marker-only promotion model over in-memory
// journal fixtures and the recording data-database fake: promotion flips the
// pipeline's open journal entries to undo = promoted and flips its data_mode to
// permanent, and does NOTHING else -- no table row or journal entry is copied,
// moved, or deleted, capture and provenance continue unchanged, and later wipes
// skip the released entries. The undo lifecycle types (UndoState, JournalEntry,
// Retirement, WipeTarget, PlanWipe) are the wipe model's own (wipe.go); promotion
// reuses them so the two halves of the marker lifecycle can never drift.

// promotionFixture returns the canonical two-pipeline journal the promotion
// tests share, plus the run attribution map. Pipeline "etl" (runs 101, 102) has
// three open entries -- an insert, an update with a spendable pre-image, and a
// delete with a spendable pre-image -- and one already-wiped entry (provenance
// memory, promotion must not re-arm it). Pipeline "other" (run 201) has one open
// entry promotion must leave alone.
func promotionFixture() ([]pg.JournalEntry, map[int64]string) {
	journal := []pg.JournalEntry{
		{ID: 1, RunID: 101, Schema: "analytics", Table: "orders", RowPK: "a", Op: pg.OpInsert, Undo: pg.UndoOpen},
		{ID: 2, RunID: 101, Schema: "analytics", Table: "orders", RowPK: "b", Op: pg.OpUpdate, PreImage: `{"id":"b","amount":10}`, Undo: pg.UndoOpen},
		{ID: 3, RunID: 102, Schema: "analytics", Table: "orders", RowPK: "c", Op: pg.OpDelete, PreImage: `{"id":"c","amount":20}`, Undo: pg.UndoOpen},
		{ID: 4, RunID: 201, Schema: "analytics", Table: "refunds", RowPK: "x", Op: pg.OpInsert, Undo: pg.UndoOpen},
		{ID: 5, RunID: 101, Schema: "analytics", Table: "orders", RowPK: "d", Op: pg.OpUpdate, Undo: pg.UndoWiped},
	}
	runPipeline := map[int64]string{101: "etl", 102: "etl", 201: "other"}
	return journal, runPipeline
}

// flipIDs projects a promotion plan's flips to their entry ids, in plan order.
func flipIDs(plan pg.PromotionPlan) []int64 {
	var ids []int64
	for _, f := range plan.Flips {
		ids = append(ids, f.EntryID)
	}
	return ids
}

// undoOf indexes a journal by entry id to undo state.
func undoOf(journal []pg.JournalEntry) map[int64]pg.UndoState {
	m := make(map[int64]pg.UndoState, len(journal))
	for _, e := range journal {
		m[e.ID] = e.Undo
	}
	return m
}

// TestPromoteEndsWipeEligibility proves promotion removes the pipeline's data from
// wipe eligibility and changes nothing else about the pipeline: promotion ends wipe
// eligibility only. After the promotion plan is applied, a wipe narrowed
// to the promoted pipeline finds an empty scope and plans a total no-op, while
// every other pipeline's wipe eligibility is untouched; the pipeline-level
// outcome is exactly the data_mode flip to permanent -- whose sole meaning is
// that future writes are not wipe-eligible -- and nothing more.
func TestPromoteEndsWipeEligibility(t *testing.T) {
	journal, runPipeline := promotionFixture()
	etl := pg.WipeTarget{Pipeline: "etl", RunPipeline: runPipeline}
	other := pg.WipeTarget{Pipeline: "other", RunPipeline: runPipeline}

	plan := pg.PlanPromotion(journal, "etl", runPipeline)

	// The plan releases exactly the pipeline's open entries: its wipe scope.
	if got, want := flipIDs(plan), []int64{1, 2, 3}; !reflect.DeepEqual(got, want) {
		t.Fatalf("PlanPromotion flips entry ids %v, want exactly the pipeline's open entries %v", got, want)
	}
	if plan.Promoted != 3 {
		t.Errorf("plan.Promoted = %d, want 3", plan.Promoted)
	}

	// The pipeline-level change is the data_mode flip to permanent, whose whole
	// meaning is wipe eligibility: permanent-mode writes are not wipe-eligible.
	// Capture is untouched by the mode.
	if plan.DataMode != declare.DataPermanent {
		t.Errorf("plan.DataMode = %q, want %q", plan.DataMode, declare.DataPermanent)
	}
	if pg.WipeEligible(plan.DataMode) {
		t.Errorf("WipeEligible(%q) = true, want false: promotion must end wipe eligibility for future writes", plan.DataMode)
	}

	promoted := pg.ApplyPromotion(journal, plan)

	// The promoted pipeline's data has left wipe eligibility: its wipe scope is
	// empty, and a wipe on it plans a total no-op.
	if scope := pg.WipeScope(promoted, etl); len(scope) != 0 {
		t.Errorf("after promotion, WipeScope(etl) = %v entries, want empty", len(scope))
	}
	if wipe := pg.PlanWipe(promoted, etl); len(wipe.Reverts) != 0 || len(wipe.Retirements) != 0 || len(wipe.Conflicts) != 0 {
		t.Errorf("after promotion, PlanWipe(etl) = %d reverts, %d retirements, %d conflicts; want a total no-op",
			len(wipe.Reverts), len(wipe.Retirements), len(wipe.Conflicts))
	}

	// Nothing else changed: the other pipeline's wipe eligibility is intact, and
	// the already-retired (wiped) entry was not re-armed -- promotion releases
	// open entries only.
	if scope := pg.WipeScope(promoted, other); !reflect.DeepEqual(scopeEntryIDs(scope), []int64{4}) {
		t.Errorf("after promotion, WipeScope(other) = %v, want [4]: other pipelines' eligibility must be untouched", scopeEntryIDs(scope))
	}
	if got := undoOf(promoted)[5]; got != pg.UndoWiped {
		t.Errorf("entry 5 undo = %q after promotion, want %q: promotion must not re-arm retired entries", got, pg.UndoWiped)
	}
}

// scopeEntryIDs projects a scope selection to its entry ids, in returned order.
func scopeEntryIDs(entries []pg.JournalEntry) []int64 {
	var ids []int64
	for _, e := range entries {
		ids = append(ids, e.ID)
	}
	return ids
}

// TestPromotionFlipsOpenToPromoted proves promotion flips the pipeline's open
// journal entries to undo = promoted, and subsequent wipes skip them. Proven both
// over the pure model -- apply the plan, then PlanWipe never visits the released
// entries -- and over the live statement the executor issues through the
// data-database seam: one guarded marker UPDATE, open entries of the pipeline's
// runs only.
func TestPromotionFlipsOpenToPromoted(t *testing.T) {
	journal, runPipeline := promotionFixture()

	plan := pg.PlanPromotion(journal, "etl", runPipeline)
	promoted := pg.ApplyPromotion(journal, plan)

	// Every flip in the plan retires to promoted -- promotion has exactly one
	// destination state.
	for _, f := range plan.Flips {
		if f.Undo != pg.UndoPromoted {
			t.Errorf("flip of entry %d retires to %q, want %q", f.EntryID, f.Undo, pg.UndoPromoted)
		}
	}

	// The pipeline's open entries are now promoted; every other entry keeps its
	// prior state (the other pipeline's open entry stays open, the wiped entry
	// stays wiped).
	want := map[int64]pg.UndoState{
		1: pg.UndoPromoted, 2: pg.UndoPromoted, 3: pg.UndoPromoted,
		4: pg.UndoOpen, 5: pg.UndoWiped,
	}
	if got := undoOf(promoted); !reflect.DeepEqual(got, want) {
		t.Fatalf("undo states after promotion = %v, want %v", got, want)
	}

	// Subsequent wipes skip the promoted entries: a bare wipe over the whole
	// journal visits only the other pipeline's open entry; no revert, retirement,
	// or conflict ever names a promoted entry.
	wipe := pg.PlanWipe(promoted, pg.WipeTarget{})
	for _, r := range wipe.Reverts {
		if r.EntryID != 4 {
			t.Errorf("subsequent wipe reverts entry %d, want only entry 4: promoted entries must be skipped", r.EntryID)
		}
	}
	for _, r := range wipe.Retirements {
		if r.EntryID != 4 {
			t.Errorf("subsequent wipe retires entry %d, want only entry 4: promoted entries must be skipped", r.EntryID)
		}
	}
	if wipe.Wiped != 1 || wipe.Skipped != 0 {
		t.Errorf("subsequent bare wipe = %d wiped, %d skipped; want 1 wiped, 0 skipped", wipe.Wiped, wipe.Skipped)
	}

	// The live flip is the same rule as one guarded statement: only the
	// pipeline's runs, only open entries, destination promoted.
	rec := pgtest.New()
	if err := pg.ExecutePromotionFlip(context.Background(), rec, []int64{101, 102}); err != nil {
		t.Fatalf("ExecutePromotionFlip: %v", err)
	}
	stmts := rec.Statements()
	if len(stmts) != 1 {
		t.Fatalf("live promotion issued %d statements, want exactly 1 (the marker flip): %v", len(stmts), stmts)
	}
	for _, mustContain := range []string{"public.data_journal", "undo = 'promoted'", "undo = 'open'", "run_id IN (101, 102)"} {
		if !strings.Contains(stmts[0], mustContain) {
			t.Errorf("live flip statement missing %q:\n%s", mustContain, stmts[0])
		}
	}
}

// TestPromotionNoDataMovement proves promotion mutates only undo markers and
// data_mode: it copies, moves, or deletes NO table rows or journal entries. Nothing
// is copied, moved, or deleted; promotion narrows what wipe may touch and forgets
// nothing. Proven over the pure model -- the applied journal is the same journal,
// entry for entry, with only undo flipped on the released entries -- and over the
// live executor, whose entire emission is one journal-marker UPDATE: no INSERT,
// DELETE, TRUNCATE, COPY, or DDL, and no statement against any declared table.
func TestPromotionNoDataMovement(t *testing.T) {
	journal, runPipeline := promotionFixture()
	before := make([]pg.JournalEntry, len(journal))
	copy(before, journal)

	plan := pg.PlanPromotion(journal, "etl", runPipeline)
	promoted := pg.ApplyPromotion(journal, plan)

	// The input journal is read-only: planning and applying mutate nothing in
	// place.
	if !reflect.DeepEqual(journal, before) {
		t.Fatalf("ApplyPromotion mutated its input journal in place")
	}

	// No journal entry is created, dropped, or reordered: same length, same ids,
	// same order.
	if len(promoted) != len(before) {
		t.Fatalf("promotion changed the journal length: %d entries, want %d", len(promoted), len(before))
	}
	flipped := map[int64]bool{}
	for _, f := range plan.Flips {
		flipped[f.EntryID] = true
	}
	for i := range promoted {
		got, was := promoted[i], before[i]
		// Everything but undo is identical: attribution (run), provenance key
		// (schema, table, row_pk), op, and the captured pre-image all survive --
		// promotion retains pre-images (compaction, not promotion, reclaims them
		// once released).
		gotSansUndo, wasSansUndo := got, was
		gotSansUndo.Undo, wasSansUndo.Undo = "", ""
		if !reflect.DeepEqual(gotSansUndo, wasSansUndo) {
			t.Errorf("entry %d changed beyond its undo marker:\n got %+v\nwant %+v", was.ID, got, was)
		}
		if !flipped[was.ID] && got.Undo != was.Undo {
			t.Errorf("entry %d outside the plan changed undo %q -> %q", was.ID, was.Undo, got.Undo)
		}
	}

	// The live executor's entire emission is marker mutation: exactly one
	// UPDATE on the journal's undo column, and nothing that moves data.
	rec := pgtest.New()
	if err := pg.ExecutePromotionFlip(context.Background(), rec, []int64{101, 102}); err != nil {
		t.Fatalf("ExecutePromotionFlip: %v", err)
	}
	stmts := rec.Statements()
	if len(stmts) != 1 {
		t.Fatalf("live promotion issued %d statements, want exactly 1: %v", len(stmts), stmts)
	}
	if !strings.HasPrefix(stmts[0], "UPDATE public.data_journal SET undo = 'promoted'") {
		t.Errorf("live flip is not the guarded journal-marker UPDATE:\n%s", stmts[0])
	}
	for _, forbidden := range []string{"INSERT", "DELETE", "TRUNCATE", "COPY", "CREATE", "DROP", "ALTER"} {
		if strings.Contains(strings.ToUpper(stmts[0]), forbidden) {
			t.Errorf("live flip contains %s -- promotion must move no data:\n%s", forbidden, stmts[0])
		}
	}

	// A promotion with no runs to release issues nothing at all.
	empty := pgtest.New()
	if err := pg.ExecutePromotionFlip(context.Background(), empty, nil); err != nil {
		t.Fatalf("ExecutePromotionFlip(no runs): %v", err)
	}
	if got := empty.Statements(); len(got) != 0 {
		t.Errorf("promotion of zero runs issued %d statements, want 0: %v", len(got), got)
	}
}

// TestPromoteCaptureProvenanceContinue proves capture and provenance continue
// unchanged after a pipeline's data is promoted to permanent: capture and
// provenance never stop. Post-promotion writes still classify to a stamp for
// every operation (slim, born promoted -- the permanent tier), those stamps
// append to the same journal, and a row's provenance stack reads continuously
// across the promotion boundary: the pre-promotion layers survive (attribution
// intact) and the post-promotion layers stack on top. The promotion itself
// touches no capture machinery.
func TestPromoteCaptureProvenanceContinue(t *testing.T) {
	journal, runPipeline := promotionFixture()

	plan := pg.PlanPromotion(journal, "etl", runPipeline)
	promoted := pg.ApplyPromotion(journal, plan)

	// Capture continues: under the permanent data mode every operation still
	// classifies to a stamp -- capture is unconditional, only the tier changes
	// (slim, no pre-image: stamp cost).
	for _, op := range []pg.WriteOp{pg.OpInsert, pg.OpUpdate, pg.OpDelete} {
		if got := pg.ClassifyPayloadTier(declare.DataPermanent, op); got != pg.PayloadSlim {
			t.Errorf("post-promotion %s classifies to tier %q, want %q (captured at stamp cost)", op, got, pg.PayloadSlim)
		}
	}

	// Post-promotion writes append to the same journal exactly as the trigger
	// stamps them: born promoted, slim. Row "b" -- already carrying a promoted
	// pre-promotion layer -- gains a new layer.
	afterWrites := append(append([]pg.JournalEntry(nil), promoted...),
		pg.JournalEntry{ID: 6, RunID: 103, Schema: "analytics", Table: "orders", RowPK: "b", Op: pg.OpUpdate, Undo: pg.UndoPromoted},
		pg.JournalEntry{ID: 7, RunID: 103, Schema: "analytics", Table: "orders", RowPK: "e", Op: pg.OpInsert, Undo: pg.UndoPromoted},
	)

	// Provenance continues unchanged: row "b"'s layer stack reads continuously
	// across the promotion boundary -- the pre-promotion layer (id 2, its run
	// attribution and provenance key intact) below, the post-promotion layer
	// (id 6) on top. Promotion forgot nothing and inserted no seam.
	var stack []pg.JournalEntry
	for _, e := range afterWrites {
		if e.Key() == (pg.RowKey{Schema: "analytics", Table: "orders", RowPK: "b"}) {
			stack = append(stack, e)
		}
	}
	if len(stack) != 2 || stack[0].ID != 2 || stack[1].ID != 6 {
		t.Fatalf("row b provenance stack = %+v, want the pre-promotion layer (id 2) then the post-promotion layer (id 6)", stack)
	}
	if stack[0].RunID != 101 || stack[0].Op != pg.OpUpdate {
		t.Errorf("pre-promotion layer lost attribution: %+v", stack[0])
	}

	// The promotion itself touched no capture machinery: its whole live emission
	// is the journal-marker UPDATE -- no trigger, no capture function, no
	// declared-table statement.
	rec := pgtest.New()
	if err := pg.ExecutePromotionFlip(context.Background(), rec, []int64{101, 102}); err != nil {
		t.Fatalf("ExecutePromotionFlip: %v", err)
	}
	for _, s := range rec.Statements() {
		for _, forbidden := range []string{"TRIGGER", "iris.capture", "analytics."} {
			if strings.Contains(s, forbidden) {
				t.Errorf("promotion touched capture machinery (%q):\n%s", forbidden, s)
			}
		}
	}
}
