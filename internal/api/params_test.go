package api

import (
	"errors"
	"net/url"
	"reflect"
	"testing"

	"github.com/MateusAMP2119/iris-engine-cli/internal/declare"
)

// compileOrdersEndpoint compiles the worked-example endpoint
// (orders_by_customer over analytics.orders) so the /q-route grammar tests run
// against a CompiledEndpoint declare itself produced, not a hand-built binding
// plan.
func compileOrdersEndpoint(t *testing.T) *declare.CompiledEndpoint {
	t.Helper()
	src := []byte(`endpoint: orders_by_customer
source: analytics.orders
fields: [id, customer_id, amount, created_at]
filters:
  customer_id: eq
  created_at: range
sort: id
`)
	ep, err := declare.ParseEndpoint(src)
	if err != nil {
		t.Fatalf("parse endpoint: %v", err)
	}
	tables := map[string]*declare.Table{
		"analytics.orders": {
			Schema: "analytics",
			Table:  "orders",
			Columns: []declare.Column{
				{Name: "id", Type: "bigint", PrimaryKey: true},
				{Name: "customer_id", Type: "uuid"},
				{Name: "amount", Type: "numeric"},
				{Name: "created_at", Type: "timestamptz"},
			},
		},
	}
	ce, err := declare.CompileEndpoint(ep, tables)
	if err != nil {
		t.Fatalf("compile endpoint: %v", err)
	}
	return ce
}

// asParamError unwraps a grammar error to its *ParamError, failing the test when
// the error is nil or is not a ParamError (every 400 the grammar raises must name
// its offending param).
func asParamError(t *testing.T, err error) *ParamError {
	t.Helper()
	if err == nil {
		t.Fatalf("expected a ParamError, got nil")
	}
	var pe *ParamError
	if !errors.As(err, &pe) {
		t.Fatalf("expected a *ParamError, got %T: %v", err, err)
	}
	return pe
}

// TestCollectionKeyRoster pins the fixed cursor key of every collection: a
// monotonic id for runs, dead_letters, and the journal; name for pipelines; the
// (lane, pos) pair for lanes; the table PK for /data; and the endpoint sort field
// for /q. Only the id-keyed collections are marked id-keyed (the before= exception).
func TestCollectionKeyRoster(t *testing.T) {
	cases := []struct {
		name string
		coll Collection
		want CursorKey
	}{
		{"runs", CollectionRuns, CursorKey{Columns: []string{"id"}, IDKeyed: true}},
		{"dead_letters", CollectionDeadLetters, CursorKey{Columns: []string{"run_id"}, IDKeyed: true}},
		{"journal", CollectionJournal, CursorKey{Columns: []string{"id"}, IDKeyed: true}},
		{"pipelines", CollectionPipelines, CursorKey{Columns: []string{"name"}, IDKeyed: false}},
		{"lanes", CollectionLanes, CursorKey{Columns: []string{"lane", "pos"}, IDKeyed: false}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, ok := CollectionKey(tc.coll)
			if !ok {
				t.Fatalf("CollectionKey(%q): not in roster", tc.coll)
			}
			if !reflect.DeepEqual(got, tc.want) {
				t.Fatalf("CollectionKey(%q) = %+v, want %+v", tc.coll, got, tc.want)
			}
		})
	}

	t.Run("data-table-pk", func(t *testing.T) {
		got := DataCursorKey([]string{"order_id"})
		want := CursorKey{Columns: []string{"order_id"}, IDKeyed: false}
		if !reflect.DeepEqual(got, want) {
			t.Fatalf("DataCursorKey = %+v, want %+v", got, want)
		}
	})

	t.Run("q-endpoint-sort", func(t *testing.T) {
		ce := compileOrdersEndpoint(t)
		got := EndpointCursorKey(ce.Sort)
		want := CursorKey{Columns: []string{"id"}, IDKeyed: false}
		if !reflect.DeepEqual(got, want) {
			t.Fatalf("EndpointCursorKey = %+v, want %+v", got, want)
		}
	})

	t.Run("unknown-collection", func(t *testing.T) {
		if _, ok := CollectionKey(Collection("widgets")); ok {
			t.Fatalf("CollectionKey(widgets): unexpectedly in roster")
		}
	})
}

// TestEqRangeGrammar pins the /q filter grammar: an eq filter binds <param>= as
// an exact equality predicate, and a range filter binds <param>_from/<param>_to as
// inclusive lower/upper bounds, either side omittable.
func TestEqRangeGrammar(t *testing.T) {
	ce := compileOrdersEndpoint(t)
	uuid := "11111111-1111-1111-1111-111111111111"
	from := "2024-01-01T00:00:00Z"
	to := "2024-12-31T23:59:59Z"

	t.Run("eq-binds-equality", func(t *testing.T) {
		plan, err := PlanEndpointQuery(ce, url.Values{"customer_id": {uuid}})
		if err != nil {
			t.Fatalf("PlanEndpointQuery: %v", err)
		}
		want := []Predicate{{Column: "customer_id", Op: OpEq, Value: uuid}}
		if !reflect.DeepEqual(plan.Predicates, want) {
			t.Fatalf("predicates = %+v, want %+v", plan.Predicates, want)
		}
	})

	t.Run("range-both-bounds-inclusive", func(t *testing.T) {
		plan, err := PlanEndpointQuery(ce, url.Values{"created_at_from": {from}, "created_at_to": {to}})
		if err != nil {
			t.Fatalf("PlanEndpointQuery: %v", err)
		}
		ops := map[PredicateOp]bool{}
		for _, p := range plan.Predicates {
			if p.Column != "created_at" {
				t.Fatalf("unexpected predicate column %q", p.Column)
			}
			ops[p.Op] = true
		}
		if !ops[OpGte] || !ops[OpLte] {
			t.Fatalf("range must bind inclusive >= and <=, got %+v", plan.Predicates)
		}
		if len(plan.Predicates) != 2 {
			t.Fatalf("range with both bounds must bind two predicates, got %+v", plan.Predicates)
		}
	})

	t.Run("range-from-only", func(t *testing.T) {
		plan, err := PlanEndpointQuery(ce, url.Values{"created_at_from": {from}})
		if err != nil {
			t.Fatalf("PlanEndpointQuery: %v", err)
		}
		if len(plan.Predicates) != 1 || plan.Predicates[0].Op != OpGte {
			t.Fatalf("from-only range must bind one inclusive lower bound, got %+v", plan.Predicates)
		}
	})

	t.Run("range-to-only", func(t *testing.T) {
		plan, err := PlanEndpointQuery(ce, url.Values{"created_at_to": {to}})
		if err != nil {
			t.Fatalf("PlanEndpointQuery: %v", err)
		}
		if len(plan.Predicates) != 1 || plan.Predicates[0].Op != OpLte {
			t.Fatalf("to-only range must bind one inclusive upper bound, got %+v", plan.Predicates)
		}
	})

	t.Run("range-neither-bound", func(t *testing.T) {
		plan, err := PlanEndpointQuery(ce, url.Values{})
		if err != nil {
			t.Fatalf("PlanEndpointQuery: %v", err)
		}
		if len(plan.Predicates) != 0 {
			t.Fatalf("no filter params must bind no predicates, got %+v", plan.Predicates)
		}
	})
}

// TestExactFieldFiltering pins that a collection route filters on exact field
// equality only: each param names a source field and binds an equality predicate;
// there are no operators, no range suffixes, and no LIKE.
func TestExactFieldFiltering(t *testing.T) {
	fields := map[string]string{"id": "bigint", "pipeline": "text", "state": "text"}

	t.Run("two-exact-fields", func(t *testing.T) {
		q := url.Values{"pipeline": {"load_orders"}, "state": {"dead_lettered"}}
		plan, err := PlanCollectionQuery(CollectionRuns, fields, q)
		if err != nil {
			t.Fatalf("PlanCollectionQuery: %v", err)
		}
		want := []Predicate{
			{Column: "pipeline", Op: OpEq, Value: "load_orders"},
			{Column: "state", Op: OpEq, Value: "dead_lettered"},
		}
		if !reflect.DeepEqual(plan.Predicates, want) {
			t.Fatalf("predicates = %+v, want %+v", plan.Predicates, want)
		}
	})

	t.Run("range-suffix-is-not-a-field", func(t *testing.T) {
		// A collection route knows no range grammar: pipeline_from is not a field.
		q := url.Values{"pipeline_from": {"a"}}
		_, err := PlanCollectionQuery(CollectionRuns, fields, q)
		if pe := asParamError(t, err); pe.Param != "pipeline_from" {
			t.Fatalf("ParamError names %q, want pipeline_from", pe.Param)
		}
	})

	t.Run("every-predicate-is-equality", func(t *testing.T) {
		q := url.Values{"pipeline": {"load_orders"}, "state": {"succeeded"}}
		plan, err := PlanCollectionQuery(CollectionRuns, fields, q)
		if err != nil {
			t.Fatalf("PlanCollectionQuery: %v", err)
		}
		for _, p := range plan.Predicates {
			if p.Op != OpEq {
				t.Fatalf("collection predicate %+v is not exact equality", p)
			}
		}
	})
}

// TestKeysetCursorPaging pins keyset-cursor pagination: ascending by the
// collection key with WHERE key > after; the id-keyed before= reverse cursor with
// WHERE key < before descending; page.next_after read from the last row's key;
// and the absence of offset or since-timestamp paging (both rejected as unknown).
func TestKeysetCursorPaging(t *testing.T) {
	fields := map[string]string{"id": "bigint", "pipeline": "text", "state": "text"}

	t.Run("after-ascending", func(t *testing.T) {
		q := url.Values{"after": {"42"}}
		plan, err := PlanCollectionQuery(CollectionRuns, fields, q)
		if err != nil {
			t.Fatalf("PlanCollectionQuery: %v", err)
		}
		if plan.Cursor.Descending {
			t.Fatalf("after cursor must be ascending")
		}
		if plan.Cursor.Bound == nil || plan.Cursor.Bound.Op != OpGt {
			t.Fatalf("after cursor must apply key > after, got %+v", plan.Cursor.Bound)
		}
		if plan.Cursor.Bound.Value != int64(42) {
			t.Fatalf("after value = %v, want int64(42)", plan.Cursor.Bound.Value)
		}
	})

	t.Run("before-reverse-descending-id-keyed", func(t *testing.T) {
		q := url.Values{"before": {"42"}}
		plan, err := PlanCollectionQuery(CollectionRuns, fields, q)
		if err != nil {
			t.Fatalf("PlanCollectionQuery: %v", err)
		}
		if !plan.Cursor.Descending {
			t.Fatalf("before cursor must be descending (newest-first)")
		}
		if plan.Cursor.Bound == nil || plan.Cursor.Bound.Op != OpLt {
			t.Fatalf("before cursor must apply key < before, got %+v", plan.Cursor.Bound)
		}
	})

	t.Run("before-rejected-on-non-id-keyed", func(t *testing.T) {
		// pipelines is name-keyed, not id-keyed: it takes no before= reverse cursor.
		pf := map[string]string{"name": "text"}
		_, err := PlanCollectionQuery(CollectionPipelines, pf, url.Values{"before": {"x"}})
		if pe := asParamError(t, err); pe.Param != "before" {
			t.Fatalf("ParamError names %q, want before", pe.Param)
		}
	})

	t.Run("after-and-before-mutually-exclusive", func(t *testing.T) {
		q := url.Values{"after": {"1"}, "before": {"9"}}
		if _, err := PlanCollectionQuery(CollectionRuns, fields, q); err == nil {
			t.Fatalf("after and before together must be rejected")
		}
	})

	t.Run("no-offset-paging", func(t *testing.T) {
		_, err := PlanCollectionQuery(CollectionRuns, fields, url.Values{"offset": {"20"}})
		if pe := asParamError(t, err); pe.Param != "offset" {
			t.Fatalf("offset must be unknown; ParamError names %q", pe.Param)
		}
	})

	t.Run("no-since-timestamp-paging", func(t *testing.T) {
		_, err := PlanCollectionQuery(CollectionRuns, fields, url.Values{"since": {"2024-01-01T00:00:00Z"}})
		if pe := asParamError(t, err); pe.Param != "since" {
			t.Fatalf("since must be unknown; ParamError names %q", pe.Param)
		}
	})

	t.Run("next-after-from-last-row-on-full-page", func(t *testing.T) {
		plan, err := PlanCollectionQuery(CollectionRuns, fields, url.Values{"limit": {"2"}})
		if err != nil {
			t.Fatalf("PlanCollectionQuery: %v", err)
		}
		rows := []map[string]any{{"id": int64(7)}, {"id": int64(9)}}
		if got := plan.Cursor.NextAfter(rows); got != int64(9) {
			t.Fatalf("NextAfter on a full page = %v, want 9 (last row key)", got)
		}
	})

	t.Run("next-after-nil-on-short-page", func(t *testing.T) {
		plan, err := PlanCollectionQuery(CollectionRuns, fields, url.Values{"limit": {"5"}})
		if err != nil {
			t.Fatalf("PlanCollectionQuery: %v", err)
		}
		rows := []map[string]any{{"id": int64(7)}}
		if got := plan.Cursor.NextAfter(rows); got != nil {
			t.Fatalf("NextAfter on a short page = %v, want nil", got)
		}
	})

	t.Run("q-endpoint-ascending-by-sort", func(t *testing.T) {
		ce := compileOrdersEndpoint(t)
		plan, err := PlanEndpointQuery(ce, url.Values{"after": {"100"}})
		if err != nil {
			t.Fatalf("PlanEndpointQuery: %v", err)
		}
		if plan.Cursor.Descending || plan.Cursor.Key.IDKeyed {
			t.Fatalf("/q pages ascending by sort, never id-keyed: %+v", plan.Cursor)
		}
		if plan.Cursor.Bound == nil || plan.Cursor.Bound.Op != OpGt {
			t.Fatalf("/q after cursor must apply sort > after, got %+v", plan.Cursor.Bound)
		}
	})

	t.Run("q-endpoint-before-rejected", func(t *testing.T) {
		ce := compileOrdersEndpoint(t)
		_, err := PlanEndpointQuery(ce, url.Values{"before": {"100"}})
		if pe := asParamError(t, err); pe.Param != "before" {
			t.Fatalf("/q takes no before cursor; ParamError names %q", pe.Param)
		}
	})
}

// TestLimitDefaultCap pins the limit rule: it defaults to 100, caps at 1000, and
// an over-cap value is a 400 naming limit, never a silent clamp.
func TestLimitDefaultCap(t *testing.T) {
	fields := map[string]string{"id": "bigint"}

	t.Run("default-100", func(t *testing.T) {
		plan, err := PlanCollectionQuery(CollectionRuns, fields, url.Values{})
		if err != nil {
			t.Fatalf("PlanCollectionQuery: %v", err)
		}
		if plan.Cursor.Limit != 100 {
			t.Fatalf("default limit = %d, want 100", plan.Cursor.Limit)
		}
	})

	t.Run("explicit-within-cap", func(t *testing.T) {
		plan, err := PlanCollectionQuery(CollectionRuns, fields, url.Values{"limit": {"250"}})
		if err != nil {
			t.Fatalf("PlanCollectionQuery: %v", err)
		}
		if plan.Cursor.Limit != 250 {
			t.Fatalf("limit = %d, want 250", plan.Cursor.Limit)
		}
	})

	t.Run("at-cap-1000", func(t *testing.T) {
		plan, err := PlanCollectionQuery(CollectionRuns, fields, url.Values{"limit": {"1000"}})
		if err != nil {
			t.Fatalf("PlanCollectionQuery: %v", err)
		}
		if plan.Cursor.Limit != 1000 {
			t.Fatalf("limit = %d, want 1000", plan.Cursor.Limit)
		}
	})

	t.Run("over-cap-is-400-not-clamped", func(t *testing.T) {
		_, err := PlanCollectionQuery(CollectionRuns, fields, url.Values{"limit": {"1001"}})
		if pe := asParamError(t, err); pe.Param != "limit" {
			t.Fatalf("over-cap limit must 400 naming limit, got %q", pe.Param)
		}
	})

	t.Run("non-integer-is-400", func(t *testing.T) {
		_, err := PlanCollectionQuery(CollectionRuns, fields, url.Values{"limit": {"lots"}})
		if pe := asParamError(t, err); pe.Param != "limit" {
			t.Fatalf("non-integer limit must 400 naming limit, got %q", pe.Param)
		}
	})

	t.Run("non-positive-is-400", func(t *testing.T) {
		_, err := PlanCollectionQuery(CollectionRuns, fields, url.Values{"limit": {"0"}})
		if pe := asParamError(t, err); pe.Param != "limit" {
			t.Fatalf("non-positive limit must 400 naming limit, got %q", pe.Param)
		}
	})
}

// TestParamTypeParse400 pins that a param value failing to parse per its
// source-column type yields a ParamError naming the param, across the closed type
// set (integer, uuid, timestamptz, bool, numeric, json, bytea, ...).
func TestParamTypeParse400(t *testing.T) {
	t.Run("endpoint-bad-uuid-eq", func(t *testing.T) {
		ce := compileOrdersEndpoint(t)
		_, err := PlanEndpointQuery(ce, url.Values{"customer_id": {"not-a-uuid"}})
		if pe := asParamError(t, err); pe.Param != "customer_id" {
			t.Fatalf("ParamError names %q, want customer_id", pe.Param)
		}
	})

	t.Run("endpoint-bad-timestamp-range", func(t *testing.T) {
		ce := compileOrdersEndpoint(t)
		_, err := PlanEndpointQuery(ce, url.Values{"created_at_from": {"yesterday"}})
		if pe := asParamError(t, err); pe.Param != "created_at_from" {
			t.Fatalf("ParamError names %q, want created_at_from", pe.Param)
		}
	})

	t.Run("collection-bad-bigint", func(t *testing.T) {
		fields := map[string]string{"id": "bigint", "handle": "bigint"}
		_, err := PlanCollectionQuery(CollectionRuns, fields, url.Values{"handle": {"NaN"}})
		if pe := asParamError(t, err); pe.Param != "handle" {
			t.Fatalf("ParamError names %q, want handle", pe.Param)
		}
	})

	t.Run("bad-after-cursor-value", func(t *testing.T) {
		fields := map[string]string{"id": "bigint"}
		_, err := PlanCollectionQuery(CollectionRuns, fields, url.Values{"after": {"abc"}})
		if pe := asParamError(t, err); pe.Param != "after" {
			t.Fatalf("ParamError names %q, want after", pe.Param)
		}
	})

	// parseValue is exercised directly across the closed type set so every wire
	// type's parse and its failure mode are pinned.
	valid := []struct {
		pgType string
		raw    string
	}{
		{"smallint", "12"},
		{"integer", "-7"},
		{"bigint", "9000000000"},
		{"double precision", "3.14"},
		{"numeric", "12.50"},
		{"numeric(10,2)", "-0.01"},
		{"boolean", "true"},
		{"text", "anything at all"},
		{"varchar(20)", "short"},
		{"uuid", "11111111-1111-1111-1111-111111111111"},
		{"timestamptz", "2024-06-01T12:00:00Z"},
		{"date", "2024-06-01"},
		{"time", "12:00:00"},
		{"json", `{"a":1}`},
		{"jsonb", `[1,2,3]`},
		{"bytea", "aGVsbG8="},
	}
	for _, tc := range valid {
		t.Run("parses/"+tc.pgType, func(t *testing.T) {
			if _, err := parseValue(tc.pgType, tc.raw); err != nil {
				t.Fatalf("parseValue(%q, %q): unexpected error %v", tc.pgType, tc.raw, err)
			}
		})
	}

	invalid := []struct {
		pgType string
		raw    string
	}{
		{"smallint", "1.5"},
		{"integer", "x"},
		{"bigint", "12.0"},
		{"double precision", "words"},
		{"numeric", "1/2"},
		{"boolean", "maybe"},
		{"uuid", "1234"},
		{"timestamptz", "not-a-time"},
		{"date", "2024-13-40"},
		{"time", "99:99"},
		{"json", "{not json"},
		{"bytea", "!!!not-base64!!!"},
	}
	for _, tc := range invalid {
		t.Run("rejects/"+tc.pgType, func(t *testing.T) {
			if _, err := parseValue(tc.pgType, tc.raw); err == nil {
				t.Fatalf("parseValue(%q, %q): expected a parse error", tc.pgType, tc.raw)
			}
		})
	}
}

// TestUnknownRepeatedParam400 pins that an unknown param or a repeated param is a
// 400 naming it, never silently ignored, on both collection and /q routes.
func TestUnknownRepeatedParam400(t *testing.T) {
	fields := map[string]string{"id": "bigint", "pipeline": "text"}

	t.Run("collection-unknown-param", func(t *testing.T) {
		_, err := PlanCollectionQuery(CollectionRuns, fields, url.Values{"colour": {"red"}})
		if pe := asParamError(t, err); pe.Param != "colour" {
			t.Fatalf("ParamError names %q, want colour", pe.Param)
		}
	})

	t.Run("collection-repeated-field", func(t *testing.T) {
		_, err := PlanCollectionQuery(CollectionRuns, fields, url.Values{"pipeline": {"a", "b"}})
		if pe := asParamError(t, err); pe.Param != "pipeline" {
			t.Fatalf("ParamError names %q, want pipeline", pe.Param)
		}
	})

	t.Run("collection-repeated-limit", func(t *testing.T) {
		_, err := PlanCollectionQuery(CollectionRuns, fields, url.Values{"limit": {"10", "20"}})
		if pe := asParamError(t, err); pe.Param != "limit" {
			t.Fatalf("ParamError names %q, want limit", pe.Param)
		}
	})

	t.Run("endpoint-unknown-param", func(t *testing.T) {
		ce := compileOrdersEndpoint(t)
		// created_at (bare) is not a param: the range binds created_at_from/_to.
		_, err := PlanEndpointQuery(ce, url.Values{"created_at": {"x"}})
		if pe := asParamError(t, err); pe.Param != "created_at" {
			t.Fatalf("ParamError names %q, want created_at", pe.Param)
		}
	})

	t.Run("endpoint-repeated-param", func(t *testing.T) {
		ce := compileOrdersEndpoint(t)
		uuid := "11111111-1111-1111-1111-111111111111"
		_, err := PlanEndpointQuery(ce, url.Values{"customer_id": {uuid, uuid}})
		if pe := asParamError(t, err); pe.Param != "customer_id" {
			t.Fatalf("ParamError names %q, want customer_id", pe.Param)
		}
	})
}
