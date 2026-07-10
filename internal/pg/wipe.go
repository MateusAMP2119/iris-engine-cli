package pg

import "sort"

// This file owns the pure wipe model: the algorithm behind `iris workload wipe
// [<pipeline>]` and behind declare destroy's data revert (specification sections
// 5, 6.3, 12 and 14). It plans a wipe entirely over in-memory journal rows -- no
// SQL is built or executed here; a later task feeds the plan to the data
// database in one transaction (journal and tables co-reside: no partial wipe).
// Keeping the algorithm pure keeps every rule unit-testable and keeps the live
// executor a dumb interpreter of an already-decided plan.
//
// The rules modeled, in the order the plan applies them:
//
//   - Scope (S05/wipe-scope-rule). Wipe scope is exactly the journal entries
//     still undo = 'open': written under disposable data_mode and unreleased by
//     promotion (promotion flips open entries to 'promoted'; permanent-mode
//     writes are born 'promoted'; earlier wipes leave 'wiped' or 'skipped').
//     Everything outside 'open' is provenance memory only, and no operation in
//     this model -- or anywhere -- re-arms it.
//
//   - Reverse replay (S05/wipe-reverse-replay). Scope entries replay in reverse
//     id order: a disposable insert reverts by deleting the row, a wipe-eligible
//     update or delete by restoring its captured pre-image. Reverse order is
//     what unwinds same-row stacking inside the scope: journal ids per (schema,
//     table, row_pk) are strictly commit-ordered (the trigger fires in the
//     writing transaction), so replaying newest-first rewinds a contested row
//     layer by layer to its pre-scope state.
//
//   - Conflict skip (S05/wipe-conflict-skip). An open entry is conflict-skipped,
//     its row left as-is, whenever a later journal entry exists for the same
//     (schema, table, row_pk) whose write is still in the row's value. The check
//     is journal-internal with no image comparison -- every write is captured,
//     so it is total -- and "still in the row's value" is decidable from undo
//     alone: 'promoted', 'skipped', and out-of-scope 'open' writes are in the
//     value; a 'wiped' write is not (spec section 5: "a skipped write is still
//     in the row's value, a wiped one is not"). Later same-scope entries never
//     conflict: replay visits them first, retiring them to 'wiped', which is
//     exactly why the replay runs in reverse. The report names the conflicting
//     run: the nearest later still-in-value entry's run, the write that sits
//     immediately on top of the skipped one.
//
//   - Retirement (S05/wipe-retires-all-visited). Every visited open entry is
//     retired: reverted ones to 'wiped', conflict-skipped ones to 'skipped'.
//     Retired entries leave the wipe scope, so conflicts are reported once and
//     never re-visited, and the summary reports both counts.
//
//   - Pipeline narrowing (S05/wipe-pipeline-scope). A named wipe narrows the
//     scope to one pipeline's journal entries -- attributed through the run that
//     wrote them -- leaving other pipelines' open entries untouched; a bare wipe
//     covers the whole scope. Declare destroy's data revert is exactly the
//     narrowed form (PlanDestroyRevert delegates to PlanWipe).
//
//   - Cursor rides along (S06.3/wipe-reverts-cursor-with-data). A pipeline's
//     source cursor lives in a declared table it writes, so a run's
//     cursor-advance is a journaled write like any other and needs no special
//     case here: wiping the run restores the pre-advance cursor row in the same
//     plan as the run's data, so the next pass reprocesses exactly the reverted
//     window and never silently skips it. That the cursor is handled by NOT
//     being special is the point: an engine-held cursor field would be
//     unattributed and unrevertible.
//
// Sealed partitions are out of reach by construction: a partition seals only
// with zero open entries (partition.go), so the wipe scope always lives entirely
// in unsealed partitions and this model never needs to know about sealing.

// UndoState is a journal entry's undo lifecycle state: the data_journal.undo
// value set (specification section 4), mirrored here so the wipe model and the
// journal DDL agree.
type UndoState string

// The journal undo states.
const (
	// UndoOpen marks an entry written under disposable data_mode and unreleased
	// by promotion: wipe scope is exactly the open entries.
	UndoOpen UndoState = "open"
	// UndoPromoted marks an entry released by promotion or born under permanent
	// data_mode: provenance memory, never wipe-touched, never re-armed.
	UndoPromoted UndoState = "promoted"
	// UndoWiped marks an entry a wipe reverted: its write is no longer in the
	// row's value.
	UndoWiped UndoState = "wiped"
	// UndoSkipped marks an entry a wipe conflict-skipped: its write is still in
	// the row's value, reported once and never re-visited.
	UndoSkipped UndoState = "skipped"
)

// RowKey identifies one row of one declared table: the (schema, table, row_pk)
// axis the journal's provenance index and the wipe conflict rule key on.
type RowKey struct {
	// Schema is the declared table's schema.
	Schema string
	// Table is the declared table's name.
	Table string
	// RowPK is the row's rendered primary-key value.
	RowPK string
}

// JournalEntry is one data_journal row in memory: the fixture shape the pure
// wipe model consumes (specification section 4; the DDL lives in schema.go).
// Attribution columns the wipe never reads (pg_role, recorded_at) are omitted:
// this is the wipe-relevant projection of a journal row, not a second schema.
type JournalEntry struct {
	// ID is the monotonic bigint identity ordering key. Per (schema, table,
	// row_pk) ids are strictly commit-ordered, which is what makes reverse
	// replay and the later-entry conflict check sound.
	ID int64
	// RunID is the writing run (runs.id; logical link, never FK-enforced).
	RunID int64
	// Schema is the written table's schema.
	Schema string
	// Table is the written table's name.
	Table string
	// RowPK is the written row's rendered primary-key value.
	RowPK string
	// Op is the captured write operation.
	Op WriteOp
	// PreImage is the prior row JSON, present only on wipe-eligible updates and
	// deletes (payload.go); empty models SQL NULL.
	PreImage string
	// Undo is the entry's undo lifecycle state.
	Undo UndoState
}

// Key returns the entry's (schema, table, row_pk) row key.
func (e JournalEntry) Key() RowKey {
	return RowKey{Schema: e.Schema, Table: e.Table, RowPK: e.RowPK}
}

// WipeTarget names what a wipe covers. The zero value is the bare invocation:
// the whole wipe scope, every pipeline's open entries.
type WipeTarget struct {
	// Pipeline, when non-empty, narrows the wipe to that pipeline's journal
	// entries only (`iris workload wipe <pipeline>`, and the revert half of
	// `iris declare destroy`).
	Pipeline string
	// RunPipeline attributes journal entries to pipelines through the run that
	// wrote them (runs.pipeline), consulted only when Pipeline narrows the
	// scope. An entry whose run is absent from the map belongs to no known
	// pipeline and never matches a narrowed target.
	RunPipeline map[int64]string
}

// covers reports whether the target's scope includes the entry (undo state
// aside): a bare target covers everything, a narrowed target only the named
// pipeline's writes.
func (t WipeTarget) covers(e JournalEntry) bool {
	return t.Pipeline == "" || t.RunPipeline[e.RunID] == t.Pipeline
}

// RevertKind is how one journal entry's write is rolled back.
type RevertKind string

// The revert kinds.
const (
	// RevertDeleteRow deletes the row: the revert of a disposable insert.
	RevertDeleteRow RevertKind = "delete_row"
	// RevertRestorePreImage restores the entry's captured pre-image: the revert
	// of a wipe-eligible update (write the prior row back) or delete (re-insert
	// the prior row).
	RevertRestorePreImage RevertKind = "restore_pre_image"
)

// RowRevert is one row-level rollback in a wipe plan, in replay order.
type RowRevert struct {
	// EntryID is the reverted journal entry.
	EntryID int64
	// Row is the affected row.
	Row RowKey
	// Op is the captured operation being rolled back.
	Op WriteOp
	// Kind is how the write rolls back: delete the row, or restore PreImage.
	Kind RevertKind
	// PreImage is the prior row JSON to restore; set exactly when Kind is
	// RevertRestorePreImage.
	PreImage string
}

// Retirement is one undo transition in a wipe plan: the visited entry and the
// state it retires to, UndoWiped for reverted entries and UndoSkipped for
// conflict-skipped ones.
type Retirement struct {
	// EntryID is the retired journal entry.
	EntryID int64
	// Undo is the retirement state: UndoWiped or UndoSkipped.
	Undo UndoState
}

// Conflict is one conflict-skip report: an open entry left as-is because a
// later write for the same row is still in the row's value. The report names
// the conflicting run.
type Conflict struct {
	// EntryID is the conflict-skipped journal entry.
	EntryID int64
	// RunID is the run whose write was skipped.
	RunID int64
	// Row is the contested row.
	Row RowKey
	// ConflictingEntryID is the nearest later still-in-value entry for the row:
	// the write sitting immediately on top of the skipped one.
	ConflictingEntryID int64
	// ConflictingRunID is the conflicting entry's run: the run the report names.
	ConflictingRunID int64
}

// WipePlan is the decided outcome of one wipe: the row rollbacks, undo
// retirements, and conflict reports the live executor applies in one data-
// database transaction, plus the summary counts. Reverts and Retirements are in
// replay (reverse id) order; Conflicts in the order encountered.
type WipePlan struct {
	// Reverts are the row rollbacks, in replay order.
	Reverts []RowRevert
	// Retirements are the undo transitions, one per visited entry, in replay
	// order.
	Retirements []Retirement
	// Conflicts are the conflict-skip reports, in the order encountered.
	Conflicts []Conflict
	// Wiped counts reverted entries; the summary reports it beside Skipped.
	Wiped int
	// Skipped counts conflict-skipped entries.
	Skipped int
}

// WipeScope selects the wipe's scope from the journal: exactly the entries
// still undo = open (written under disposable data_mode, unreleased by
// promotion), narrowed to the target's pipeline when one is named. Promoted,
// wiped, and skipped entries are provenance memory only, never selected;
// nothing re-arms them. The selection is returned in ascending id order.
func WipeScope(journal []JournalEntry, target WipeTarget) []JournalEntry {
	var scope []JournalEntry
	for _, e := range journal {
		if e.Undo == UndoOpen && target.covers(e) {
			scope = append(scope, e)
		}
	}
	sort.Slice(scope, func(i, j int) bool { return scope[i].ID < scope[j].ID })
	return scope
}

// PlanWipe plans one wipe over the journal: it selects the target's scope,
// replays it in reverse id order -- deleting disposable inserts, restoring
// pre-images for updates and deletes, conflict-skipping any entry with a later
// still-in-value write on its row -- and retires every visited entry, reverted
// ones to wiped and conflict-skipped ones to skipped. The journal is read-only
// input; the returned plan is the wipe's entire effect.
func PlanWipe(journal []JournalEntry, target WipeTarget) WipePlan {
	scope := WipeScope(journal, target)

	// undo tracks each entry's state as the replay retires scope entries: a
	// same-scope later entry already reverted this pass is 'wiped' here and so
	// never conflicts, while one already conflict-skipped is 'skipped' and does
	// -- its write is still in the row's value.
	undo := make(map[int64]UndoState, len(journal))
	for _, e := range journal {
		undo[e.ID] = e.Undo
	}

	var plan WipePlan
	for i := len(scope) - 1; i >= 0; i-- {
		e := scope[i]
		if c, contested := nearestLaterInValue(journal, undo, e); contested {
			plan.Conflicts = append(plan.Conflicts, Conflict{
				EntryID:            e.ID,
				RunID:              e.RunID,
				Row:                e.Key(),
				ConflictingEntryID: c.ID,
				ConflictingRunID:   c.RunID,
			})
			plan.Retirements = append(plan.Retirements, Retirement{EntryID: e.ID, Undo: UndoSkipped})
			undo[e.ID] = UndoSkipped
			plan.Skipped++
			continue
		}
		plan.Reverts = append(plan.Reverts, revertOf(e))
		plan.Retirements = append(plan.Retirements, Retirement{EntryID: e.ID, Undo: UndoWiped})
		undo[e.ID] = UndoWiped
		plan.Wiped++
	}
	return plan
}

// PlanDestroyRevert plans declare destroy's data revert for one pipeline: it is
// exactly the narrowed wipe (`iris workload wipe <pipeline>`) on the destroy
// target, by delegation rather than by a parallel implementation, so the two
// can never drift (specification sections 5 and 12).
func PlanDestroyRevert(journal []JournalEntry, pipeline string, runPipeline map[int64]string) WipePlan {
	return PlanWipe(journal, WipeTarget{Pipeline: pipeline, RunPipeline: runPipeline})
}

// nearestLaterInValue returns the lowest-id journal entry after e on the same
// (schema, table, row_pk) whose write is still in the row's value -- undo
// anything but wiped, under the replay-updated undo states -- and whether one
// exists. It is the wipe conflict check: journal-internal, no image comparison,
// total because every write is captured.
func nearestLaterInValue(journal []JournalEntry, undo map[int64]UndoState, e JournalEntry) (JournalEntry, bool) {
	var nearest JournalEntry
	found := false
	for _, o := range journal {
		if o.ID <= e.ID || o.Key() != e.Key() || undo[o.ID] == UndoWiped {
			continue
		}
		if !found || o.ID < nearest.ID {
			nearest = o
			found = true
		}
	}
	return nearest, found
}

// revertOf returns the row rollback for one scope entry: delete the row for a
// disposable insert, restore the captured pre-image for an update or delete.
func revertOf(e JournalEntry) RowRevert {
	r := RowRevert{EntryID: e.ID, Row: e.Key(), Op: e.Op}
	if e.Op == OpInsert {
		r.Kind = RevertDeleteRow
		return r
	}
	r.Kind = RevertRestorePreImage
	r.PreImage = e.PreImage
	return r
}

// CompactJournal applies the compaction collapse rule to a set of journal
// entries (S14/compaction-collapse-rule). It is pure unit logic.
//
// Rules:
//   - Pre-images are nulled for any entry whose undo is not UndoOpen (released
//     past undo eligibility).
//   - Within each (schema, table, row_pk, run_id) group, only the entry with
//     the highest ID (latest op) is kept; its op and (possibly nulled) pre-image
//     survive.
//   - Each run's distinct rows survive exactly: no cross-row dropping within a run.
//
// Entries are returned in ascending id order of the survivors.
func CompactJournal(entries []JournalEntry) []JournalEntry {
	// Group by (schema, table, row_pk, run_id) and keep only the highest-id per group.
	type key struct {
		schema, table, rowPK string
		runID                int64
	}
	best := map[key]JournalEntry{}
	for _, e := range entries {
		k := key{schema: e.Schema, table: e.Table, rowPK: e.RowPK, runID: e.RunID}
		if cur, ok := best[k]; !ok || e.ID > cur.ID {
			best[k] = e
		}
	}
	// Collect survivors, null released pre-images on them, sort by id asc.
	out := make([]JournalEntry, 0, len(best))
	for _, e := range best {
		if e.Undo != UndoOpen {
			e.PreImage = ""
		}
		out = append(out, e)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out
}
