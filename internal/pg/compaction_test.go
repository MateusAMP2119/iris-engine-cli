package pg_test

import (
	"reflect"
	"testing"

	"github.com/MateusAMP2119/iris-engine-cli/internal/pg"
)

// TestCompactEntriesCollapseRule proves the pure seal-time compaction rule: a
// sealed partition's pre-images past undo eligibility are nulled, duplicate stamps
// per (schema, table, row_pk, run_id) collapse to the latest op, and each run's
// exact write set survives compaction.
//
// spec: S14/compaction-collapse-rule
func TestCompactEntriesCollapseRule(t *testing.T) {
	t.Run("S14/compaction-collapse-rule", func(t *testing.T) {
		entries := []pg.JournalEntry{
			// run 7 writes row a three times: insert then two updates -> latest update.
			{ID: 1, Schema: "analytics", Table: "orders", RowPK: "a", RunID: 7, Op: pg.OpInsert, PreImage: "i", Undo: pg.UndoPromoted},
			{ID: 2, Schema: "analytics", Table: "orders", RowPK: "a", RunID: 7, Op: pg.OpUpdate, PreImage: "u1", Undo: pg.UndoPromoted},
			{ID: 5, Schema: "analytics", Table: "orders", RowPK: "a", RunID: 7, Op: pg.OpUpdate, PreImage: "u2", Undo: pg.UndoPromoted},
			// run 7 writes a different row b once: survives untouched (op kept).
			{ID: 3, Schema: "analytics", Table: "orders", RowPK: "b", RunID: 7, Op: pg.OpInsert, PreImage: "bi", Undo: pg.UndoPromoted},
			// run 9 writes the same row a: a different key (run scoped) -> survives.
			{ID: 4, Schema: "analytics", Table: "orders", RowPK: "a", RunID: 9, Op: pg.OpUpdate, PreImage: "k", Undo: pg.UndoPromoted},
			// an entry still undo-open keeps its pre-image (past undo eligibility only nulls).
			{ID: 6, Schema: "analytics", Table: "orders", RowPK: "c", RunID: 9, Op: pg.OpUpdate, PreImage: "keepme", Undo: pg.UndoOpen},
		}

		got := pg.CompactEntries(entries)

		want := []pg.JournalEntry{
			{ID: 5, Schema: "analytics", Table: "orders", RowPK: "a", RunID: 7, Op: pg.OpUpdate, PreImage: "", Undo: pg.UndoPromoted},
			{ID: 3, Schema: "analytics", Table: "orders", RowPK: "b", RunID: 7, Op: pg.OpInsert, PreImage: "", Undo: pg.UndoPromoted},
			{ID: 4, Schema: "analytics", Table: "orders", RowPK: "a", RunID: 9, Op: pg.OpUpdate, PreImage: "", Undo: pg.UndoPromoted},
			{ID: 6, Schema: "analytics", Table: "orders", RowPK: "c", RunID: 9, Op: pg.OpUpdate, PreImage: "keepme", Undo: pg.UndoOpen},
		}

		wantByID := map[int64]pg.JournalEntry{}
		for _, w := range want {
			wantByID[w.ID] = w
		}
		if len(got) != len(want) {
			t.Fatalf("compacted set size = %d, want %d: %+v", len(got), len(want), got)
		}
		var lastID int64
		for i, g := range got {
			if i > 0 && g.ID <= lastID {
				t.Errorf("compacted rows not in id order at %d: %d after %d", i, g.ID, lastID)
			}
			lastID = g.ID
			w, ok := wantByID[g.ID]
			if !ok {
				t.Errorf("unexpected surviving entry id %d: %+v", g.ID, g)
				continue
			}
			if !reflect.DeepEqual(g, w) {
				t.Errorf("surviving entry id %d = %+v, want %+v", g.ID, g, w)
			}
		}

		// Each run's exact write set survives: every distinct provenance key that had
		// any entry has exactly one after compaction.
		type pkey struct {
			s, t, p string
			r       int64
		}
		keys := map[pkey]int{}
		for _, g := range got {
			keys[pkey{g.Schema, g.Table, g.RowPK, g.RunID}]++
		}
		for k, n := range keys {
			if n != 1 {
				t.Errorf("provenance key %+v survived %d times, want exactly 1 (write set must survive)", k, n)
			}
		}
	})
}

// TestCompactEntriesEmpty proves compaction of an empty or single-entry input is a
// no-op-shaped identity (no panic, stable output).
//
// spec: S14/compaction-collapse-rule
func TestCompactEntriesEmpty(t *testing.T) {
	t.Run("S14/compaction-collapse-rule", func(t *testing.T) {
		if got := pg.CompactEntries(nil); len(got) != 0 {
			t.Errorf("compact of nil = %+v, want empty", got)
		}
		one := []pg.JournalEntry{{ID: 1, Schema: "s", Table: "t", RowPK: "p", RunID: 1, Op: pg.OpInsert, PreImage: "x", Undo: pg.UndoPromoted}}
		got := pg.CompactEntries(one)
		if len(got) != 1 || got[0].PreImage != "" {
			t.Errorf("compact of single past-undo entry = %+v, want one entry with nil pre-image", got)
		}
	})
}
