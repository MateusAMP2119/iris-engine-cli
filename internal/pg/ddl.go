package pg

import (
	"fmt"
	"strings"

	"github.com/MateusAMP2119/iris-engine-cli/internal/declare"
)

// This file renders declared user tables (schemas/<schema>/<table>/table.yaml)
// into the data-database DDL of specification section 5: the CREATE TABLE a
// missing table is provisioned from, and the ALTER TABLE ADD COLUMN an additive
// migration applies. pg is the data-database DDL owner, so this rendering lives
// beside the journal DDL; the closed type mapping it consults is the declare
// leaf's (ResolveType). The output is deterministic and matches the
// section 5 worked example byte-for-byte, so a golden diff is a contract diff.
//
// Types are assumed already validated by declare.ValidateTableTypes, the apply
// precondition; an unresolved type (only reachable if validation was skipped)
// falls back to its verbatim YAML token rather than panicking.

// RenderCreateTable renders a declared table as a CREATE TABLE statement
// (specification section 5). Each column's four modifiers render as their SQL
// clauses: primary_key as PRIMARY KEY (which subsumes NOT NULL and uniqueness),
// an effective not-null as NOT NULL, a raw-SQL default as DEFAULT <expr>, and
// unique as UNIQUE. Non-primary-key modifiers render in the fixed order NOT
// NULL, DEFAULT, UNIQUE. Columns are aligned to two padded fields (name then
// type), matching the section 5 worked example.
func RenderCreateTable(t *declare.Table) string {
	types := make([]string, len(t.Columns))
	nameWidth, typeWidth := 0, 0
	for i, c := range t.Columns {
		types[i] = renderType(c.Type)
		if n := len(quoteIdent(c.Name)); n > nameWidth {
			nameWidth = n
		}
		if n := len(types[i]); n > typeWidth {
			typeWidth = n
		}
	}

	lines := make([]string, len(t.Columns))
	for i, c := range t.Columns {
		name := fmt.Sprintf("%-*s", nameWidth, quoteIdent(c.Name))
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
		quoteIdent(t.Schema), quoteIdent(t.Table), strings.Join(lines, ",\n"))
}

// RenderAddColumn renders the additive ALTER TABLE ADD COLUMN DDL for one column
// added to schema.table (specification section 5). It is the applied form of a
// migration file's recorded column definition; emitting it during sync is
// E03.7's, this is only the deterministic rendering.
func RenderAddColumn(schema, table string, col declare.MigrationColumn) string {
	def := renderType(col.Type)
	stmt := fmt.Sprintf("ALTER TABLE %s.%s ADD COLUMN %s %s",
		quoteIdent(schema), quoteIdent(table), quoteIdent(col.Name), def)
	if col.Default != "" {
		stmt += " DEFAULT " + col.Default
	}
	return stmt + ";"
}

// renderType resolves a YAML type token to its Postgres type. Types are an
// apply precondition (declare.ValidateTableTypes); an unresolved type falls back
// to its verbatim token so rendering stays total and never panics.
func renderType(yamlType string) string {
	pgt, err := declare.ResolveType(yamlType)
	if err != nil {
		return yamlType
	}
	return pgt
}

// columnModifiers renders a column's trailing DDL clauses. A primary-key column
// renders PRIMARY KEY (which implies NOT NULL and uniqueness), plus a DEFAULT
// when one is declared. Otherwise the clauses render in the fixed order NOT NULL
// (when the column is not effectively nullable), DEFAULT <expr>, UNIQUE. The
// result is empty for a plain nullable column with no default and no uniqueness.
func columnModifiers(c declare.Column) string {
	if c.PrimaryKey {
		if c.Default != "" {
			return "PRIMARY KEY DEFAULT " + c.Default
		}
		return "PRIMARY KEY"
	}
	var parts []string
	if !c.IsNullable() {
		parts = append(parts, "NOT NULL")
	}
	if c.Default != "" {
		parts = append(parts, "DEFAULT "+c.Default)
	}
	if c.Unique {
		parts = append(parts, "UNIQUE")
	}
	return strings.Join(parts, " ")
}
