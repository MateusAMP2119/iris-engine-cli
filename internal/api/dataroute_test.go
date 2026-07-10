package api_test

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/MateusAMP2119/iris-engine-cli/internal/api"
	"github.com/MateusAMP2119/iris-engine-cli/internal/pat"
	"github.com/MateusAMP2119/iris-engine-cli/internal/store"
)

// This file proves the raw /data route of specification section 7 at the mux
// tier with a recording executor fake (integration, no live Postgres): column
// projection, eq/range filters, keyset paging by the table PK, the strict wire
// grammar, and the guarantee that no data-surface route filters disposable rows
// out. The executor records exactly the fixed statement text, the bound args,
// and the execution role it was handed, so every assertion is about what would
// reach the shared read pool.

// recordedRead is one executor call: the execution identity and the exact
// statement the route handed the pool seam.
type recordedRead struct {
	role    string
	self    bool
	name    string
	sql     string
	args    []any
	columns []string
}

// fakeExecutor is a recording api.ReadExecutor: it captures every call and
// answers scripted rows or a scripted error.
type fakeExecutor struct {
	mu    sync.Mutex
	calls []recordedRead
	rows  []map[string]any
	err   error
}

func (f *fakeExecutor) record(c recordedRead) ([]map[string]any, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, c)
	if f.err != nil {
		return nil, f.err
	}
	return f.rows, nil
}

func (f *fakeExecutor) ExecuteRead(_ context.Context, role, name, text string, args []any, columns []string) ([]map[string]any, error) {
	return f.record(recordedRead{role: role, name: name, sql: text, args: args, columns: columns})
}

func (f *fakeExecutor) ExecuteReadSelf(_ context.Context, name, text string, args []any, columns []string) ([]map[string]any, error) {
	return f.record(recordedRead{self: true, name: name, sql: text, args: args, columns: columns})
}

// last returns the most recent recorded call.
func (f *fakeExecutor) last(t *testing.T) recordedRead {
	t.Helper()
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.calls) == 0 {
		t.Fatal("the executor was never called")
	}
	return f.calls[len(f.calls)-1]
}

// count returns how many reads the executor served.
func (f *fakeExecutor) count() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.calls)
}

// mapDataSource is a fake api.DataSource over a fixed shape set.
type mapDataSource map[string]*api.DataShape

func (m mapDataSource) DataShape(schema, table string) (*api.DataShape, bool) {
	s, ok := m[schema+"."+table]
	return s, ok
}

// ordersShape is the declared table the /data tests read: a five-column shape
// with a single-column bigint primary key.
func ordersShape() *api.DataShape {
	return &api.DataShape{
		Schema: "analytics",
		Table:  "orders",
		Columns: []api.ResponseColumn{
			{Name: "id", PgType: "bigint"},
			{Name: "customer_id", PgType: "uuid"},
			{Name: "amount", PgType: "numeric"},
			{Name: "status", PgType: "text"},
			{Name: "created_at", PgType: "timestamptz"},
		},
		PrimaryKey: []string{"id"},
	}
}

// dataMux builds a leader-role mux with the /data seams wired to the fakes (a
// non-GET must reach the route's own 405, not the standby mutation gate).
func dataMux(exec *fakeExecutor) http.Handler {
	return leaderMux(
		api.WithDataSource(mapDataSource{"analytics.orders": ordersShape()}),
		api.WithReadExecutor(exec),
	)
}

// dataPAT is a data-scope authority carrying its engine-managed read role.
func dataPAT(id, role string) api.Authority {
	return api.Authority{PATID: id, Scopes: []pat.Scope{pat.ScopeData}, DataRole: role}
}

// doGet performs one request as the given authority and decodes the envelope.
func doGet(t *testing.T, h http.Handler, method, path string, a api.Authority) (int, qEnvelope) {
	t.Helper()
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(method, path, nil)
	req = req.WithContext(api.WithAuthority(req.Context(), a))
	h.ServeHTTP(rec, req)
	var env qEnvelope
	if err := json.Unmarshal(rec.Body.Bytes(), &env); err != nil {
		t.Fatalf("%s %s: body is not a JSON envelope: %v (%q)", method, path, err, rec.Body.String())
	}
	return rec.Code, env
}

// containsArg reports whether args carries a value equal to want.
func containsArg(args []any, want any) bool {
	for _, a := range args {
		if reflect.DeepEqual(a, want) {
			return true
		}
	}
	return false
}

// TestDataRawRoute proves /data/{schema}/{table} of specification section 7:
// ad-hoc reads with column projection, eq and range filters bound as params
// into one fixed statement, keyset paging by the table PK, and the strict wire
// grammar and status matrix around them.
//
// spec: S07/data-raw-route
func TestDataRawRoute(t *testing.T) {
	t.Run("S07/data-raw-route", func(t *testing.T) {
		alice := dataPAT("a1", "iris_pat_r_alice")

		t.Run("serves the declared projection by default, ordered by the PK", func(t *testing.T) {
			exec := &fakeExecutor{rows: []map[string]any{
				{"id": int64(1), "customer_id": "c1", "amount": "10", "status": "paid", "created_at": "t1"},
				{"id": int64(2), "customer_id": "c2", "amount": "20", "status": "open", "created_at": "t2"},
			}}
			code, env := doGet(t, dataMux(exec), http.MethodGet, "/data/analytics/orders", alice)
			if code != http.StatusOK || env.Error != nil {
				t.Fatalf("GET /data/analytics/orders = %d %+v, want 200", code, env.Error)
			}
			if len(env.Data) != 2 || env.Data[0]["id"] != float64(1) || env.Data[1]["status"] != "open" {
				t.Errorf("data = %v, want the executor's two rows", env.Data)
			}
			if env.Page == nil || env.Page.Limit != api.DefaultLimit || env.Page.NextAfter != nil {
				t.Errorf("page = %+v, want limit %d and next_after null (a short page)", env.Page, api.DefaultLimit)
			}
			call := exec.last(t)
			if call.role != "iris_pat_r_alice" || call.self {
				t.Errorf("executed as (role=%q, self=%v), want the calling PAT's role", call.role, call.self)
			}
			if want := []string{"id", "customer_id", "amount", "status", "created_at"}; !reflect.DeepEqual(call.columns, want) {
				t.Errorf("projection = %v, want the full declared column list %v", call.columns, want)
			}
			for _, frag := range []string{"SELECT id, customer_id, amount, status, created_at", "FROM analytics.orders", "ORDER BY id ASC"} {
				if !strings.Contains(call.sql, frag) {
					t.Errorf("statement missing %q:\n%s", frag, call.sql)
				}
			}
		})

		t.Run("columns= projects the requested columns in caller order", func(t *testing.T) {
			exec := &fakeExecutor{rows: []map[string]any{{"id": int64(1), "amount": "10"}}}
			code, _ := doGet(t, dataMux(exec), http.MethodGet, "/data/analytics/orders?columns=id,amount", alice)
			if code != http.StatusOK {
				t.Fatalf("projected GET = %d, want 200", code)
			}
			call := exec.last(t)
			if want := []string{"id", "amount"}; !reflect.DeepEqual(call.columns, want) {
				t.Errorf("projection = %v, want %v", call.columns, want)
			}
			if !strings.Contains(call.sql, "SELECT id, amount") {
				t.Errorf("statement does not project the requested columns:\n%s", call.sql)
			}
		})

		t.Run("eq and range filters bind as params, never as statement text", func(t *testing.T) {
			exec := &fakeExecutor{}
			from := "2026-01-01T00:00:00Z"
			code, _ := doGet(t, dataMux(exec), http.MethodGet,
				"/data/analytics/orders?status=paid&created_at_from="+from+"&amount_to=99.5", alice)
			if code != http.StatusOK {
				t.Fatalf("filtered GET = %d, want 200", code)
			}
			call := exec.last(t)
			if strings.Contains(call.sql, "paid") || strings.Contains(call.sql, "99.5") {
				t.Errorf("a caller value reached statement text:\n%s", call.sql)
			}
			if !containsArg(call.args, "paid") {
				t.Errorf("args %v missing the eq filter value \"paid\"", call.args)
			}
			wantFrom, err := time.Parse(time.RFC3339, from)
			if err != nil {
				t.Fatal(err)
			}
			if !containsArg(call.args, wantFrom) {
				t.Errorf("args %v missing the parsed range lower bound %v", call.args, wantFrom)
			}
			if !containsArg(call.args, "99.5") {
				t.Errorf("args %v missing the numeric range upper bound", call.args)
			}
		})

		t.Run("keyset paging by the PK: after binds the cursor, a full page returns next_after", func(t *testing.T) {
			exec := &fakeExecutor{rows: []map[string]any{{"id": int64(5)}, {"id": int64(6)}}}
			code, env := doGet(t, dataMux(exec), http.MethodGet, "/data/analytics/orders?columns=id&after=4&limit=2", alice)
			if code != http.StatusOK {
				t.Fatalf("paged GET = %d, want 200", code)
			}
			call := exec.last(t)
			if !containsArg(call.args, int64(4)) {
				t.Errorf("args %v missing the after cursor value", call.args)
			}
			if !containsArg(call.args, 2) {
				t.Errorf("args %v missing the limit", call.args)
			}
			if env.Page == nil || env.Page.NextAfter != float64(6) || env.Page.Limit != 2 {
				t.Errorf("page = %+v, want next_after 6 and limit 2 (a full page continues)", env.Page)
			}
		})

		t.Run("the wire grammar refuses with a 400 naming the param", func(t *testing.T) {
			for name, query := range map[string]string{
				"unknown param":            "?bogus=1",
				"repeated param":           "?status=a&status=b",
				"over-cap limit":           "?limit=1001",
				"unparseable typed value":  "?id=not-a-bigint",
				"unknown projected column": "?columns=id,nope",
				"repeated columns param":   "?columns=id&columns=amount",
				"projection without PK":    "?columns=amount",
			} {
				t.Run(name, func(t *testing.T) {
					exec := &fakeExecutor{}
					code, env := doGet(t, dataMux(exec), http.MethodGet, "/data/analytics/orders"+query, alice)
					if code != http.StatusBadRequest || env.Error == nil || env.Error.Code != "bad_param" {
						t.Errorf("GET %s = %d %+v, want 400 bad_param", query, code, env.Error)
					}
					if exec.count() != 0 {
						t.Errorf("a refused request reached the executor")
					}
				})
			}
		})

		t.Run("status matrix: 404 unknown table, 405 non-GET, 500 unwired", func(t *testing.T) {
			exec := &fakeExecutor{}
			mux := dataMux(exec)
			if code, env := doGet(t, mux, http.MethodGet, "/data/analytics/nope", alice); code != http.StatusNotFound || env.Error == nil || env.Error.Code != "not_found" {
				t.Errorf("unknown table = %d %+v, want 404 not_found", code, env.Error)
			}
			if code, env := doGet(t, mux, http.MethodPost, "/data/analytics/orders", alice); code != http.StatusMethodNotAllowed || env.Error == nil || env.Error.Code != "method_not_allowed" {
				t.Errorf("POST = %d %+v, want 405 method_not_allowed", code, env.Error)
			}
			if code, env := doGet(t, api.NewMux(), http.MethodGet, "/data/analytics/orders", alice); code != http.StatusInternalServerError || env.Error == nil || env.Error.Code != "internal" {
				t.Errorf("unwired /data = %d %+v, want the 500 internal envelope", code, env.Error)
			}
		})

		t.Run("a grant refusal surfaces as 403 forbidden, never the Postgres text", func(t *testing.T) {
			exec := &fakeExecutor{err: fmt.Errorf("execute: %w: ERROR: permission denied for table orders", store.ErrReadForbidden)}
			code, env := doGet(t, dataMux(exec), http.MethodGet, "/data/analytics/orders", alice)
			if code != http.StatusForbidden || env.Error == nil || env.Error.Code != "forbidden" {
				t.Fatalf("refused GET = %d %+v, want 403 forbidden", code, env.Error)
			}
			if !strings.Contains(env.Error.Message, "analytics.orders") {
				t.Errorf("message %q does not name the addressed table", env.Error.Message)
			}
			if strings.Contains(env.Error.Message, "permission denied") {
				t.Errorf("message %q leaks the Postgres error text", env.Error.Message)
			}
		})
	})
}

// TestDisposableRowsVisible proves the disposable-rows caveat of specification
// section 7: data-surface reads return disposable rows unfiltered -- no route
// adds a predicate to hide them, and every row the executor serves reaches the
// wire untouched, on /data and /q alike.
//
// spec: S07/disposable-rows-visible
func TestDisposableRowsVisible(t *testing.T) {
	t.Run("S07/disposable-rows-visible", func(t *testing.T) {
		alice := dataPAT("a1", "iris_pat_r_alice")
		// Three rows, of which two are un-promoted disposable workspace rows in
		// the journal's eyes. The data surface cannot tell and must not try.
		rows := []map[string]any{
			{"id": int64(1), "status": "promoted"},
			{"id": int64(2), "status": "disposable"},
			{"id": int64(3), "status": "disposable"},
		}

		t.Run("/data serves every row the statement returns, and the statement filters nothing", func(t *testing.T) {
			exec := &fakeExecutor{rows: rows}
			code, env := doGet(t, dataMux(exec), http.MethodGet, "/data/analytics/orders?columns=id,status", alice)
			if code != http.StatusOK {
				t.Fatalf("GET = %d, want 200", code)
			}
			if len(env.Data) != len(rows) {
				t.Fatalf("served %d rows, want all %d (disposable rows are never filtered)", len(env.Data), len(rows))
			}
			for i, want := range rows {
				if env.Data[i]["id"] != float64(want["id"].(int64)) || env.Data[i]["status"] != want["status"] {
					t.Errorf("row %d = %v, want %v untouched", i, env.Data[i], want)
				}
			}

			// The route's statement is exactly the engine-assembled shape statement:
			// the same fixed text BuildDataStatement derives for an unfiltered read
			// (only the PK paging slots), with no extra predicate (no disposable,
			// promotion, or journal filter smuggled in).
			call := exec.last(t)
			shape := ordersShape()
			ds, err := api.BuildDataStatement(shape.Schema, shape.Table, []string{"id", "status"},
				map[string]string{"id": "bigint"}, shape.PrimaryKey)
			if err != nil {
				t.Fatalf("BuildDataStatement: %v", err)
			}
			if call.sql != ds.SQL {
				t.Errorf("route statement diverges from the engine-assembled shape statement:\nroute:\n%s\nshape:\n%s", call.sql, ds.SQL)
			}
			for _, banned := range []string{"disposable", "promoted", "journal", "iris_provenance"} {
				if strings.Contains(strings.ToLower(call.sql), banned) {
					t.Errorf("statement carries a %q filter; the data surface never filters disposable rows:\n%s", banned, call.sql)
				}
			}
		})

		t.Run("/q serves every row the executor returns", func(t *testing.T) {
			exec := &fakeExecutor{rows: rows}
			ce := compileQEndpoint(t, `endpoint: orders_by_customer
source: analytics.orders
fields: [id, customer_id]
filters:
  customer_id: eq
sort: id
`)
			mux := api.NewMux(
				api.WithEndpoints(mapEndpointSource{ce.Name: ce}),
				api.WithEndpointReader(api.NewPoolReader(exec)),
			)
			code, env := doGet(t, mux, http.MethodGet, "/q/orders_by_customer", alice)
			if code != http.StatusOK {
				t.Fatalf("GET /q = %d, want 200", code)
			}
			if len(env.Data) != len(rows) {
				t.Fatalf("/q served %d rows, want all %d (disposable rows are never filtered)", len(env.Data), len(rows))
			}
			call := exec.last(t)
			if call.sql != ce.SQL {
				t.Errorf("/q executed a statement other than the compiled endpoint text:\n%s", call.sql)
			}
		})
	})
}
