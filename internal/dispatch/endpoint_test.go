package dispatch_test

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"

	"github.com/MateusAMP2119/iris-engine-cli/internal/declare"
	"github.com/MateusAMP2119/iris-engine-cli/internal/dispatch"
	"github.com/MateusAMP2119/iris-engine-cli/internal/store/storetest"
)

// This file proves the dispatch half of the endpoint apply lifecycle
// (specification section 7): iris endpoint apply prepare-verifies the derived
// SQL against the data database and persists to endpoints + endpoint_filters
// atomically through the single meta writer, all-or-nothing; and the lifecycle
// runs independently of declare apply and declare destroy. Every write rides a
// real Dispatcher over a recording fake -- no live Postgres -- so a test
// asserts the exact write set, its transaction grouping, and that a failed
// verify or a rolled-back commit changes nothing, neither meta nor the live
// serving registry.

// ordersByCustomerYAML is the canonical spec-section-7 endpoint, compiled
// against the analytics.orders source below.
const ordersByCustomerYAML = `endpoint: orders_by_customer
source: analytics.orders
fields: [id, customer_id, amount]
filters:
  customer_id: eq
  created_at: range
sort: id
`

// ordersRecentYAML is a second endpoint over the same source, for the
// multi-endpoint all-or-nothing assertions.
const ordersRecentYAML = `endpoint: orders_recent
source: analytics.orders
fields: [id, created_at]
filters:
  created_at: range
sort: id
`

// endpointTables is the declared schemas/ set the test endpoints compile
// against: analytics.orders with a unique primary key id (a valid sort).
func endpointTables() map[string]*declare.Table {
	return map[string]*declare.Table{"analytics.orders": {
		Schema: "analytics",
		Table:  "orders",
		Columns: []declare.Column{
			{Name: "id", Type: "uuid", PrimaryKey: true},
			{Name: "customer_id", Type: "uuid"},
			{Name: "amount", Type: "numeric"},
			{Name: "created_at", Type: "timestamptz"},
		},
	}}
}

// compileTestEndpoint parses and compiles one endpoint YAML document against the
// test source table, exactly the E09.2 compile path the apply consumes.
func compileTestEndpoint(t *testing.T, doc string) *declare.CompiledEndpoint {
	t.Helper()
	ep, err := declare.ParseEndpoint([]byte(doc))
	if err != nil {
		t.Fatalf("parse endpoint: %v", err)
	}
	ce, err := declare.CompileEndpoint(ep, endpointTables())
	if err != nil {
		t.Fatalf("compile endpoint: %v", err)
	}
	return ce
}

// verifyCall is one recorded prepare-verify invocation: the endpoint and the
// derived SQL text it presented to the data database.
type verifyCall struct {
	name string
	sql  string
}

// fakeVerifier is a recording dispatch.PrepareVerifier with injectable
// per-endpoint failures, standing in for the data database's PREPARE.
type fakeVerifier struct {
	mu    sync.Mutex
	calls []verifyCall
	fail  map[string]error
}

func (v *fakeVerifier) PrepareVerify(_ context.Context, name, sql string) error {
	v.mu.Lock()
	defer v.mu.Unlock()
	v.calls = append(v.calls, verifyCall{name: name, sql: sql})
	return v.fail[name]
}

// Calls returns a copy of the recorded verify invocations, in call order.
func (v *fakeVerifier) Calls() []verifyCall {
	v.mu.Lock()
	defer v.mu.Unlock()
	return append([]verifyCall(nil), v.calls...)
}

// endpointHarness wires an EndpointApplier over a real Dispatcher (the
// single-writer path), a recording write connection, a recording verifier, and
// the live serving registry.
type endpointHarness struct {
	applier *dispatch.EndpointApplier
	live    *dispatch.EndpointRegistry
	rec     *storetest.WriteRecorder
	verify  *fakeVerifier
	d       *dispatch.Dispatcher
}

func newEndpointHarness(t *testing.T) endpointHarness {
	t.Helper()
	rec := storetest.NewWriteRecorder()
	d := dispatch.New(rec)
	d.Start(context.Background())
	t.Cleanup(d.Stop)
	verify := &fakeVerifier{fail: map[string]error{}}
	live := dispatch.NewEndpointRegistry()
	return endpointHarness{
		applier: dispatch.NewEndpointApplier(verify, d, live),
		live:    live,
		rec:     rec,
		verify:  verify,
		d:       d,
	}
}

// TestEndpointApplyVerifyPersist proves iris endpoint apply prepare-verifies the
// derived SQL against the data database and persists to endpoints +
// endpoint_filters atomically, all-or-nothing (specification section 7): the
// verifier sees each endpoint's one derived statement before any write, the
// whole apply commits as exactly one meta transaction (shape row plus filter
// rows, multi-endpoint applies included), and a verify failure or a rolled-back
// commit leaves meta untouched and publishes nothing.
//
// spec: S07/endpoint-apply-verify-persist
func TestEndpointApplyVerifyPersist(t *testing.T) {
	ctx := context.Background()

	t.Run("prepare-verify sees the derived SQL and the apply is one atomic transaction", func(t *testing.T) {
		h := newEndpointHarness(t)
		ce := compileTestEndpoint(t, ordersByCustomerYAML)

		if err := h.applier.Apply(ctx, []*declare.CompiledEndpoint{ce}); err != nil {
			t.Fatalf("Apply: %v", err)
		}

		calls := h.verify.Calls()
		if len(calls) != 1 || calls[0].name != "orders_by_customer" || calls[0].sql != ce.SQL {
			t.Fatalf("prepare-verify calls = %+v, want one call presenting the derived SQL of orders_by_customer", calls)
		}

		txns := h.rec.Transactions()
		if len(txns) != 1 {
			t.Fatalf("apply committed %d transactions, want exactly 1 (atomic, all-or-nothing)", len(txns))
		}
		batch := txns[0]
		// The shape row: endpoints keyed by name, with the dotted source, the JSON
		// projection, and the keyset sort.
		if len(batch) != 4 {
			t.Fatalf("apply transaction has %d statements, want 4 (endpoints upsert, filters delete, two filter inserts):\n%+v", len(batch), batch)
		}
		up := batch[0]
		if !strings.Contains(up.SQL, "INSERT INTO endpoints") {
			t.Errorf("statement 0 = %q, want the endpoints upsert", up.SQL)
		}
		if len(up.Args) != 4 || up.Args[0] != "orders_by_customer" || up.Args[1] != "analytics.orders" || up.Args[3] != "id" {
			t.Errorf("endpoints upsert args = %v, want [orders_by_customer analytics.orders <fields json> id]", up.Args)
		}
		if fields, ok := up.Args[2].(string); !ok || !strings.Contains(fields, "customer_id") {
			t.Errorf("endpoints.fields arg = %v, want the JSON projection naming customer_id", up.Args[2])
		}
		if !strings.Contains(batch[1].SQL, "DELETE FROM endpoint_filters") {
			t.Errorf("statement 1 = %q, want the clearing endpoint_filters delete", batch[1].SQL)
		}
		// Filter rows in declaration order: customer_id eq, then created_at range.
		wantFilters := [][]any{
			{"orders_by_customer", "customer_id", "eq"},
			{"orders_by_customer", "created_at", "range"},
		}
		for i, want := range wantFilters {
			s := batch[2+i]
			if !strings.Contains(s.SQL, "INSERT INTO endpoint_filters") {
				t.Errorf("statement %d = %q, want an endpoint_filters insert", 2+i, s.SQL)
				continue
			}
			if len(s.Args) != 3 || s.Args[0] != want[0] || s.Args[1] != want[1] || s.Args[2] != want[2] {
				t.Errorf("filter insert %d args = %v, want %v", i, s.Args, want)
			}
		}
	})

	t.Run("a multi-endpoint apply is one all-or-nothing transaction", func(t *testing.T) {
		h := newEndpointHarness(t)
		a := compileTestEndpoint(t, ordersByCustomerYAML)
		b := compileTestEndpoint(t, ordersRecentYAML)

		if err := h.applier.Apply(ctx, []*declare.CompiledEndpoint{a, b}); err != nil {
			t.Fatalf("Apply: %v", err)
		}
		txns := h.rec.Transactions()
		if len(txns) != 1 {
			t.Fatalf("two-endpoint apply committed %d transactions, want exactly 1", len(txns))
		}
		if !containsToken(txns[0], "orders_by_customer") || !containsToken(txns[0], "orders_recent") {
			t.Errorf("the single transaction is missing an endpoint's rows:\n%+v", txns[0])
		}
	})

	t.Run("a verify failure returns before any write and publishes nothing", func(t *testing.T) {
		h := newEndpointHarness(t)
		good := compileTestEndpoint(t, ordersByCustomerYAML)
		bad := compileTestEndpoint(t, ordersRecentYAML)
		h.verify.fail["orders_recent"] = errors.New(`relation "analytics.orders" does not exist`)

		err := h.applier.Apply(ctx, []*declare.CompiledEndpoint{good, bad})
		if err == nil || !strings.Contains(err.Error(), "orders_recent") {
			t.Fatalf("Apply error = %v, want a prepare-verify failure naming orders_recent", err)
		}
		if stmts := h.rec.Statements(); len(stmts) != 0 {
			t.Errorf("a failed verify still wrote %d statements, want 0 (all-or-nothing):\n%+v", len(stmts), stmts)
		}
		for _, name := range []string{"orders_by_customer", "orders_recent"} {
			if _, ok := h.live.Endpoint(name); ok {
				t.Errorf("endpoint %q went live although its apply failed verification", name)
			}
		}
	})

	t.Run("a rolled-back commit publishes nothing", func(t *testing.T) {
		h := newEndpointHarness(t)
		ce := compileTestEndpoint(t, ordersByCustomerYAML)
		h.rec.FailTx(errors.New("meta transaction aborted"))

		if err := h.applier.Apply(ctx, []*declare.CompiledEndpoint{ce}); err == nil {
			t.Fatal("Apply succeeded although the meta transaction rolled back")
		}
		if _, ok := h.live.Endpoint("orders_by_customer"); ok {
			t.Error("a rolled-back apply still published the shape; publish must follow the commit")
		}
	})
}

// TestEndpointLifecycleIndependent proves iris endpoint apply and iris endpoint
// remove publish and retire endpoints independently of declare apply, and that
// declare destroy leaves tables and endpoints standing (specification section
// 7): an endpoint applies against an empty workload registry and touches no
// workload table, remove retires only the endpoint rows, and a pipeline destroy
// issues no endpoint delete and never drops a table -- the applied endpoint
// keeps serving.
//
// spec: S07/endpoint-lifecycle-independent
func TestEndpointLifecycleIndependent(t *testing.T) {
	ctx := context.Background()

	t.Run("apply publishes with no declare apply and touches no workload table", func(t *testing.T) {
		h := newEndpointHarness(t)
		ce := compileTestEndpoint(t, ordersByCustomerYAML)

		// The workload registry is empty: no pipeline was ever declared or applied,
		// and the endpoint apply still publishes.
		if err := h.applier.Apply(ctx, []*declare.CompiledEndpoint{ce}); err != nil {
			t.Fatalf("Apply with no declared pipeline: %v", err)
		}
		if _, ok := h.live.Endpoint("orders_by_customer"); !ok {
			t.Fatal("applied endpoint is not live although no declare apply is required")
		}
		stmts := h.rec.Statements()
		for _, tbl := range []string{"pipelines", "dependencies", "lanes"} {
			if containsToken(stmts, tbl) {
				t.Errorf("endpoint apply touched the %s table; the endpoint lifecycle is independent of the workload graph", tbl)
			}
		}
	})

	t.Run("remove retires only the endpoint rows", func(t *testing.T) {
		h := newEndpointHarness(t)
		ce := compileTestEndpoint(t, ordersByCustomerYAML)
		if err := h.applier.Apply(ctx, []*declare.CompiledEndpoint{ce}); err != nil {
			t.Fatalf("Apply: %v", err)
		}

		if err := h.applier.Remove(ctx, "orders_by_customer"); err != nil {
			t.Fatalf("Remove: %v", err)
		}
		if _, ok := h.live.Endpoint("orders_by_customer"); ok {
			t.Error("removed endpoint is still live")
		}
		txns := h.rec.Transactions()
		if len(txns) != 2 {
			t.Fatalf("apply+remove committed %d transactions, want 2 (one each)", len(txns))
		}
		rm := txns[1]
		if len(rm) != 2 ||
			!strings.Contains(rm[0].SQL, "DELETE FROM endpoint_filters") ||
			!strings.Contains(rm[1].SQL, "DELETE FROM endpoints") {
			t.Errorf("remove transaction = %+v, want [endpoint_filters delete, endpoints delete] (children first)", rm)
		}
		for _, s := range rm {
			if strings.Contains(s.SQL, "DROP TABLE") {
				t.Errorf("remove issued DDL %q; retiring a read surface touches no data", s.SQL)
			}
		}
	})

	t.Run("removing an unknown endpoint refuses and writes nothing", func(t *testing.T) {
		h := newEndpointHarness(t)
		err := h.applier.Remove(ctx, "nope")
		if err == nil || !strings.Contains(err.Error(), "nope") {
			t.Fatalf("Remove(nope) error = %v, want a refusal naming the endpoint", err)
		}
		if stmts := h.rec.Statements(); len(stmts) != 0 {
			t.Errorf("a refused remove still wrote %d statements, want 0", len(stmts))
		}
	})

	t.Run("declare destroy leaves tables and endpoints standing", func(t *testing.T) {
		h := newEndpointHarness(t)
		ce := compileTestEndpoint(t, ordersByCustomerYAML)
		if err := h.applier.Apply(ctx, []*declare.CompiledEndpoint{ce}); err != nil {
			t.Fatalf("Apply: %v", err)
		}

		// Destroy the pipeline that authors analytics.orders, on the same
		// single-writer path.
		reg := storetest.NewRegistryFake().Register("load_orders")
		destroyer := dispatch.NewDestroyer(reg, h.d)
		before := len(h.rec.Statements())
		if err := destroyer.DestroyPipeline(ctx, "load_orders"); err != nil {
			t.Fatalf("DestroyPipeline: %v", err)
		}

		for _, s := range h.rec.Statements()[before:] {
			if strings.Contains(s.SQL, "endpoint") {
				t.Errorf("declare destroy touched an endpoint table: %q; endpoints outlive the workload graph", s.SQL)
			}
			if strings.Contains(s.SQL, "DROP TABLE") {
				t.Errorf("declare destroy dropped a table: %q; declared tables stand after destroy", s.SQL)
			}
		}
		if _, ok := h.live.Endpoint("orders_by_customer"); !ok {
			t.Error("endpoint stopped serving after declare destroy; the read surface outlives the pipeline")
		}
	})
}
