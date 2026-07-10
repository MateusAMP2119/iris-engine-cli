package daemon

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/MateusAMP2119/iris-engine-cli/internal/declare"
	"github.com/MateusAMP2119/iris-engine-cli/internal/dispatch"
	"github.com/MateusAMP2119/iris-engine-cli/internal/store"
)

// fakeEndpointReader stands in for the meta EndpointRowReader: it returns fixed
// persisted rows (or an error), so the reload's recompile-and-publish is provable
// with no live Postgres.
type fakeEndpointReader struct {
	rows []store.EndpointRow
	err  error
}

func (f fakeEndpointReader) ReadEndpoints(context.Context) ([]store.EndpointRow, error) {
	return f.rows, f.err
}

// writeOrdersSchema lays down a minimal schemas/analytics/orders/table.yaml so the
// reload can recompile an endpoint against a real source shape.
func writeOrdersSchema(t *testing.T, ws string) {
	t.Helper()
	path := filepath.Join(ws, "schemas", "analytics", "orders", "table.yaml")
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(path, []byte(`schema: analytics
table: orders
columns:
  - name: id
    type: bigint
    primary_key: true
  - name: customer_id
    type: uuid
  - name: amount
    type: numeric
`), 0o644); err != nil {
		t.Fatalf("write table.yaml: %v", err)
	}
}

// TestReloadEndpointsRepublishesFromMeta proves the daemon recompiles persisted
// endpoints from meta and publishes them into the live serving registry at startup:
// after a reload, the registry serves the endpoint's compiled shape (its derived SQL
// recompiled from the persisted rows, never a stored text) with no re-apply, so a
// restart or failover keeps serving /q.
//
// spec: S07/endpoints-reload-from-meta
func TestReloadEndpointsRepublishesFromMeta(t *testing.T) {
	t.Run("S07/endpoints-reload-from-meta", func(t *testing.T) {
		ws := t.TempDir()
		writeOrdersSchema(t, ws)

		reader := fakeEndpointReader{rows: []store.EndpointRow{{
			Name:    "orders_by_customer",
			Source:  "analytics.orders",
			Fields:  []string{"id", "customer_id", "amount"},
			Sort:    "id",
			Filters: []store.EndpointFilterRow{{Param: "customer_id", Op: "eq"}},
		}}}
		reg := dispatch.NewEndpointRegistry()

		// A fresh registry serves nothing (the restart state the reload must repair).
		if _, ok := reg.Endpoint("orders_by_customer"); ok {
			t.Fatal("registry served the endpoint before any reload")
		}

		if err := reloadEndpoints(context.Background(), reader, reg, ws, nil); err != nil {
			t.Fatalf("reloadEndpoints: %v", err)
		}

		got, ok := reg.Endpoint("orders_by_customer")
		if !ok {
			t.Fatal("registry does not serve the persisted endpoint after reload")
		}
		if got.SQL == "" {
			t.Error("reloaded endpoint has no derived SQL; the shape was not recompiled")
		}

		// The reloaded shape is byte-identical to a fresh compile of the same declared
		// endpoint: the reload recompiles deterministically from the persisted rows.
		tables, err := declare.ValidateSchemaTree(filepath.Join(ws, "schemas"))
		if err != nil {
			t.Fatalf("read schemas: %v", err)
		}
		want, err := declare.CompileEndpoint(endpointFromRow(reader.rows[0]), declare.TableIndex(tables))
		if err != nil {
			t.Fatalf("compile expected endpoint: %v", err)
		}
		if got.SQL != want.SQL {
			t.Errorf("reloaded SQL = %q, want %q", got.SQL, want.SQL)
		}
	})
}

// TestReloadEndpointsEmptyIsNoOp proves an engine with nothing applied reloads to an
// empty registry and no error (the common first-boot case).
//
// spec: S07/endpoints-reload-from-meta
func TestReloadEndpointsEmptyIsNoOp(t *testing.T) {
	ws := t.TempDir()
	reg := dispatch.NewEndpointRegistry()
	if err := reloadEndpoints(context.Background(), fakeEndpointReader{}, reg, ws, nil); err != nil {
		t.Fatalf("reloadEndpoints on an empty meta: %v", err)
	}
	if _, ok := reg.Endpoint("anything"); ok {
		t.Error("empty reload published a phantom endpoint")
	}
}

// TestReloadEndpointsSkipsDriftedSource proves a persisted endpoint whose source no
// longer compiles is skipped (logged, not fatal) while every other endpoint still
// reloads, so one drifted contract never blocks the whole read surface -- and the
// reload itself never fails the daemon.
//
// spec: S07/endpoints-reload-from-meta
func TestReloadEndpointsSkipsDriftedSource(t *testing.T) {
	ws := t.TempDir()
	writeOrdersSchema(t, ws)

	reader := fakeEndpointReader{rows: []store.EndpointRow{
		{Name: "good", Source: "analytics.orders", Fields: []string{"id", "amount"}, Sort: "id"},
		{Name: "drifted", Source: "analytics.gone", Fields: []string{"id"}, Sort: "id"},
	}}
	reg := dispatch.NewEndpointRegistry()

	if err := reloadEndpoints(context.Background(), reader, reg, ws, nil); err != nil {
		t.Fatalf("reloadEndpoints must not fail on a drifted source: %v", err)
	}
	if _, ok := reg.Endpoint("good"); !ok {
		t.Error("the compilable endpoint was not served after reload")
	}
	if _, ok := reg.Endpoint("drifted"); ok {
		t.Error("the drifted endpoint was served despite failing to compile")
	}
}

// TestReloadEndpointsReadError proves a meta read failure is surfaced (the caller
// logs it and keeps serving), never swallowed.
//
// spec: S07/endpoints-reload-from-meta
func TestReloadEndpointsReadError(t *testing.T) {
	ws := t.TempDir()
	reg := dispatch.NewEndpointRegistry()
	boom := errors.New("meta unreachable")
	if err := reloadEndpoints(context.Background(), fakeEndpointReader{err: boom}, reg, ws, nil); !errors.Is(err, boom) {
		t.Errorf("reload read error not surfaced: %v", err)
	}
}
