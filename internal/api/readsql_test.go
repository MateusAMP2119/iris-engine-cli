package api

import (
	"net/url"
	"strings"
	"testing"

	"github.com/MateusAMP2119/iris-engine-cli/internal/declare"
)

// compileStatusEndpoint compiles a /q endpoint with a text filter column, so the
// SQL-safety tests can bind a hostile caller string that survives type parsing.
func compileStatusEndpoint(t *testing.T) *declare.CompiledEndpoint {
	t.Helper()
	src := []byte(`endpoint: orders_by_status
source: analytics.orders
fields: [id, status]
filters:
  status: eq
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
				{Name: "status", Type: "text"},
			},
		},
	}
	ce, err := declare.CompileEndpoint(ep, tables)
	if err != nil {
		t.Fatalf("compile endpoint: %v", err)
	}
	return ce
}

// hostileValue is a caller value shaped like a SQL injection: if any read path
// ever splices a request value into statement text, this string surfaces it.
const hostileValue = "x'; DROP TABLE meta.pats; --"

// TestNoCallerSQL proves the SQL-safety contract of specification section 7: the
// read surface executes only engine-built statements with bound params -- the
// compiled text for /q and text assembled from validated identifiers for /data --
// and caller input never becomes SQL text.
//
// spec: S07/no-caller-sql
func TestNoCallerSQL(t *testing.T) {
	t.Run("S07/no-caller-sql", func(t *testing.T) {
		t.Run("/q binds caller values into the compiled text, never splices them", func(t *testing.T) {
			ce := compileStatusEndpoint(t)
			compiled := ce.SQL

			plan, err := PlanEndpointQuery(ce, url.Values{"status": {hostileValue}, "limit": {"5"}})
			if err != nil {
				t.Fatalf("PlanEndpointQuery: %v", err)
			}
			args, err := BindArgs(ce.Params, plan)
			if err != nil {
				t.Fatalf("BindArgs: %v", err)
			}
			if ce.SQL != compiled {
				t.Error("binding a request mutated the compiled statement text")
			}
			if strings.Contains(ce.SQL, hostileValue) {
				t.Error("a caller value appeared in the compiled statement text")
			}
			if len(args) != len(ce.Params) {
				t.Fatalf("BindArgs returned %d args for %d slots", len(args), len(ce.Params))
			}
			if args[0] != hostileValue {
				t.Errorf("args[0] = %v, want the caller value bound to the status slot", args[0])
			}
			if args[1] != nil {
				t.Errorf("args[1] (after) = %v, want nil for an omitted cursor", args[1])
			}
			if args[2] != 5 {
				t.Errorf("args[2] (limit) = %v, want 5", args[2])
			}
		})

		t.Run("/data assembles a fixed text from validated identifiers; caller values only bind", func(t *testing.T) {
			fields := map[string]string{"id": "bigint", "status": "text"}
			ds, err := BuildDataStatement("analytics", "orders", []string{"id", "status"}, fields, []string{"id"})
			if err != nil {
				t.Fatalf("BuildDataStatement: %v", err)
			}

			plan, err := PlanDataQuery([]string{"id"}, fields, url.Values{
				"status": {hostileValue}, "status_from": {hostileValue}, "after": {"42"}, "limit": {"5"},
			})
			if err != nil {
				t.Fatalf("PlanDataQuery: %v", err)
			}
			args, err := BindArgs(ds.Params, plan)
			if err != nil {
				t.Fatalf("BindArgs: %v", err)
			}

			if strings.Contains(ds.SQL, hostileValue) {
				t.Error("a caller value appeared in the assembled /data statement text")
			}
			for _, want := range []string{"SELECT id, status", "FROM analytics.orders", "ORDER BY id ASC"} {
				if !strings.Contains(ds.SQL, want) {
					t.Errorf("/data SQL missing %q:\n%s", want, ds.SQL)
				}
			}
			// Slots in deterministic order, one eq plus one inclusive range pair per
			// filter column (the /data grammar is eq/range, specification section 7):
			// id eq, id_from, id_to, status eq, status_from, status_to, after, limit.
			if len(args) != 8 {
				t.Fatalf("BindArgs returned %d args, want 8", len(args))
			}
			for i, slot := range map[int]string{0: "id eq", 1: "id_from", 2: "id_to", 5: "status_to"} {
				if args[i] != nil {
					t.Errorf("args[%d] (%s, omitted) = %v, want nil", i, slot, args[i])
				}
			}
			if args[3] != hostileValue {
				t.Errorf("args[3] (status eq) = %v, want the caller value", args[3])
			}
			if args[4] != hostileValue {
				t.Errorf("args[4] (status_from) = %v, want the caller value bound, never spliced", args[4])
			}
			if args[6] != int64(42) {
				t.Errorf("args[6] (after) = %v, want int64(42)", args[6])
			}
			if args[7] != 5 {
				t.Errorf("args[7] (limit) = %v, want 5", args[7])
			}
		})

		t.Run("/data statement text is deterministic for one shape", func(t *testing.T) {
			fields := map[string]string{"id": "bigint", "status": "text"}
			a, err := BuildDataStatement("analytics", "orders", []string{"id", "status"}, fields, []string{"id"})
			if err != nil {
				t.Fatalf("BuildDataStatement: %v", err)
			}
			b, err := BuildDataStatement("analytics", "orders", []string{"id", "status"}, fields, []string{"id"})
			if err != nil {
				t.Fatalf("BuildDataStatement: %v", err)
			}
			if a.SQL != b.SQL || a.Name != b.Name {
				t.Errorf("identical shapes produced different statements:\n%s / %s\n%s / %s", a.Name, a.SQL, b.Name, b.SQL)
			}
		})

		t.Run("a hostile identifier never reaches statement text", func(t *testing.T) {
			ok := map[string]string{"id": "bigint"}
			cases := []struct {
				name          string
				schema, table string
				projection    []string
				fields        map[string]string
				pk            []string
			}{
				{"schema with statement splice", "analytics; DROP TABLE meta.pats", "orders", []string{"id"}, ok, []string{"id"}},
				{"quoted table", `orders"; --`, "orders", []string{"id"}, ok, []string{"id"}},
				{"table with comment", "analytics", "orders--", []string{"id"}, ok, []string{"id"}},
				{"projection with comma splice", "analytics", "orders", []string{"id, secret"}, ok, []string{"id"}},
				{"uppercase identifier", "Analytics", "orders", []string{"id"}, ok, []string{"id"}},
				{"filter column with quote", "analytics", "orders", []string{"id"}, map[string]string{`amount"`: "numeric", "id": "bigint"}, []string{"id"}},
				{"hostile filter type", "analytics", "orders", []string{"id"}, map[string]string{"id": "bigint; DROP TABLE meta.pats"}, []string{"id"}},
				{"pk with splice", "analytics", "orders", []string{"id"}, ok, []string{"id;"}},
				{"empty projection", "analytics", "orders", nil, ok, []string{"id"}},
				{"empty pk", "analytics", "orders", []string{"id"}, ok, nil},
				{"pk column without a known type", "analytics", "orders", []string{"id"}, map[string]string{"status": "text"}, []string{"id"}},
			}
			for _, tc := range cases {
				t.Run(tc.name, func(t *testing.T) {
					if _, err := BuildDataStatement(tc.schema, tc.table, tc.projection, tc.fields, tc.pk); err == nil {
						t.Errorf("BuildDataStatement accepted a hostile shape, want a refusal")
					}
				})
			}
		})

		t.Run("binding refuses a plan the statement cannot carry", func(t *testing.T) {
			ce := compileStatusEndpoint(t)

			// A predicate on a column no slot binds would silently drop a filter.
			stray := &QueryPlan{
				Predicates: []Predicate{{Column: "amount", Op: OpEq, Value: 1}},
				Cursor:     CursorPlan{Key: EndpointCursorKey("id"), Limit: 10},
			}
			if _, err := BindArgs(ce.Params, stray); err == nil {
				t.Error("BindArgs bound a plan with a predicate no slot carries, want a refusal")
			}

			// A descending cursor has no slot in an ascending prepared text.
			desc := &QueryPlan{
				Cursor: CursorPlan{
					Key: EndpointCursorKey("id"), Limit: 10, Descending: true,
					Bound: &CursorBound{Op: OpLt, Value: int64(9)},
				},
			}
			if _, err := BindArgs(ce.Params, desc); err == nil {
				t.Error("BindArgs bound a descending cursor into an ascending statement, want a refusal")
			}

			if _, err := BindArgs(ce.Params, nil); err == nil {
				t.Error("BindArgs accepted a nil plan, want a refusal")
			}
		})
	})
}

// TestDataSurfaceCannotAddressMeta proves the database-split half of specification
// section 7 at the statement layer: every identifier the /data assembler accepts is
// a single bare name, so no statement can carry a database-qualified reference --
// meta, a separate database, is unaddressable from the data surface.
//
// spec: S07/engine-storage-unreachable
func TestDataSurfaceCannotAddressMeta(t *testing.T) {
	t.Run("S07/engine-storage-unreachable", func(t *testing.T) {
		ok := map[string]string{"id": "bigint"}
		cases := []struct {
			name          string
			schema, table string
			projection    []string
			fields        map[string]string
			pk            []string
		}{
			{"database-qualified schema", "meta.public", "pats", []string{"id"}, ok, []string{"id"}},
			{"database-qualified table", "analytics", "meta.pats", []string{"id"}, ok, []string{"id"}},
			{"database-qualified projection column", "analytics", "orders", []string{"meta.pats.token_hash"}, ok, []string{"id"}},
			{"database-qualified filter column", "analytics", "orders", []string{"id"}, map[string]string{"meta.pats.id": "bigint", "id": "bigint"}, []string{"id"}},
			{"database-qualified pk", "analytics", "orders", []string{"id"}, ok, []string{"meta.pats.id"}},
		}
		for _, tc := range cases {
			t.Run(tc.name, func(t *testing.T) {
				if _, err := BuildDataStatement(tc.schema, tc.table, tc.projection, tc.fields, tc.pk); err == nil {
					t.Errorf("BuildDataStatement accepted a database-qualified identifier, want a refusal")
				}
			})
		}

		t.Run("the assembled reference is exactly schema.table inside the connected database", func(t *testing.T) {
			ds, err := BuildDataStatement("analytics", "orders", []string{"id"}, map[string]string{"id": "bigint"}, []string{"id"})
			if err != nil {
				t.Fatalf("BuildDataStatement: %v", err)
			}
			if !strings.Contains(ds.SQL, "FROM analytics.orders") {
				t.Errorf("SQL does not reference the bare schema.table:\n%s", ds.SQL)
			}
			for _, line := range strings.Split(ds.SQL, "\n") {
				if strings.HasPrefix(line, "FROM ") && strings.Count(line, ".") != 1 {
					t.Errorf("FROM clause is not exactly schema.table: %q", line)
				}
			}
		})
	})
}
