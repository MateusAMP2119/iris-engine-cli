package pg

import (
	"fmt"
	"strings"

	"github.com/MateusAMP2119/iris-engine-cli/internal/declare"
)

// This file renders declared user tables (schemas/<schema>/<table>/table.yaml)
// into data-database DDL: the CREATE TABLE a missing table is provisioned from,
// and the ALTER TABLE ADD COLUMN an additive migration applies. pg is the
// data-database DDL owner, so this rendering lives beside the journal DDL; the
// closed type mapping it consults is the declare leaf's (ResolveType). The
// output is deterministic, so a golden diff is a contract diff.
//
// Two deliberate deviations from the worked example the rendering is modeled on,
// both correctness-preserving supersets of its shape:
//   - Every identifier (schema, table, column) is double-quoted unconditionally,
//     not just the reserved words the worked example happens to avoid, so any
//     user name -- order, user, default, a name containing a quote -- renders as
//     a valid identifier rather than a keyword collision that fails at the
//     database. The example's alignment and clause set are preserved.
//   - A column's DEFAULT clause renders ahead of its constraint keywords
//     (DEFAULT <expr> before PRIMARY KEY / NOT NULL / UNIQUE), the pg_dump
//     convention; the worked example never combines the two on one column, so
//     this only fixes an order it leaves unspecified.
//
// A type outside the closed set is refused: rendering returns the resolve error
// rather than emitting invalid SQL that would surface only at the database.

// RenderCreateTable renders a declared table as a CREATE TABLE statement. Each
// column's four modifiers render as their SQL clauses: a raw-SQL default as
// DEFAULT <expr> (rendered first), then primary_key as PRIMARY KEY (which
// subsumes NOT NULL and uniqueness), or -- for a non-primary-key column -- an
// effective not-null as NOT NULL and unique as UNIQUE. Columns are aligned to
// two padded fields (quoted name, then type). A column whose YAML type is
// outside the closed set returns an error naming the table, column, and bad
// type.
func RenderCreateTable(t *declare.Table) (string, error) {
	types := make([]string, len(t.Columns))
	nameWidth, typeWidth := 0, 0
	for i, c := range t.Columns {
		pgt, err := declare.ResolveColumnType(c)
		if err != nil {
			return "", fmt.Errorf("pg: render CREATE TABLE %s.%s: %w", t.Schema, t.Table, err)
		}
		types[i] = pgt
		if n := len(quoteIdentifier(c.Name)); n > nameWidth {
			nameWidth = n
		}
		if n := len(pgt); n > typeWidth {
			typeWidth = n
		}
	}

	lines := make([]string, len(t.Columns))
	for i, c := range t.Columns {
		name := fmt.Sprintf("%-*s", nameWidth, quoteIdentifier(c.Name))
		suffix := columnModifiers(c)
		if suffix == "" {
			// No trailing clause: the type ends the line, unpadded.
			lines[i] = "    " + name + " " + types[i]
			continue
		}
		typ := fmt.Sprintf("%-*s", typeWidth, types[i])
		lines[i] = "    " + name + " " + typ + " " + suffix
	}

	return fmt.Sprintf("CREATE TABLE %s.%s (\n%s\n);",
		quoteIdentifier(t.Schema), quoteIdentifier(t.Table), strings.Join(lines, ",\n")), nil
}

// RenderAddColumn renders the additive ALTER TABLE ADD COLUMN DDL for one column
// added to schema.table. It is the applied form of a migration file's recorded
// column definition; emitting it during sync belongs to the sync engine, while
// this is only the deterministic rendering. A column whose YAML type is outside
// the closed set returns an error.
//
// The ADD COLUMN carries IF NOT EXISTS (Postgres 9.6+) so it is idempotent: the
// data ALTER runs before the meta migration head is recorded, so a head-record
// failure leaves the column present with the head unrecorded, and the next apply
// replays the same migration. Without IF NOT EXISTS that replay aborts "column
// already exists" and the state is unrecoverable; with it the replay is a no-op.
func RenderAddColumn(schema, table string, col declare.MigrationColumn) (string, error) {
	def, err := declare.ResolveType(col.Type)
	if err != nil {
		return "", fmt.Errorf("pg: render ADD COLUMN %s.%s.%s: %w", schema, table, col.Name, err)
	}
	stmt := fmt.Sprintf("ALTER TABLE %s.%s ADD COLUMN IF NOT EXISTS %s %s",
		quoteIdentifier(schema), quoteIdentifier(table), quoteIdentifier(col.Name), def)
	if col.Default != "" {
		stmt += " DEFAULT " + col.Default
	}
	return stmt + ";", nil
}

// quoteIdentifier double-quotes a SQL identifier unconditionally and escapes any
// embedded double quote by doubling it (the SQL standard escape). Every schema,
// table, and column name rendered into user-table DDL passes through here, so a
// reserved word or a name containing a quote is always a safe, unambiguous
// identifier.
func quoteIdentifier(name string) string {
	return `"` + strings.ReplaceAll(name, `"`, `""`) + `"`
}

// columnModifiers renders a column's trailing DDL clauses in the fixed order
// DEFAULT <expr>, then the constraint keywords. A primary-key column renders
// PRIMARY KEY (which implies NOT NULL and uniqueness); otherwise a not-null
// column renders NOT NULL and a unique column renders UNIQUE. The result is
// empty for a plain nullable column with no default and no uniqueness.
func columnModifiers(c declare.Column) string {
	var parts []string
	if c.Default != "" {
		parts = append(parts, "DEFAULT "+c.Default)
	}
	if c.PrimaryKey {
		parts = append(parts, "PRIMARY KEY")
	} else {
		if !c.IsNullable() {
			parts = append(parts, "NOT NULL")
		}
		if c.Unique {
			parts = append(parts, "UNIQUE")
		}
	}
	return strings.Join(parts, " ")
}
