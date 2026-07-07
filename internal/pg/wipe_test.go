package pg_test

import (
	"reflect"
	"testing"

	"github.com/MateusAMP2119/iris-engine-cli/internal/pg"
)

// The wipe unit tests exercise the pure wipe model over in-memory journal
// fixtures (specification sections 5, 6.3 and 14): scope selection of open
// disposable entries, reverse-order replay, the journal-internal conflict-skip
// rule, retirement of every visited entry, pipeline narrowing, and the
// cursor-with-data revert. No Postgres, no SQL: the plan is data; a later task
// executes it live.

// wholeScope is the bare-invocation target: no pipeline narrowing, the whole
// wipe scope.
var wholeScope = pg.WipeTarget{}

// applyRetirements returns a copy of journal with each retired entry's undo
// column flipped to its retirement state, modeling the journal after the wipe's
// transaction commits.
func applyRetirements(journal []pg.JournalEntry, retirements []pg.Retirement) []pg.JournalEntry {
	undoByID := make(map[int64]pg.UndoState, len(retirements))
	for _, r := range retirements {
		undoByID[r.EntryID] = r.Undo
	}
	out := make([]pg.JournalEntry, len(journal))
	copy(out, journal)
	for i := range out {
		if undo, ok := undoByID[out[i].ID]; ok {
			out[i].Undo = undo
		}
	}
	return out
}

// scopeIDs projects a scope selection to its entry ids, in returned order.
func scopeIDs(entries []pg.JournalEntry) []int64 {
	var ids []int64
	for _, e := range entries {
		ids = append(ids, e.ID)
	}
	return ids
}

// revertEntryIDs projects a plan's reverts to their entry ids, in replay order.
func revertEntryIDs(plan pg.WipePlan) []int64 {
	var ids []int64
	for _, r := range plan.Reverts {
		ids = append(ids, r.EntryID)
	}
	return ids
}

// spec: S05/wipe-scope-rule
//
// Wipe scope selects exactly the journal entries written under disposable
// data_mode that remain undo = open. Promotion flips a disposable pipeline's
// open entries to promoted and permanent-mode writes are born promoted, so
// undo = open is precisely "written under disposable data_mode, unreleased by
// promotion". Promoted, wiped, and skipped entries are provenance memory only:
// excluded from scope, and nothing re-arms them.
func TestWipeScopeRule(t *testing.T) {
	journal := []pg.JournalEntry{
		{ID: 1, RunID: 40, Schema: "analytics", Table: "orders", RowPK: "a", Op: pg.OpInsert, Undo: pg.UndoOpen},
		{ID: 2, RunID: 50, Schema: "analytics", Table: "orders", RowPK: "b", Op: pg.OpUpdate, Undo: pg.UndoPromoted},
		{ID: 3, RunID: 41, Schema: "analytics", Table: "orders", RowPK: "c", Op: pg.OpDelete, Undo: pg.UndoWiped},
		{ID: 4, RunID: 42, Schema: "analytics", Table: "orders", RowPK: "d", Op: pg.OpUpdate, Undo: pg.UndoSkipped},
		{ID: 5, RunID: 43, Schema: "analytics", Table: "orders", RowPK: "e", Op: pg.OpUpdate, PreImage: `{"id":"e","amount":1}`, Undo: pg.UndoOpen},
	}

	scope := pg.WipeScope(journal, wholeScope)
	if got, want := scopeIDs(scope), []int64{1, 5}; !reflect.DeepEqual(got, want) {
		t.Fatalf("WipeScope selected entry ids %v, want exactly the open entries %v (promoted, wiped and skipped are provenance memory only)", got, want)
	}

	// Nothing re-arms a retired entry: after the wipe retires the whole scope,
	// a fresh scope selection over the retired journal is empty. There is no
	// operation in the model that flips promoted, wiped, or skipped back to
	// open.
	plan := pg.PlanWipe(journal, wholeScope)
	retired := applyRetirements(journal, plan.Retirements)
	if rescope := pg.WipeScope(retired, wholeScope); len(rescope) != 0 {
		t.Fatalf("WipeScope after retirement selected %v, want empty: no command re-arms promoted or wiped entries", scopeIDs(rescope))
	}
}

// spec: S05/wipe-reverse-replay
//
// Wipe replays wipe-scope journal entries in reverse id order, deleting
// disposable inserts and restoring pre-images for updates and deletes. Reverse
// order is the mechanism that unwinds same-row stacking inside the scope: a row
// inserted then updated by disposable runs first has its update's pre-image
// restored, then the insert deleted, leaving no disposable residue.
func TestWipeReverseReplay(t *testing.T) {
	journal := []pg.JournalEntry{
		{ID: 1, RunID: 42, Schema: "analytics", Table: "orders", RowPK: "r1", Op: pg.OpInsert, Undo: pg.UndoOpen},
		{ID: 2, RunID: 42, Schema: "analytics", Table: "orders", RowPK: "r2", Op: pg.OpUpdate, PreImage: `{"id":"r2","amount":1}`, Undo: pg.UndoOpen},
		{ID: 3, RunID: 42, Schema: "analytics", Table: "orders", RowPK: "r3", Op: pg.OpDelete, PreImage: `{"id":"r3","amount":2}`, Undo: pg.UndoOpen},
		// Same row as the insert at id 1: stacked writes inside one scope.
		{ID: 4, RunID: 42, Schema: "analytics", Table: "orders", RowPK: "r1", Op: pg.OpUpdate, PreImage: `{"id":"r1","amount":0}`, Undo: pg.UndoOpen},
	}

	plan := pg.PlanWipe(journal, wholeScope)

	if got, want := revertEntryIDs(plan), []int64{4, 3, 2, 1}; !reflect.DeepEqual(got, want) {
		t.Fatalf("replay visited entry ids %v, want reverse id order %v", got, want)
	}
	if plan.Skipped != 0 {
		t.Fatalf("plan skipped %d entries, want 0: stacked writes inside one wipe scope unwind by reverse order, never conflict-skip", plan.Skipped)
	}

	wantReverts := []pg.RowRevert{
		{EntryID: 4, Row: pg.RowKey{Schema: "analytics", Table: "orders", RowPK: "r1"}, Op: pg.OpUpdate, Kind: pg.RevertRestorePreImage, PreImage: `{"id":"r1","amount":0}`},
		{EntryID: 3, Row: pg.RowKey{Schema: "analytics", Table: "orders", RowPK: "r3"}, Op: pg.OpDelete, Kind: pg.RevertRestorePreImage, PreImage: `{"id":"r3","amount":2}`},
		{EntryID: 2, Row: pg.RowKey{Schema: "analytics", Table: "orders", RowPK: "r2"}, Op: pg.OpUpdate, Kind: pg.RevertRestorePreImage, PreImage: `{"id":"r2","amount":1}`},
		{EntryID: 1, Row: pg.RowKey{Schema: "analytics", Table: "orders", RowPK: "r1"}, Op: pg.OpInsert, Kind: pg.RevertDeleteRow},
	}
	if !reflect.DeepEqual(plan.Reverts, wantReverts) {
		t.Fatalf("plan reverts = %+v,\nwant %+v (delete disposable inserts, restore pre-images for updates and deletes)", plan.Reverts, wantReverts)
	}
}

// spec: S05/wipe-conflict-skip
//
// An open entry is conflict-skipped, its row left as-is, whenever any later
// journal entry exists for the same (schema, table, row_pk) whose write is
// still in the row's value -- promoted, skipped, or open outside the wipe's
// scope. There is no image comparison anywhere: the decision consumes only
// journal rows (the model's API never receives a row image), and the report
// names the conflicting run. A later wiped entry does not conflict: its write
// was already reverted out of the row's value.
func TestWipeConflictSkip(t *testing.T) {
	row := pg.RowKey{Schema: "analytics", Table: "orders", RowPK: "x"}
	tests := []struct {
		name    string
		journal []pg.JournalEntry
		target  pg.WipeTarget
		// wantConflict is the expected single conflict; nil means the open
		// entry at id 1 must revert instead.
		wantConflict *pg.Conflict
	}{
		{
			name: "later promoted entry conflicts",
			journal: []pg.JournalEntry{
				{ID: 1, RunID: 40, Schema: "analytics", Table: "orders", RowPK: "x", Op: pg.OpUpdate, PreImage: `{"id":"x","amount":1}`, Undo: pg.UndoOpen},
				{ID: 2, RunID: 50, Schema: "analytics", Table: "orders", RowPK: "x", Op: pg.OpUpdate, Undo: pg.UndoPromoted},
			},
			target:       wholeScope,
			wantConflict: &pg.Conflict{EntryID: 1, RunID: 40, Row: row, ConflictingEntryID: 2, ConflictingRunID: 50},
		},
		{
			name: "later skipped entry conflicts: a skipped write is still in the row's value",
			journal: []pg.JournalEntry{
				{ID: 1, RunID: 40, Schema: "analytics", Table: "orders", RowPK: "x", Op: pg.OpUpdate, PreImage: `{"id":"x","amount":1}`, Undo: pg.UndoOpen},
				{ID: 2, RunID: 51, Schema: "analytics", Table: "orders", RowPK: "x", Op: pg.OpUpdate, Undo: pg.UndoSkipped},
			},
			target:       wholeScope,
			wantConflict: &pg.Conflict{EntryID: 1, RunID: 40, Row: row, ConflictingEntryID: 2, ConflictingRunID: 51},
		},
		{
			name: "later open entry outside a narrowed scope conflicts",
			journal: []pg.JournalEntry{
				{ID: 1, RunID: 40, Schema: "analytics", Table: "orders", RowPK: "x", Op: pg.OpUpdate, PreImage: `{"id":"x","amount":1}`, Undo: pg.UndoOpen},
				{ID: 2, RunID: 41, Schema: "analytics", Table: "orders", RowPK: "x", Op: pg.OpUpdate, PreImage: `{"id":"x","amount":2}`, Undo: pg.UndoOpen},
			},
			target: pg.WipeTarget{
				Pipeline:    "pipeline_a",
				RunPipeline: map[int64]string{40: "pipeline_a", 41: "pipeline_b"},
			},
			wantConflict: &pg.Conflict{EntryID: 1, RunID: 40, Row: row, ConflictingEntryID: 2, ConflictingRunID: 41},
		},
		{
			name: "nearest later still-in-value entry names the conflicting run",
			journal: []pg.JournalEntry{
				{ID: 1, RunID: 40, Schema: "analytics", Table: "orders", RowPK: "x", Op: pg.OpUpdate, PreImage: `{"id":"x","amount":1}`, Undo: pg.UndoOpen},
				{ID: 3, RunID: 51, Schema: "analytics", Table: "orders", RowPK: "x", Op: pg.OpUpdate, Undo: pg.UndoSkipped},
				{ID: 5, RunID: 52, Schema: "analytics", Table: "orders", RowPK: "x", Op: pg.OpUpdate, Undo: pg.UndoPromoted},
			},
			target:       wholeScope,
			wantConflict: &pg.Conflict{EntryID: 1, RunID: 40, Row: row, ConflictingEntryID: 3, ConflictingRunID: 51},
		},
		{
			name: "later wiped entry does not conflict: its write left the row's value",
			journal: []pg.JournalEntry{
				{ID: 1, RunID: 40, Schema: "analytics", Table: "orders", RowPK: "x", Op: pg.OpUpdate, PreImage: `{"id":"x","amount":1}`, Undo: pg.UndoOpen},
				{ID: 2, RunID: 52, Schema: "analytics", Table: "orders", RowPK: "x", Op: pg.OpUpdate, Undo: pg.UndoWiped},
			},
			target:       wholeScope,
			wantConflict: nil,
		},
		{
			name: "later entry for a different row never conflicts",
			journal: []pg.JournalEntry{
				{ID: 1, RunID: 40, Schema: "analytics", Table: "orders", RowPK: "x", Op: pg.OpUpdate, PreImage: `{"id":"x","amount":1}`, Undo: pg.UndoOpen},
				{ID: 2, RunID: 50, Schema: "analytics", Table: "orders", RowPK: "y", Op: pg.OpUpdate, Undo: pg.UndoPromoted},
			},
			target:       wholeScope,
			wantConflict: nil,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			plan := pg.PlanWipe(tt.journal, tt.target)
			if tt.wantConflict == nil {
				if len(plan.Conflicts) != 0 {
					t.Fatalf("plan conflicts = %+v, want none", plan.Conflicts)
				}
				if got, want := revertEntryIDs(plan), []int64{1}; !reflect.DeepEqual(got, want) {
					t.Fatalf("plan reverted entry ids %v, want %v", got, want)
				}
				return
			}
			if got, want := plan.Conflicts, []pg.Conflict{*tt.wantConflict}; !reflect.DeepEqual(got, want) {
				t.Fatalf("plan conflicts = %+v, want %+v (report names the conflicting run)", got, want)
			}
			// The conflicted row is left as-is: no revert touches it.
			for _, r := range plan.Reverts {
				if r.EntryID == tt.wantConflict.EntryID {
					t.Fatalf("conflict-skipped entry %d also appears in reverts %+v: its row must be left as-is", r.EntryID, plan.Reverts)
				}
			}
		})
	}
}

// spec: S05/wipe-retires-all-visited
//
// Wipe retires every visited open entry: reverted ones to undo = wiped,
// conflict-skipped ones to undo = skipped. The split keeps provenance decidable
// (a skipped write is still in the row's value, a wiped one is not) and means
// conflicts are reported once and never re-visited: a retired entry leaves the
// wipe scope, so a second wipe finds nothing and reports nothing. The summary
// reports both counts.
func TestWipeRetiresAllVisited(t *testing.T) {
	journal := []pg.JournalEntry{
		{ID: 1, RunID: 40, Schema: "analytics", Table: "orders", RowPK: "a", Op: pg.OpInsert, Undo: pg.UndoOpen},
		{ID: 2, RunID: 40, Schema: "analytics", Table: "orders", RowPK: "b", Op: pg.OpUpdate, PreImage: `{"id":"b","amount":1}`, Undo: pg.UndoOpen},
		// A later promoted write to row b: entry 2 must conflict-skip.
		{ID: 3, RunID: 50, Schema: "analytics", Table: "orders", RowPK: "b", Op: pg.OpUpdate, Undo: pg.UndoPromoted},
	}

	plan := pg.PlanWipe(journal, wholeScope)

	wantRetirements := []pg.Retirement{
		{EntryID: 2, Undo: pg.UndoSkipped},
		{EntryID: 1, Undo: pg.UndoWiped},
	}
	if !reflect.DeepEqual(plan.Retirements, wantRetirements) {
		t.Fatalf("plan retirements = %+v, want %+v: every visited open entry retires, reverted to wiped and conflict-skipped to skipped", plan.Retirements, wantRetirements)
	}
	if plan.Wiped != 1 || plan.Skipped != 1 {
		t.Fatalf("summary counts wiped=%d skipped=%d, want wiped=1 skipped=1: the summary reports both counts", plan.Wiped, plan.Skipped)
	}

	// Reported once, never re-visited: after the retirements land, a second
	// wipe visits nothing and repeats no conflict report.
	retired := applyRetirements(journal, plan.Retirements)
	second := pg.PlanWipe(retired, wholeScope)
	if len(second.Conflicts) != 0 || len(second.Retirements) != 0 || len(second.Reverts) != 0 {
		t.Fatalf("second wipe = %+v, want an empty plan: retired entries are never re-visited and conflicts never re-reported", second)
	}
}

// spec: S05/wipe-pipeline-scope
//
// A named `iris workload wipe <pipeline>` narrows the wipe scope to that
// pipeline's journal entries only, leaving other pipelines' open entries
// untouched; bare invocation covers the whole wipe scope; and declare destroy's
// data revert is exactly the narrowed form.
func TestWipePipelineScope(t *testing.T) {
	runPipeline := map[int64]string{40: "pipeline_a", 41: "pipeline_b"}
	journal := []pg.JournalEntry{
		{ID: 1, RunID: 40, Schema: "analytics", Table: "orders", RowPK: "a", Op: pg.OpInsert, Undo: pg.UndoOpen},
		{ID: 2, RunID: 41, Schema: "analytics", Table: "shipments", RowPK: "s", Op: pg.OpInsert, Undo: pg.UndoOpen},
		{ID: 3, RunID: 40, Schema: "analytics", Table: "orders", RowPK: "c", Op: pg.OpUpdate, PreImage: `{"id":"c","amount":1}`, Undo: pg.UndoOpen},
	}

	narrowed := pg.PlanWipe(journal, pg.WipeTarget{Pipeline: "pipeline_a", RunPipeline: runPipeline})
	if got, want := revertEntryIDs(narrowed), []int64{3, 1}; !reflect.DeepEqual(got, want) {
		t.Fatalf("named wipe reverted entry ids %v, want %v: only pipeline_a's entries", got, want)
	}
	for _, r := range narrowed.Retirements {
		if r.EntryID == 2 {
			t.Fatalf("named wipe retired entry 2 (pipeline_b's): other pipelines' open entries must be left untouched")
		}
	}
	// pipeline_b's entry remains open, so a later bare wipe still covers it.
	afterNarrow := applyRetirements(journal, narrowed.Retirements)
	if got, want := scopeIDs(pg.WipeScope(afterNarrow, wholeScope)), []int64{2}; !reflect.DeepEqual(got, want) {
		t.Fatalf("scope after the named wipe = %v, want %v: pipeline_b's entry stays open", got, want)
	}

	bare := pg.PlanWipe(journal, wholeScope)
	if got, want := revertEntryIDs(bare), []int64{3, 2, 1}; !reflect.DeepEqual(got, want) {
		t.Fatalf("bare wipe reverted entry ids %v, want %v: bare invocation covers the whole wipe scope", got, want)
	}

	// declare destroy's data revert is exactly the narrowed form.
	destroy := pg.PlanDestroyRevert(journal, "pipeline_a", runPipeline)
	if !reflect.DeepEqual(destroy, narrowed) {
		t.Fatalf("PlanDestroyRevert = %+v,\nwant the narrowed wipe plan %+v", destroy, narrowed)
	}
}

// spec: S06.3/wipe-reverts-cursor-with-data
//
// Persistent pipeline state (the source cursor) lives in a declared table the
// pipeline writes, so a run's cursor advance is a journaled write like any
// other. Wiping a disposable run rolls that cursor-advance write back together
// with the run's data -- the same plan, the same replay, the same retirement --
// restoring the pre-advance cursor value, so the next pass reprocesses exactly
// the reverted window and never silently skips it.
func TestWipeRevertsCursorWithData(t *testing.T) {
	preAdvance := `{"pipeline":"load_orders","last_seen_id":100}`
	journal := []pg.JournalEntry{
		// The run's data writes: rows 101 and 102 beyond the old cursor.
		{ID: 10, RunID: 42, Schema: "analytics", Table: "orders", RowPK: "101", Op: pg.OpInsert, Undo: pg.UndoOpen},
		{ID: 11, RunID: 42, Schema: "analytics", Table: "orders", RowPK: "102", Op: pg.OpInsert, Undo: pg.UndoOpen},
		// The same run's cursor advance: last_seen_id 100 -> 102, journaled
		// with the pre-advance row as its pre-image.
		{ID: 12, RunID: 42, Schema: "analytics", Table: "load_orders_cursor", RowPK: "load_orders", Op: pg.OpUpdate, PreImage: preAdvance, Undo: pg.UndoOpen},
	}

	plan := pg.PlanWipe(journal, wholeScope)

	// One plan reverts cursor and data together, in reverse order.
	if got, want := revertEntryIDs(plan), []int64{12, 11, 10}; !reflect.DeepEqual(got, want) {
		t.Fatalf("plan reverted entry ids %v, want %v: the cursor-advance write rolls back together with the run's data", got, want)
	}
	cursor := plan.Reverts[0]
	if cursor.Kind != pg.RevertRestorePreImage || cursor.PreImage != preAdvance {
		t.Fatalf("cursor revert = %+v, want a pre-image restore of the pre-advance cursor %q: the next pass must reprocess exactly the reverted window", cursor, preAdvance)
	}

	// The cursor entry retires with the data entries: nothing of run 42 stays
	// open, so no later pass sees a cursor past data that no longer exists.
	wantRetirements := []pg.Retirement{
		{EntryID: 12, Undo: pg.UndoWiped},
		{EntryID: 11, Undo: pg.UndoWiped},
		{EntryID: 10, Undo: pg.UndoWiped},
	}
	if !reflect.DeepEqual(plan.Retirements, wantRetirements) {
		t.Fatalf("plan retirements = %+v, want %+v: the cursor advance retires together with the run's data", plan.Retirements, wantRetirements)
	}
	if plan.Wiped != 3 || plan.Skipped != 0 {
		t.Fatalf("summary counts wiped=%d skipped=%d, want wiped=3 skipped=0", plan.Wiped, plan.Skipped)
	}
}
