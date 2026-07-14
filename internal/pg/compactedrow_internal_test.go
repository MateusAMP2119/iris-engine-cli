package pg

import (
	"fmt"
	"testing"
)

// This file proves ParseCompactedRow inverts the canonical compacted-row
// serialization QueryCompactedRows produces (and the archive export persists):
// the archived-provenance fallback depends on recovering exact JournalEntry
// stamps from those bytes, including a pre-image whose JSON contains the field
// delimiter itself.

func TestParseCompactedRow(t *testing.T) {
	t.Run("parse-compacted-row", func(t *testing.T) {
		encode := func(id int64, role string, run int64, schema, table, pk, op, pre, undo, rec string) []byte {
			// The exact encoder shape from QueryCompactedRows.
			return []byte(fmt.Sprintf("%d|%s|%d|%s|%s|%s|%s|%s|%s|%s", id, role, run, schema, table, pk, op, pre, undo, rec))
		}

		t.Run("round-trips a full entry, pre-image pipes included", func(t *testing.T) {
			pre := `{"amount": 5, "note": "a|b|c"}`
			b := encode(42, "iris_admin", 7, "analytics", "orders", "200", "update", pre, "promoted", "2026-07-14T00:00:00Z")
			e, ok := ParseCompactedRow(b)
			if !ok {
				t.Fatalf("ParseCompactedRow rejected a canonical row: %q", b)
			}
			want := JournalEntry{ID: 42, RunID: 7, Schema: "analytics", Table: "orders", RowPK: "200",
				Op: WriteOp("update"), PreImage: pre, Undo: UndoState("promoted")}
			if e != want {
				t.Errorf("parsed entry = %+v, want %+v", e, want)
			}
		})

		t.Run("round-trips an empty pre-image (SQL NULL)", func(t *testing.T) {
			b := encode(1, "r", 2, "s", "t", "pk", "insert", "", "open", "rec")
			e, ok := ParseCompactedRow(b)
			if !ok {
				t.Fatalf("ParseCompactedRow rejected an empty-pre row: %q", b)
			}
			if e.PreImage != "" || e.Op != WriteOp("insert") || e.Undo != UndoOpen {
				t.Errorf("parsed entry = %+v, want empty pre, insert, open", e)
			}
		})

		t.Run("rejects malformed bytes rather than misreading them", func(t *testing.T) {
			for _, b := range [][]byte{
				[]byte(""),
				[]byte("not-a-row"),
				[]byte("x|role|7|s|t|pk|op|pre|undo|rec"),   // non-numeric id
				[]byte("1|role|x|s|t|pk|op|pre|undo|rec"),   // non-numeric run id
				[]byte("1|role|7|s|t|pk|op|tail-too-short"), // no undo/recorded_at split
			} {
				if _, ok := ParseCompactedRow(b); ok {
					t.Errorf("ParseCompactedRow accepted malformed bytes %q", b)
				}
			}
		})
	})
}
