package pg_test

import (
	"context"
	"path/filepath"
	"strings"
	"testing"

	"github.com/MateusAMP2119/iris-engine-cli/internal/declare"
	"github.com/MateusAMP2119/iris-engine-cli/internal/golden"
	"github.com/MateusAMP2119/iris-engine-cli/internal/pg"
	"github.com/MateusAMP2119/iris-engine-cli/internal/pg/pgtest"
)

// ordersYAML is the worked example table.yaml: the four
// column modifiers each on their own column (primary_key, nullable: false,
// bare default, no modifiers).
const ordersYAML = `schema: analytics
table: orders
columns:
  - name: id
    type: uuid
    primary_key: true
  - name: customer_id
    type: uuid
    nullable: false
  - name: amount
    type: numeric
  - name: created_at
    type: timestamptz
    default: now()
`

// widgetsYAML is a modifiers-exercising fixture: it drives the parametrized
// types (varchar(n), numeric(p,s)), a UNIQUE column, and a single column
// carrying DEFAULT, NOT NULL, and UNIQUE together, so the multi-modifier join
// order (DEFAULT before the constraint keywords) is pinned by a golden.
const widgetsYAML = `schema: shop
table: widgets
columns:
  - name: id
    type: uuid
    primary_key: true
  - name: sku
    type: varchar(32)
    nullable: false
    unique: true
  - name: price
    type: numeric(10,2)
    default: "0"
  - name: label
    type: text
    nullable: false
    default: "'unnamed'"
    unique: true
  - name: notes
    type: text
`

// itemsYAML exercises a primary-key column that also declares a default, pinning
// the DEFAULT-before-PRIMARY-KEY clause order in a golden.
const itemsYAML = `schema: catalog
table: items
columns:
  - name: id
    type: uuid
    primary_key: true
    default: gen_random_uuid()
  - name: name
    type: text
    nullable: false
`

// reservedYAML names its schema, table, and columns after SQL reserved words, so
// the golden pins that every identifier is double-quoted (the correctness
// superset of the worked-example shape).
const reservedYAML = `schema: order
table: select
columns:
  - name: user
    type: uuid
    primary_key: true
  - name: group
    type: text
    nullable: false
  - name: default
    type: text
    default: "'x'"
    unique: true
`

// parseTable parses a table.yaml document, failing the test on any error.
func parseTable(t *testing.T, doc string) *declare.Table {
	t.Helper()
	tbl, err := declare.ParseTable([]byte(doc))
	if err != nil {
		t.Fatalf("ParseTable: %v", err)
	}
	return tbl
}

// TestRenderCreateTable proves the four column modifiers (primary_key, nullable,
// default as a raw SQL expression, unique) render into the corresponding PRIMARY
// KEY, NOT NULL, DEFAULT, and UNIQUE clauses in a generated CREATE TABLE, with a
// DEFAULT clause always ahead of the constraint keywords and every identifier
// double-quoted. The rendered DDL is "applied" through the pg.DB seam (the
// recording fake) and diffed byte-for-byte against golden files, matching the
// worked example shape.
func TestRenderCreateTable(t *testing.T) {
	ctx := context.Background()
	cases := []struct {
		name       string
		table      *declare.Table
		goldenFile string
	}{
		{"orders", parseTable(t, ordersYAML), "orders_create.sql"},
		{"widgets", parseTable(t, widgetsYAML), "widgets_create.sql"},
		{"items", parseTable(t, itemsYAML), "items_create.sql"},
		{"reserved", parseTable(t, reservedYAML), "reserved_create.sql"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			// Types resolve first (the apply precondition), then the CREATE is
			// issued through the data-database seam and captured for the golden.
			if err := declare.ValidateTableTypes(tc.table); err != nil {
				t.Fatalf("ValidateTableTypes: %v", err)
			}
			ddl, err := pg.RenderCreateTable(tc.table)
			if err != nil {
				t.Fatalf("RenderCreateTable: %v", err)
			}
			rec := pgtest.New()
			if err := rec.Exec(ctx, ddl); err != nil {
				t.Fatalf("record CREATE: %v", err)
			}
			golden.Assert(t, rec.Dump(), filepath.Join("testdata", tc.goldenFile))
		})
	}
}

// TestRenderCreateTableQuotesReservedIdentifiers proves that a schema, table, or
// column named after a SQL reserved word renders double-quoted, so the generated
// DDL is a syntactically valid identifier rather than a keyword collision that
// would fail only at the database.
func TestRenderCreateTableQuotesReservedIdentifiers(t *testing.T) {
	ddl, err := pg.RenderCreateTable(parseTable(t, reservedYAML))
	if err != nil {
		t.Fatalf("RenderCreateTable: %v", err)
	}
	// Every reserved identifier renders quoted, schema and table included.
	for _, want := range []string{`"order"."select"`, `"user"`, `"group"`, `"default"`} {
		if !strings.Contains(ddl, want) {
			t.Errorf("rendered DDL missing quoted identifier %s:\n%s", want, ddl)
		}
	}
	// The bare, unquoted keyword collision must never appear.
	if strings.Contains(ddl, "order.select") {
		t.Errorf("rendered DDL contains the unquoted keyword collision order.select:\n%s", ddl)
	}
}

// TestRenderCreateTableUnknownType proves RenderCreateTable refuses a column
// whose YAML type is outside the closed set, returning the resolve error rather
// than emitting invalid SQL that surfaces only at the database.
func TestRenderCreateTableUnknownType(t *testing.T) {
	bad := &declare.Table{
		Schema: "analytics",
		Table:  "orders",
		Columns: []declare.Column{
			{Name: "id", Type: "uuid", PrimaryKey: true},
			{Name: "blobby", Type: "blob"},
		},
	}
	ddl, err := pg.RenderCreateTable(bad)
	if err == nil {
		t.Fatalf("RenderCreateTable accepted an out-of-set type; want an error, got:\n%s", ddl)
	}
	if ddl != "" {
		t.Errorf("RenderCreateTable returned DDL alongside an error: %q", ddl)
	}
	for _, want := range []string{"analytics", "orders", "blobby", "blob"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q does not name %q", err, want)
		}
	}
}

// TestRenderAddColumn proves the column definition a migration file records
// renders to the additive ADD COLUMN DDL, so the recorded definition
// is a faithful, applicable form of the migration. The rendered ALTER is issued
// through the pg.DB seam and diffed against the golden; identifiers are quoted
// and an out-of-set type is refused.
func TestRenderAddColumn(t *testing.T) {
	ctx := context.Background()
	col := declare.MigrationColumn{Name: "status", Type: "text", Default: "'pending'"}
	alter, err := pg.RenderAddColumn("analytics", "orders", col)
	if err != nil {
		t.Fatalf("RenderAddColumn: %v", err)
	}
	rec := pgtest.New()
	if err := rec.Exec(ctx, alter); err != nil {
		t.Fatalf("record ALTER: %v", err)
	}
	golden.Assert(t, rec.Dump(), filepath.Join("testdata", "add_status.alter.sql"))

	// A reserved-word identifier renders quoted.
	reserved, err := pg.RenderAddColumn("order", "select", declare.MigrationColumn{Name: "group", Type: "text"})
	if err != nil {
		t.Fatalf("RenderAddColumn(reserved): %v", err)
	}
	for _, want := range []string{`"order"."select"`, `"group"`} {
		if !strings.Contains(reserved, want) {
			t.Errorf("rendered ALTER missing quoted identifier %s:\n%s", want, reserved)
		}
	}

	// An out-of-set type is refused rather than emitted.
	if alter, err := pg.RenderAddColumn("analytics", "orders", declare.MigrationColumn{Name: "x", Type: "blob"}); err == nil {
		t.Errorf("RenderAddColumn accepted an out-of-set type; want an error, got:\n%s", alter)
	}
}
