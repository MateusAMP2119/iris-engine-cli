package api

import (
	"net/url"
	"strconv"
	"testing"
)

// TestBeforeReverseCursor pins the id-keyed before= reverse keyset cursor
// (specification section 7, the one bounded pagination exception): id-keyed
// collections also take before=, a reverse cursor so log views page newest-first,
// still keyed by the collection key, never by a clock. It proves the reverse page
// is descending with a strict key < before bound; that before= rides only on
// id-keyed collections (name- and composite-keyed collections and /q reject it);
// that after= and before= are mutually exclusive (a page is forward or reverse,
// never both); that the bound value parses per the key column's type, no clock
// param involved; and that a reverse page continues from its last row's key just
// like a forward one.
//
// spec: S07/before-reverse-cursor
func TestBeforeReverseCursor(t *testing.T) {
	runFields := map[string]string{"id": "bigint", "pipeline": "text", "state": "text"}

	// Every id-keyed engine-state collection admits the reverse cursor; the cursor
	// key column is the collection's monotonic id.
	idKeyed := []struct {
		name    string
		coll    Collection
		fields  map[string]string
		keyCol  string
		beforeV any
	}{
		{"runs", CollectionRuns, runFields, "id", int64(42)},
		{"dead_letters", CollectionDeadLetters, map[string]string{"run_id": "bigint"}, "run_id", int64(7)},
		{"journal", CollectionJournal, map[string]string{"id": "bigint"}, "id", int64(99)},
	}

	for _, tc := range idKeyed {
		t.Run("reverse-descending-key-driven/"+tc.name, func(t *testing.T) {
			plan, err := PlanCollectionQuery(tc.coll, tc.fields, url.Values{"before": {strconv.FormatInt(tc.beforeV.(int64), 10)}})
			if err != nil {
				t.Fatalf("PlanCollectionQuery(before): %v", err)
			}
			// Newest-first: the page descends by the key.
			if !plan.Cursor.Descending {
				t.Fatalf("%s before= must page descending (newest-first)", tc.name)
			}
			// Ordering stays key-driven: the plan's key is the collection's monotonic id.
			if len(plan.Cursor.Key.Columns) != 1 || plan.Cursor.Key.Columns[0] != tc.keyCol {
				t.Fatalf("%s reverse cursor key = %v, want [%s] (a key, never a clock)", tc.name, plan.Cursor.Key.Columns, tc.keyCol)
			}
			if !plan.Cursor.Key.IDKeyed {
				t.Fatalf("%s reverse cursor must be id-keyed", tc.name)
			}
			// The bound is a strict key < before, no timestamp comparison.
			if plan.Cursor.Bound == nil || plan.Cursor.Bound.Op != OpLt {
				t.Fatalf("%s before= must apply key < before, got %+v", tc.name, plan.Cursor.Bound)
			}
			if plan.Cursor.Bound.Value != tc.beforeV {
				t.Fatalf("%s before value = %v (%T), want %v (parsed per key type)", tc.name, plan.Cursor.Bound.Value, plan.Cursor.Bound.Value, tc.beforeV)
			}
		})
	}

	t.Run("reverse-page-continues-from-last-key", func(t *testing.T) {
		plan, err := PlanCollectionQuery(CollectionRuns, runFields, url.Values{"before": {"42"}, "limit": {"2"}})
		if err != nil {
			t.Fatalf("PlanCollectionQuery: %v", err)
		}
		// A full reverse page hands back its last (smallest) row's key to continue
		// paging older-still, passed back as before=. It is still a key.
		rows := []map[string]any{{"id": int64(41)}, {"id": int64(39)}}
		if got := plan.Cursor.NextAfter(rows); got != int64(39) {
			t.Fatalf("reverse NextAfter on a full page = %v, want 39 (last row key)", got)
		}
	})

	t.Run("before-rejected-on-name-keyed", func(t *testing.T) {
		// pipelines is name-keyed, not id-keyed: no newest-first log view, no before=.
		_, err := PlanCollectionQuery(CollectionPipelines, map[string]string{"name": "text"}, url.Values{"before": {"x"}})
		if pe := asParamError(t, err); pe.Param != "before" {
			t.Fatalf("name-keyed collection must reject before=; ParamError names %q, want before", pe.Param)
		}
	})

	t.Run("before-rejected-on-composite-keyed", func(t *testing.T) {
		// lanes is (lane, pos)-keyed: a composite key takes no single-value cursor.
		_, err := PlanCollectionQuery(CollectionLanes, map[string]string{"lane": "text", "pos": "int"}, url.Values{"before": {"3"}})
		if pe := asParamError(t, err); pe.Param != "before" {
			t.Fatalf("composite-keyed collection must reject before=; ParamError names %q, want before", pe.Param)
		}
	})

	t.Run("after-and-before-mutually-exclusive", func(t *testing.T) {
		_, err := PlanCollectionQuery(CollectionRuns, runFields, url.Values{"after": {"1"}, "before": {"9"}})
		if err == nil {
			t.Fatalf("after= and before= together must be rejected (a page is forward or reverse, not both)")
		}
	})
}
