package declare

import (
	"errors"
	"fmt"
	"regexp"
	"strings"
)

// This file holds the closed YAML-to-Postgres type mapping.
// The mapping is a total function on a closed set: every listed YAML
// type resolves to exactly one Postgres type, and a token outside the set (or a
// malformed parametrized form) fails apply. Rendering the resolved type into DDL
// belongs to the data-database client (internal/pg); this leaf owns only the
// mapping and its validation.

// pgTypeMap is the closed set of non-parametrized YAML-to-Postgres type mappings.
// The parametrized forms varchar(n) and numeric(p,s)
// are matched structurally; the bare numeric lives here.
var pgTypeMap = map[string]string{
	"uuid":        "uuid",
	"text":        "text",
	"int":         "integer",
	"bigint":      "bigint",
	"smallint":    "smallint",
	"numeric":     "numeric",
	"double":      "double precision",
	"bool":        "boolean",
	"timestamptz": "timestamptz",
	"timestamp":   "timestamp",
	"date":        "date",
	"time":        "time",
	"json":        "json",
	"jsonb":       "jsonb",
	"bytea":       "bytea",
}

// varcharRe matches the parametrized varchar(n) form, capturing the length. A
// bare varchar (no length) is deliberately not matched: it is outside the closed
// set and fails apply.
var varcharRe = regexp.MustCompile(`^varchar\(\s*(\d+)\s*\)$`)

// numericRe matches the parametrized numeric(p,s) form, capturing precision and
// scale. The bare numeric is handled by pgTypeMap.
var numericRe = regexp.MustCompile(`^numeric\(\s*(\d+)\s*,\s*(\d+)\s*\)$`)

// ResolveType maps one YAML type token to its Postgres type.
// A token outside the closed set -- including a bare varchar, an
// empty parameter list, or an unlisted alias -- returns an error naming the bad
// type; callers add table and column context. The returned Postgres type is
// canonical: parametrized forms render without interior whitespace.
func ResolveType(yamlType string) (string, error) {
	t := strings.TrimSpace(yamlType)
	if pgt, ok := pgTypeMap[t]; ok {
		return pgt, nil
	}
	if m := varcharRe.FindStringSubmatch(t); m != nil {
		return "varchar(" + m[1] + ")", nil
	}
	if m := numericRe.FindStringSubmatch(t); m != nil {
		return "numeric(" + m[1] + "," + m[2] + ")", nil
	}
	return "", fmt.Errorf("unknown type %q (not in the closed type set)", yamlType)
}

// ResolveColumnType maps a column's YAML type to its Postgres type. An
// out-of-set type returns an error naming the column and the bad type; the
// table context is added by ValidateTableTypes.
func ResolveColumnType(col Column) (string, error) {
	pgt, err := ResolveType(col.Type)
	if err != nil {
		return "", fmt.Errorf("column %q: %w", col.Name, err)
	}
	return pgt, nil
}

// ValidateTableTypes sweeps every column of a table and reports every column
// whose YAML type is outside the closed set, so apply refuses a table with any
// unknown type. Each reported error names the table,
// column, and bad type; multiple offending columns are joined. A table whose
// types all resolve returns nil.
func ValidateTableTypes(t *Table) error {
	if t == nil {
		return errors.New("declare: validate table types: nil table")
	}
	var errs []error
	for _, c := range t.Columns {
		if _, err := ResolveColumnType(c); err != nil {
			errs = append(errs, fmt.Errorf("declare: table %s.%s: %w", t.Schema, t.Table, err))
		}
	}
	return errors.Join(errs...)
}
