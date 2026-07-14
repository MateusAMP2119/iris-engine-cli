package declare

import (
	"errors"
	"fmt"
	"os"
	"strings"
)

// publicSchema is the engine-reserved schema name: apply rejects a public schema
// folder under schemas/ and any reads/writes entry targeting public.*.
const publicSchema = "public"

// ValidateAccess checks the shape and reservation rules of one pipeline
// declaration's reads/writes entries. Reads/writes are access-only: they grant
// schema+table+field access on the pipeline's
// Postgres role and are recorded in meta, but they are never exclusive and
// never create a dependency edge or run order of their own -- ordering derives
// only from lanes and depends_on (see ValidateDependencies, whose acyclicity
// and upstream-first checks read only a declaration's name and depends_on).
// Two pipelines may declare overlapping or identical reads/writes with no
// rejection; concurrent-writer safety is the engine's problem, not a
// declaration constraint.
//
// Every entry must name a dotted schema.table (exactly one dot, both sides
// non-empty) and carry a non-empty fields list; an entry violating either rule
// is rejected, and there is no implicit all-columns fallback for an omitted
// fields list. An entry whose schema is exactly "public" is rejected: public is
// engine-reserved.
//
// All violations across both lists are reported together (errors.Join), in a
// deterministic order: reads before writes, entry order preserved within each
// list. A pipeline with no violations returns nil.
func ValidateAccess(p *Pipeline) error {
	var errs []error
	for i, a := range p.Reads {
		if err := validateAccessEntry("reads", i, a); err != nil {
			errs = append(errs, err)
		}
	}
	for i, a := range p.Writes {
		if err := validateAccessEntry("writes", i, a); err != nil {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}

// validateAccessEntry checks one reads/writes entry (list names "reads" or
// "writes", idx is its position) against the table-shape, fields-required, and
// public-reservation rules, joining every violation the single entry has.
func validateAccessEntry(list string, idx int, a Access) error {
	var errs []error

	schema, _, ok := splitDottedTable(a.Table)
	switch {
	case !ok:
		errs = append(errs, fmt.Errorf("declare: %s[%d]: table %q is not a dotted schema.table name (two non-empty parts, exactly one dot)", list, idx, a.Table))
	case schema == publicSchema:
		errs = append(errs, fmt.Errorf("declare: %s[%d]: table %q targets the public schema, which is engine-reserved; reads/writes on public.* are rejected", list, idx, a.Table))
	}

	if len(a.Fields) == 0 {
		errs = append(errs, fmt.Errorf("declare: %s[%d] (table %q): fields is required and must be non-empty; there is no implicit all-columns fallback", list, idx, a.Table))
	}

	return errors.Join(errs...)
}

// splitDottedTable splits a reads/writes table name into its schema and table
// parts. ok is false unless the name is exactly two non-empty parts joined by
// one dot -- multiple dots, a leading/trailing dot, or no dot at all all fail.
func splitDottedTable(table string) (schema, name string, ok bool) {
	parts := strings.Split(table, ".")
	if len(parts) != 2 || parts[0] == "" || parts[1] == "" {
		return "", "", false
	}
	return parts[0], parts[1], true
}

// ValidateSchemaTreeReserved reads the top level of a schemas/ directory and
// rejects a folder named "public" directly under it: public is engine-reserved,
// so it may never be declared as a schema folder, independent of
// ValidateSchemaTree's per-table folder-agreement checks.
func ValidateSchemaTreeReserved(schemasDir string) error {
	entries, err := os.ReadDir(schemasDir)
	if err != nil {
		return fmt.Errorf("declare: read schemas directory %s: %w", schemasDir, err)
	}
	for _, e := range entries {
		name := e.Name()
		if isHidden(name) {
			continue
		}
		if e.IsDir() && name == publicSchema {
			return fmt.Errorf("declare: schemas/%s is reserved; public is engine-reserved and apply rejects a public schema folder", publicSchema)
		}
	}
	return nil
}
