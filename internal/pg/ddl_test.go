package pg_test

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/MateusAMP2119/iris-engine-cli/internal/declare"
	"github.com/MateusAMP2119/iris-engine-cli/internal/golden"
	"github.com/MateusAMP2119/iris-engine-cli/internal/pg"
	"github.com/MateusAMP2119/iris-engine-cli/internal/pg/pgtest"
)

// ordersYAML is the specification section 5 worked example table.yaml: the four
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
// carrying NOT NULL, DEFAULT, and UNIQUE together, so the multi-modifier join
// order is pinned by a golden.
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
// KEY, NOT NULL, DEFAULT, and UNIQUE clauses in a generated CREATE TABLE. The
// rendered DDL is "applied" through the pg.DB seam (the recording fake) and
// diffed byte-for-byte against golden files, matching the section 5 worked
// example shape.
//
// spec: S05/modifier-ddl-rendering
func TestRenderCreateTable(t *testing.T) {
	ctx := context.Background()
	cases := []struct {
		name       string
		table      *declare.Table
		goldenFile string
	}{
		{"orders", parseTable(t, ordersYAML), "orders_create.sql"},
		{"widgets", parseTable(t, widgetsYAML), "widgets_create.sql"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			// Types resolve first (the apply precondition), then the CREATE is
			// issued through the data-database seam and captured for the golden.
			if err := declare.ValidateTableTypes(tc.table); err != nil {
				t.Fatalf("ValidateTableTypes: %v", err)
			}
			rec := pgtest.New()
			if err := rec.Exec(ctx, pg.RenderCreateTable(tc.table)); err != nil {
				t.Fatalf("record CREATE: %v", err)
			}
			golden.Assert(t, rec.Dump(), filepath.Join("testdata", tc.goldenFile))
		})
	}
}

// TestRenderAddColumn proves the column definition a migration file records
// renders to the section 5 additive ADD COLUMN DDL, so the recorded definition
// is a faithful, applicable form of the migration. The rendered ALTER is issued
// through the pg.DB seam and diffed against the golden.
//
// spec: S05/migration-file-format
func TestRenderAddColumn(t *testing.T) {
	ctx := context.Background()
	col := declare.MigrationColumn{Name: "status", Type: "text", Default: "'pending'"}
	rec := pgtest.New()
	if err := rec.Exec(ctx, pg.RenderAddColumn("analytics", "orders", col)); err != nil {
		t.Fatalf("record ALTER: %v", err)
	}
	golden.Assert(t, rec.Dump(), filepath.Join("testdata", "add_status.alter.sql"))
}
