package declare_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/MateusAMP2119/iris-lakehouse/internal/declare"
	"github.com/MateusAMP2119/iris-lakehouse/internal/fixtures"
	"github.com/MateusAMP2119/iris-lakehouse/internal/golden"
)

// ordersSource returns the parsed analytics.orders table the golden endpoint's
// source resolves against: id is the unique primary key (a valid sort), the rest
// are ordinary columns. It mirrors the golden workspace's
// schemas/analytics/orders/table.yaml so the compiler's source-column checks run
// against a real, declared table without touching disk.
func ordersSource() *declare.Table {
	return &declare.Table{
		Schema: "analytics",
		Table:  "orders",
		Columns: []declare.Column{
			{Name: "id", Type: "uuid", PrimaryKey: true},
			{Name: "customer_id", Type: "uuid"},
			{Name: "amount", Type: "numeric"},
			{Name: "created_at", Type: "timestamptz", Default: "now()"},
		},
	}
}

// ordersIndex is the single-table schema set the golden endpoint compiles against.
func ordersIndex() map[string]*declare.Table {
	return map[string]*declare.Table{"analytics.orders": ordersSource()}
}

// goldenEndpoint parses the checked-in golden endpoint file, the canonical
// orders_by_customer read surface.
func goldenEndpoint(t *testing.T) *declare.Endpoint {
	t.Helper()
	path := filepath.Join(fixtures.WorkspaceGolden(), "endpoints", "orders_by_customer.yaml")
	data, err := os.ReadFile(path) //nolint:gosec // G304: a checked-in fixture path.
	if err != nil {
		t.Fatalf("read golden endpoint: %v", err)
	}
	ep, err := declare.ParseEndpoint(data)
	if err != nil {
		t.Fatalf("parse golden endpoint: %v", err)
	}
	return ep
}

// TestEndpointYAMLFileShape proves an endpoint is one flat file at
// endpoints/<name>.yaml whose filename equals its endpoint: field, and that a
// file whose basename disagrees with the field is rejected naming both.
func TestEndpointYAMLFileShape(t *testing.T) {
	t.Run("filename-matches-field", func(t *testing.T) {
		dir := t.TempDir()
		path := filepath.Join(dir, "orders_by_customer.yaml")
		writeFile(t, path, "endpoint: orders_by_customer\nsource: analytics.orders\nfields: [id]\nsort: id\n")

		de, err := declare.LoadEndpointFile(path)
		if err != nil {
			t.Fatalf("LoadEndpointFile(matching) = %v, want nil", err)
		}
		if de.Name != "orders_by_customer" {
			t.Errorf("discovered name = %q, want orders_by_customer", de.Name)
		}
	})

	t.Run("filename-mismatch-rejected", func(t *testing.T) {
		dir := t.TempDir()
		path := filepath.Join(dir, "wrong_name.yaml")
		writeFile(t, path, "endpoint: orders_by_customer\nsource: analytics.orders\nfields: [id]\nsort: id\n")

		_, err := declare.LoadEndpointFile(path)
		if err == nil {
			t.Fatal("LoadEndpointFile(mismatch) = nil, want an error naming filename and field")
		}
		for _, want := range []string{"wrong_name", "orders_by_customer"} {
			if !strings.Contains(err.Error(), want) {
				t.Errorf("mismatch error %q does not name %q", err, want)
			}
		}
	})

	t.Run("non-yaml-extension-rejected", func(t *testing.T) {
		dir := t.TempDir()
		path := filepath.Join(dir, "orders_by_customer.txt")
		writeFile(t, path, "endpoint: orders_by_customer\nsource: analytics.orders\nfields: [id]\nsort: id\n")

		if _, err := declare.LoadEndpointFile(path); err == nil {
			t.Fatal("LoadEndpointFile(.txt) = nil, want an error: an endpoint is a .yaml file")
		}
	})
}

// TestEndpointFilesCanonicalLocation proves declared read endpoints are
// discovered as flat, shape-only YAML files at endpoints/<name>.yaml in the
// workspace: the golden workspace yields exactly the orders_by_customer endpoint,
// and a nested subfolder under endpoints/ is rejected (the flat-file exception).
func TestEndpointFilesCanonicalLocation(t *testing.T) {
	t.Run("golden-workspace-flat-file", func(t *testing.T) {
		eps, err := declare.DiscoverEndpoints(fixtures.WorkspaceGolden())
		if err != nil {
			t.Fatalf("DiscoverEndpoints(golden) = %v, want nil", err)
		}
		if len(eps) != 1 {
			t.Fatalf("discovered %d endpoints, want 1", len(eps))
		}
		ep := eps[0]
		if ep.Name != "orders_by_customer" {
			t.Errorf("endpoint name = %q, want orders_by_customer", ep.Name)
		}
		if ep.Spec == nil || ep.Spec.Source != "analytics.orders" {
			t.Errorf("endpoint source = %+v, want analytics.orders", ep.Spec)
		}
		if !strings.HasSuffix(ep.Path, filepath.Join("endpoints", "orders_by_customer.yaml")) {
			t.Errorf("endpoint path = %q, want it under endpoints/orders_by_customer.yaml", ep.Path)
		}
	})

	t.Run("absent-endpoints-dir-empty", func(t *testing.T) {
		eps, err := declare.DiscoverEndpoints(t.TempDir())
		if err != nil {
			t.Fatalf("DiscoverEndpoints(no endpoints/) = %v, want nil", err)
		}
		if len(eps) != 0 {
			t.Errorf("discovered %d endpoints, want 0 for an absent endpoints/ tree", len(eps))
		}
	})

	t.Run("nested-subfolder-rejected", func(t *testing.T) {
		root := t.TempDir()
		nested := filepath.Join(root, "endpoints", "orders_by_customer")
		if err := os.MkdirAll(nested, 0o755); err != nil {
			t.Fatal(err)
		}
		writeFile(t, filepath.Join(nested, "iris-declare.yaml"), "endpoint: orders_by_customer\n")

		if _, err := declare.DiscoverEndpoints(root); err == nil {
			t.Fatal("DiscoverEndpoints(nested) = nil, want rejection: endpoints/ holds flat files only")
		}
	})
}

// TestEndpointSingleTableProjection proves an endpoint declares an explicit field
// projection over one table only: a valid flat projection compiles, while a join,
// an aggregation, and a computed (non-identifier) field are each rejected.
func TestEndpointSingleTableProjection(t *testing.T) {
	t.Run("flat-projection-accepted", func(t *testing.T) {
		ep, err := declare.ParseEndpoint([]byte("endpoint: e\nsource: analytics.orders\nfields: [id, amount]\nsort: id\n"))
		if err != nil {
			t.Fatalf("ParseEndpoint(flat) = %v, want nil", err)
		}
		if _, err := declare.CompileEndpoint(ep, ordersIndex()); err != nil {
			t.Fatalf("CompileEndpoint(flat) = %v, want nil", err)
		}
	})

	t.Run("join-key-rejected", func(t *testing.T) {
		_, err := declare.ParseEndpoint([]byte("endpoint: e\nsource: analytics.orders\nfields: [id]\njoin: raw.orders_staging\nsort: id\n"))
		if err == nil {
			t.Fatal("ParseEndpoint(join) = nil, want rejection: an endpoint is single-table, no joins")
		}
		if !strings.Contains(err.Error(), "join") {
			t.Errorf("join error %q does not name the offending key", err)
		}
	})

	t.Run("aggregation-key-rejected", func(t *testing.T) {
		_, err := declare.ParseEndpoint([]byte("endpoint: e\nsource: analytics.orders\nfields: [customer_id]\ngroup_by: [customer_id]\nsort: customer_id\n"))
		if err == nil {
			t.Fatal("ParseEndpoint(group_by) = nil, want rejection: no aggregations")
		}
	})

	t.Run("computed-field-rejected", func(t *testing.T) {
		_, err := declare.ParseEndpoint([]byte("endpoint: e\nsource: analytics.orders\nfields: [id, \"sum(amount)\"]\nsort: id\n"))
		if err == nil {
			t.Fatal("ParseEndpoint(computed field) = nil, want rejection: a projection is bare columns, no computed fields")
		}
	})
}

// TestEndpointSourceValidation proves endpoint compile resolves the single
// declared source table against the schemas/ set and refuses the journal (and the
// reserved public schema) as a source, while an undeclared source is rejected.
func TestEndpointSourceValidation(t *testing.T) {
	t.Run("declared-source-resolves", func(t *testing.T) {
		if _, err := declare.CompileEndpoint(goldenEndpoint(t), ordersIndex()); err != nil {
			t.Fatalf("CompileEndpoint(declared source) = %v, want nil", err)
		}
	})

	t.Run("undeclared-source-rejected", func(t *testing.T) {
		ep, err := declare.ParseEndpoint([]byte("endpoint: e\nsource: analytics.missing\nfields: [id]\nsort: id\n"))
		if err != nil {
			t.Fatal(err)
		}
		if _, err := declare.CompileEndpoint(ep, ordersIndex()); err == nil {
			t.Fatal("CompileEndpoint(undeclared source) = nil, want rejection naming schemas/")
		}
	})

	t.Run("journal-refused", func(t *testing.T) {
		ep, err := declare.ParseEndpoint([]byte("endpoint: e\nsource: public.data_journal\nfields: [id]\nsort: id\n"))
		if err != nil {
			t.Fatal(err)
		}
		_, err = declare.CompileEndpoint(ep, ordersIndex())
		if err == nil {
			t.Fatal("CompileEndpoint(journal source) = nil, want refusal: the journal is never a source")
		}
		if !strings.Contains(err.Error(), "data_journal") {
			t.Errorf("journal refusal %q does not name the journal", err)
		}
	})

	t.Run("public-schema-refused", func(t *testing.T) {
		ep, err := declare.ParseEndpoint([]byte("endpoint: e\nsource: public.orders\nfields: [id]\nsort: id\n"))
		if err != nil {
			t.Fatal(err)
		}
		if _, err := declare.CompileEndpoint(ep, ordersIndex()); err == nil {
			t.Fatal("CompileEndpoint(public source) = nil, want refusal: public is engine-reserved")
		}
	})
}

// TestEndpointFilterSortValidation proves endpoint compile accepts only eq or
// range as a filter kind and requires sort to be a unique source column: a bad
// filter op is rejected at parse, and a non-unique or unknown sort column is
// rejected at compile.
func TestEndpointFilterSortValidation(t *testing.T) {
	t.Run("eq-and-range-accepted", func(t *testing.T) {
		ep := goldenEndpoint(t) // customer_id: eq, created_at: range
		if _, err := declare.CompileEndpoint(ep, ordersIndex()); err != nil {
			t.Fatalf("CompileEndpoint(eq+range) = %v, want nil", err)
		}
	})

	t.Run("bad-filter-op-rejected", func(t *testing.T) {
		_, err := declare.ParseEndpoint([]byte("endpoint: e\nsource: analytics.orders\nfields: [id]\nfilters:\n  amount: like\nsort: id\n"))
		if err == nil {
			t.Fatal("ParseEndpoint(op=like) = nil, want rejection: only eq or range")
		}
		if !strings.Contains(err.Error(), "like") {
			t.Errorf("bad-op error %q does not name the offending op", err)
		}
	})

	t.Run("non-unique-sort-rejected", func(t *testing.T) {
		ep, err := declare.ParseEndpoint([]byte("endpoint: e\nsource: analytics.orders\nfields: [id, amount]\nsort: amount\n"))
		if err != nil {
			t.Fatal(err)
		}
		_, err = declare.CompileEndpoint(ep, ordersIndex())
		if err == nil {
			t.Fatal("CompileEndpoint(sort=amount) = nil, want rejection: sort must be a unique column")
		}
		if !strings.Contains(err.Error(), "amount") || !strings.Contains(err.Error(), "unique") {
			t.Errorf("non-unique-sort error %q must name the column and the uniqueness rule", err)
		}
	})

	t.Run("unknown-sort-column-rejected", func(t *testing.T) {
		ep, err := declare.ParseEndpoint([]byte("endpoint: e\nsource: analytics.orders\nfields: [id]\nsort: nope\n"))
		if err != nil {
			t.Fatal(err)
		}
		if _, err := declare.CompileEndpoint(ep, ordersIndex()); err == nil {
			t.Fatal("CompileEndpoint(sort=nope) = nil, want rejection: sort is not a source column")
		}
	})

	t.Run("filter-on-non-source-column-rejected", func(t *testing.T) {
		ep, err := declare.ParseEndpoint([]byte("endpoint: e\nsource: analytics.orders\nfields: [id]\nfilters:\n  nope: eq\nsort: id\n"))
		if err != nil {
			t.Fatal(err)
		}
		if _, err := declare.CompileEndpoint(ep, ordersIndex()); err == nil {
			t.Fatal("CompileEndpoint(filter on unknown column) = nil, want rejection")
		}
	})
}

// TestEndpointSQLDeterministic proves compile derives exactly one parameterized
// SQL text deterministically from an endpoint: the golden orders_by_customer
// endpoint yields the checked-in golden SQL byte-for-byte, and two independent
// compiles of the same input yield byte-identical SQL.
func TestEndpointSQLDeterministic(t *testing.T) {
	ep := goldenEndpoint(t)

	c1, err := declare.CompileEndpoint(ep, ordersIndex())
	if err != nil {
		t.Fatalf("CompileEndpoint(golden) = %v, want nil", err)
	}
	c2, err := declare.CompileEndpoint(goldenEndpoint(t), ordersIndex())
	if err != nil {
		t.Fatalf("CompileEndpoint(golden, second) = %v, want nil", err)
	}

	// Determinism: identical input yields byte-identical SQL.
	if c1.SQL != c2.SQL {
		t.Errorf("compile is non-deterministic:\n first: %q\nsecond: %q", c1.SQL, c2.SQL)
	}

	// The exact parameterized text is the contract, pinned by a golden.
	golden.Assert(t, []byte(c1.SQL+"\n"), filepath.Join("testdata", "endpoint_orders_by_customer.sql"))

	// The parameterized statement binds exactly its filter, keyset and limit
	// params, never assembling caller SQL.
	if !strings.Contains(c1.SQL, "$1") || !strings.Contains(c1.SQL, "$5") {
		t.Errorf("compiled SQL does not carry the expected bound params:\n%s", c1.SQL)
	}
}

// writeFile writes content to path, creating parent directories, failing the test
// on error.
func writeFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}
