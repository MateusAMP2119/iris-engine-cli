package api

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/MateusAMP2119/iris-engine-cli/internal/declare"
)

// These tests pin the response-side half of the read-API wire contract: the
// data/page success envelope, the shared error envelope with its closed code
// set, and the per-column-type JSON serialization rules. They are byte-exact
// where the wire shape is pinned: the envelope key order, the row column order
// (rows mirror source columns), and each type's JSON form are asserted against
// literal JSON bytes, not canonicalized values.

// ordersColumns is the response shape of the worked-example endpoint
// (orders_by_customer over analytics.orders), in projection order.
func ordersColumns() []ResponseColumn {
	return []ResponseColumn{
		{Name: "id", PgType: "bigint"},
		{Name: "customer_id", PgType: "uuid"},
		{Name: "amount", PgType: "numeric"},
		{Name: "created_at", PgType: "timestamptz"},
	}
}

// ordersTable is the declared source table of the worked example, for
// EndpointColumns resolution.
func ordersTable() *declare.Table {
	return &declare.Table{
		Schema: "analytics",
		Table:  "orders",
		Columns: []declare.Column{
			{Name: "id", Type: "bigint", PrimaryKey: true},
			{Name: "customer_id", Type: "uuid"},
			{Name: "amount", Type: "numeric"},
			{Name: "created_at", Type: "timestamptz"},
		},
	}
}

// TestResponseEnvelope pins the success envelope: { "data": [ ... ], "page": {
// "next_after": <key|null>, "limit": <n> } }, with rows mirroring the source
// columns in projection order, data always a JSON array (never null),
// next_after the last served row's key when the page filled to its limit and
// null otherwise, and a composite key rendered as the ordered key tuple.
func TestResponseEnvelope(t *testing.T) {
	cols := ordersColumns()
	row1 := map[string]any{
		"id":          int64(1),
		"customer_id": "0a68e17c-9915-4b26-a584-1c979be62b19",
		"amount":      "19.99",
		"created_at":  time.Date(2026, 7, 4, 12, 0, 0, 0, time.UTC),
	}
	row2 := map[string]any{
		"id":          int64(2),
		"customer_id": "0a68e17c-9915-4b26-a584-1c979be62b19",
		"amount":      "5.00",
		"created_at":  time.Date(2026, 7, 4, 12, 30, 0, 0, time.UTC),
	}

	t.Run("full-page-next-after", func(t *testing.T) {
		// Two rows against limit 2: the page filled, so next_after is the last
		// row's key value, serialized per the key column's type (bigint: number).
		cursor := CursorPlan{Key: EndpointCursorKey("id"), Limit: 2}
		got, err := RenderPage(cols, []map[string]any{row1, row2}, cursor)
		if err != nil {
			t.Fatalf("RenderPage: %v", err)
		}
		want := `{"data":[` +
			`{"id":1,"customer_id":"0a68e17c-9915-4b26-a584-1c979be62b19","amount":"19.99","created_at":"2026-07-04T12:00:00Z"},` +
			`{"id":2,"customer_id":"0a68e17c-9915-4b26-a584-1c979be62b19","amount":"5.00","created_at":"2026-07-04T12:30:00Z"}` +
			`],"page":{"next_after":2,"limit":2}}`
		if string(got) != want {
			t.Fatalf("RenderPage:\n got %s\nwant %s", got, want)
		}
	})

	t.Run("short-page-null-next-after", func(t *testing.T) {
		// One row against limit 100: the page did not fill, so there is no next
		// page and next_after is JSON null.
		cursor := CursorPlan{Key: EndpointCursorKey("id"), Limit: 100}
		got, err := RenderPage(cols, []map[string]any{row1}, cursor)
		if err != nil {
			t.Fatalf("RenderPage: %v", err)
		}
		want := `{"data":[` +
			`{"id":1,"customer_id":"0a68e17c-9915-4b26-a584-1c979be62b19","amount":"19.99","created_at":"2026-07-04T12:00:00Z"}` +
			`],"page":{"next_after":null,"limit":100}}`
		if string(got) != want {
			t.Fatalf("RenderPage:\n got %s\nwant %s", got, want)
		}
	})

	t.Run("empty-page-data-array", func(t *testing.T) {
		// No rows: data is the empty array, never null; next_after is null.
		cursor := CursorPlan{Key: EndpointCursorKey("id"), Limit: 100}
		got, err := RenderPage(cols, nil, cursor)
		if err != nil {
			t.Fatalf("RenderPage: %v", err)
		}
		want := `{"data":[],"page":{"next_after":null,"limit":100}}`
		if string(got) != want {
			t.Fatalf("RenderPage:\n got %s\nwant %s", got, want)
		}
	})

	t.Run("rows-mirror-source-columns", func(t *testing.T) {
		// The row's keys are exactly the source columns, in projection order,
		// resolved from the compiled endpoint: never alphabetical, never a subset.
		ce := compileOrdersEndpoint(t)
		resolved, err := EndpointColumns(ce, ordersTable())
		if err != nil {
			t.Fatalf("EndpointColumns: %v", err)
		}
		wantCols := ordersColumns()
		if len(resolved) != len(wantCols) {
			t.Fatalf("EndpointColumns: got %d columns, want %d", len(resolved), len(wantCols))
		}
		for i := range wantCols {
			if resolved[i] != wantCols[i] {
				t.Fatalf("EndpointColumns[%d] = %+v, want %+v", i, resolved[i], wantCols[i])
			}
		}
		cursor := CursorPlan{Key: EndpointCursorKey(ce.Sort), Limit: 100}
		got, err := RenderPage(resolved, []map[string]any{row1}, cursor)
		if err != nil {
			t.Fatalf("RenderPage: %v", err)
		}
		wantRow := `{"id":1,"customer_id":"0a68e17c-9915-4b26-a584-1c979be62b19","amount":"19.99","created_at":"2026-07-04T12:00:00Z"}`
		if !strings.Contains(string(got), wantRow) {
			t.Fatalf("RenderPage: row does not mirror source column order:\n got %s\nwant row %s", got, wantRow)
		}
	})

	t.Run("composite-key-tuple", func(t *testing.T) {
		// The lanes collection is keyed by the (lane, pos) pair: a full page's
		// next_after is the ordered key tuple, each element serialized per its
		// column's type.
		laneCols := []ResponseColumn{
			{Name: "lane", PgType: "text"},
			{Name: "pos", PgType: "int"},
			{Name: "pipeline", PgType: "text"},
		}
		key, ok := CollectionKey(CollectionLanes)
		if !ok {
			t.Fatalf("CollectionKey(lanes): not in roster")
		}
		cursor := CursorPlan{Key: key, Limit: 1}
		rows := []map[string]any{
			{"lane": "ingest", "pos": int32(3), "pipeline": "load_orders"},
		}
		got, err := RenderPage(laneCols, rows, cursor)
		if err != nil {
			t.Fatalf("RenderPage: %v", err)
		}
		want := `{"data":[{"lane":"ingest","pos":3,"pipeline":"load_orders"}],"page":{"next_after":["ingest",3],"limit":1}}`
		if string(got) != want {
			t.Fatalf("RenderPage:\n got %s\nwant %s", got, want)
		}
	})

	t.Run("missing-column-is-an-error", func(t *testing.T) {
		// A row that does not carry every response column is a rendering fault
		// (rows mirror source columns), surfaced as an error, never a hole.
		cursor := CursorPlan{Key: EndpointCursorKey("id"), Limit: 100}
		bad := map[string]any{"id": int64(1)}
		if _, err := RenderPage(cols, []map[string]any{bad}, cursor); err == nil {
			t.Fatalf("RenderPage: expected an error for a row missing columns")
		}
	})

	t.Run("key-column-outside-shape-is-an-error", func(t *testing.T) {
		// next_after serializes per the key column's type; a cursor key outside
		// the response shape is a route misconfiguration, not a silent guess.
		cursor := CursorPlan{Key: EndpointCursorKey("uid"), Limit: 1}
		if _, err := RenderPage(cols, []map[string]any{row1}, cursor); err == nil {
			t.Fatalf("RenderPage: expected an error for a key column outside the shape")
		}
	})
}

// TestColumnTypeSerialization pins the per-column-type JSON forms:
// int/bigint/smallint/double as JSON numbers, numeric as a string (no float
// loss), bool as a JSON boolean, text/varchar/uuid as strings,
// timestamptz/timestamp/date/time as RFC 3339 strings, json/jsonb inline, bytea
// as base64, SQL NULL as JSON null, and recorded_at audit strings opaque:
// emitted verbatim, never parsed or interpreted for ordering.
func TestColumnTypeSerialization(t *testing.T) {
	cases := []struct {
		name   string
		pgType string
		value  any
		want   string // the exact JSON fragment for the value
	}{
		{"int-number", "int", int32(7), `7`},
		{"integer-number", "integer", int32(-12), `-12`},
		{"bigint-number", "bigint", int64(9007199254740993), `9007199254740993`}, // above float64 precision: no round-trip loss
		{"smallint-number", "smallint", int16(-3), `-3`},
		{"double-number", "double precision", float64(2.5), `2.5`},
		{"numeric-string", "numeric", "12345678901234567890.12345", `"12345678901234567890.12345"`},
		{"numeric-parametrized-string", "numeric(10,2)", "19.99", `"19.99"`},
		{"bool-true", "boolean", true, `true`},
		{"bool-false", "boolean", false, `false`},
		{"text-string", "text", "plain", `"plain"`},
		{"varchar-string", "varchar(255)", "v", `"v"`},
		{"uuid-string", "uuid", "0a68e17c-9915-4b26-a584-1c979be62b19", `"0a68e17c-9915-4b26-a584-1c979be62b19"`},
		{"timestamptz-rfc3339", "timestamptz", time.Date(2026, 7, 4, 12, 0, 0, 500000000, time.UTC), `"2026-07-04T12:00:00.5Z"`},
		{"timestamptz-offset", "timestamptz", time.Date(2026, 7, 4, 12, 0, 0, 0, time.FixedZone("", 2*3600)), `"2026-07-04T12:00:00+02:00"`},
		{"timestamp-rfc3339", "timestamp", time.Date(2026, 7, 4, 12, 0, 0, 0, time.UTC), `"2026-07-04T12:00:00Z"`},
		{"date-rfc3339", "date", time.Date(2026, 7, 4, 0, 0, 0, 0, time.UTC), `"2026-07-04"`},
		{"time-rfc3339", "time", time.Date(0, 1, 1, 12, 34, 56, 0, time.UTC), `"12:34:56"`},
		{"time-fractional", "time", time.Date(0, 1, 1, 12, 34, 56, 250000000, time.UTC), `"12:34:56.25"`},
		{"json-inline", "json", json.RawMessage(`{"a": 1, "b": [true, null]}`), `{"a":1,"b":[true,null]}`},
		{"jsonb-inline", "jsonb", json.RawMessage(`[1, 2, 3]`), `[1,2,3]`},
		{"jsonb-string-inline", "jsonb", `{"nested": "x"}`, `{"nested":"x"}`},
		{"bytea-base64", "bytea", []byte("iris"), `"aXJpcw=="`},
		{"null-text", "text", nil, `null`},
		{"null-bigint", "bigint", nil, `null`},
		{"null-jsonb", "jsonb", nil, `null`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := EncodeRow([]ResponseColumn{{Name: "v", PgType: tc.pgType}}, map[string]any{"v": tc.value})
			if err != nil {
				t.Fatalf("EncodeRow(%s, %v): %v", tc.pgType, tc.value, err)
			}
			want := `{"v":` + tc.want + `}`
			if string(got) != want {
				t.Fatalf("EncodeRow(%s):\n got %s\nwant %s", tc.pgType, got, want)
			}
		})
	}

	t.Run("recorded-at-opaque", func(t *testing.T) {
		// recorded_at is an opaque audit string (log correlation only): it is
		// emitted verbatim as a JSON string even when it looks timestamp-ish, and
		// is never parsed, normalized, or interpreted for ordering.
		raw := "2026-07-04 12:00:00.123456+00 boot=42"
		got, err := EncodeRow(
			[]ResponseColumn{{Name: "recorded_at", PgType: "text"}},
			map[string]any{"recorded_at": raw},
		)
		if err != nil {
			t.Fatalf("EncodeRow(recorded_at): %v", err)
		}
		want := `{"recorded_at":"2026-07-04 12:00:00.123456+00 boot=42"}`
		if string(got) != want {
			t.Fatalf("EncodeRow(recorded_at):\n got %s\nwant %s", got, want)
		}
	})

	t.Run("type-mismatch-is-an-error", func(t *testing.T) {
		// A value whose Go type does not match its column's wire type is a
		// rendering fault, surfaced as an error naming the column, never coerced.
		mismatches := []struct {
			name   string
			pgType string
			value  any
		}{
			{"string-for-bigint", "bigint", "7"},
			{"float-for-int", "int", float64(7)},
			{"number-for-numeric", "numeric", float64(19.99)},
			{"string-for-bool", "boolean", "true"},
			{"int-for-text", "text", int64(1)},
			{"string-for-timestamptz", "timestamptz", "2026-07-04T12:00:00Z"},
			{"invalid-json-inline", "jsonb", json.RawMessage(`{"a":`)},
			{"string-for-bytea", "bytea", "aXJpcw=="},
		}
		for _, m := range mismatches {
			if _, err := EncodeRow([]ResponseColumn{{Name: "v", PgType: m.pgType}}, map[string]any{"v": m.value}); err == nil {
				t.Errorf("%s: EncodeRow(%s, %#v): expected an error", m.name, m.pgType, m.value)
			}
		}
	})

	t.Run("unsupported-type-is-an-error", func(t *testing.T) {
		if _, err := EncodeRow([]ResponseColumn{{Name: "v", PgType: "money"}}, map[string]any{"v": "1"}); err == nil {
			t.Fatalf("EncodeRow(money): expected an error for a type outside the closed set")
		}
	})
}

// TestErrorEnvelopeClosedCodes pins the error envelope: errors reuse the
// envelope with an error object carrying a code from the closed set {bad_param,
// unauthorized, forbidden, not_found, method_not_allowed, internal} and a
// human-readable message; a code outside the set is refused, never emitted; and
// each code maps to its fixed HTTP status.
func TestErrorEnvelopeClosedCodes(t *testing.T) {
	t.Run("envelope-shape", func(t *testing.T) {
		got, err := RenderError(CodeBadParam, `param "limit": exceeds the maximum of 1000`)
		if err != nil {
			t.Fatalf("RenderError: %v", err)
		}
		want := `{"error":{"code":"bad_param","message":"param \"limit\": exceeds the maximum of 1000"}}`
		if string(got) != want {
			t.Fatalf("RenderError:\n got %s\nwant %s", got, want)
		}
	})

	t.Run("closed-code-set", func(t *testing.T) {
		// Exactly these six codes render, each with its fixed HTTP status.
		codes := []struct {
			code   ErrorCode
			status int
		}{
			{CodeBadParam, 400},
			{CodeUnauthorized, 401},
			{CodeForbidden, 403},
			{CodeNotFound, 404},
			{CodeMethodNotAllowed, 405},
			{CodeInternal, 500},
		}
		for _, c := range codes {
			if !c.code.Valid() {
				t.Errorf("ErrorCode(%q).Valid() = false, want true", c.code)
			}
			if got := c.code.HTTPStatus(); got != c.status {
				t.Errorf("ErrorCode(%q).HTTPStatus() = %d, want %d", c.code, got, c.status)
			}
			out, err := RenderError(c.code, "m")
			if err != nil {
				t.Errorf("RenderError(%q): %v", c.code, err)
				continue
			}
			want := `{"error":{"code":"` + string(c.code) + `","message":"m"}}`
			if string(out) != want {
				t.Errorf("RenderError(%q):\n got %s\nwant %s", c.code, out, want)
			}
		}
	})

	t.Run("out-of-set-code-refused", func(t *testing.T) {
		for _, bad := range []ErrorCode{"", "teapot", "conflict", "BAD_PARAM", "bad param"} {
			if bad.Valid() {
				t.Errorf("ErrorCode(%q).Valid() = true, want false", bad)
			}
			if _, err := RenderError(bad, "m"); err == nil {
				t.Errorf("RenderError(%q): expected an error for a code outside the closed set", bad)
			}
		}
	})
}
