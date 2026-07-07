package declare

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"

	"github.com/goccy/go-yaml"
)

// tableFile is the canonical filename of a table's desired-state declaration.
const tableFile = "table.yaml"

// migrationsDirName is the canonical name of a table folder's engine-written
// migration ledger directory.
const migrationsDirName = "migrations"

// Column is one parsed table.yaml column: a name and YAML type plus the four
// modifiers of specification section 5. The YAML-to-Postgres type mapping and
// DDL rendering belong to a later task; this parses the shape only.
type Column struct {
	// Name is the column name.
	Name string `yaml:"name"`
	// Type is the YAML type token (mapped to Postgres by a later task).
	Type string `yaml:"type"`
	// PrimaryKey marks the column as a primary key (which implies not-null).
	PrimaryKey bool `yaml:"primary_key"`
	// Nullable is the explicit nullability; nil means unset (defaults to true,
	// but false under a primary key).
	Nullable *bool `yaml:"nullable"`
	// Default is a raw SQL default expression, verbatim.
	Default string `yaml:"default"`
	// Unique marks the column unique.
	Unique bool `yaml:"unique"`
}

// IsNullable resolves the effective nullability: false under a primary key,
// otherwise the explicit value, otherwise the default of true.
func (c Column) IsNullable() bool {
	if c.PrimaryKey {
		return false
	}
	if c.Nullable == nil {
		return true
	}
	return *c.Nullable
}

// Table is a parsed table.yaml: the desired head of one declared table
// (specification section 5).
type Table struct {
	// Schema is the schema name; validated against its folder.
	Schema string `yaml:"schema"`
	// Table is the table name; validated against its folder.
	Table string `yaml:"table"`
	// Columns are the table's declared columns, in order.
	Columns []Column `yaml:"columns"`
}

// ParseTable parses a table.yaml document into a Table. It is pure over bytes;
// folder-agreement is checked by ValidateSchemaTree.
func ParseTable(data []byte) (*Table, error) {
	var t Table
	if err := yaml.Unmarshal(data, &t); err != nil {
		return nil, fmt.Errorf("declare: parse %s: %w", tableFile, err)
	}
	return &t, nil
}

// DiscoveredTable is one table folder found under a schemas/ tree: its schema
// and table names (from the authoritative folders), its folder path, the parsed
// desired-state table.yaml, and whether an engine-written migrations/ ledger is
// present.
type DiscoveredTable struct {
	// Schema is the schema folder name (authoritative).
	Schema string
	// Table is the table folder name (authoritative).
	Table string
	// Dir is the table folder path.
	Dir string
	// Spec is the parsed table.yaml desired state.
	Spec *Table
	// Raw is the verbatim table.yaml bytes the folder was read from, hashed as-is by
	// ChecksumTableYAML to pin the create head recorded when a table is created from
	// its head with no migration files on disk. It is the authoritative source of that
	// checksum -- always present for a discovered table -- so provisioning never
	// checksums an absent ledger's nil bytes.
	Raw []byte
	// HasMigrations reports whether a migrations/ ledger directory is present.
	HasMigrations bool
}

// ValidateSchemaTree reads a schemas/ directory and returns its tables. The tree
// is a folder per schema and a folder per table; each table folder holds
// table.yaml (required) plus an optional engine-written migrations/ ledger. The
// schema:/table: keys in each table.yaml are validated against their folders,
// which are authoritative; a mismatch is rejected (specification section 3).
func ValidateSchemaTree(schemasDir string) ([]DiscoveredTable, error) {
	schemaEntries, err := os.ReadDir(schemasDir)
	if err != nil {
		return nil, fmt.Errorf("declare: read schemas directory %s: %w", schemasDir, err)
	}

	var tables []DiscoveredTable
	for _, se := range schemaEntries {
		name := se.Name()
		if isHidden(name) {
			continue
		}
		if !se.IsDir() {
			return nil, fmt.Errorf("declare: schemas/%s is not a schema folder; schemas/ holds one folder per schema", name)
		}
		schemaDir := filepath.Join(schemasDir, name)
		tableEntries, err := os.ReadDir(schemaDir)
		if err != nil {
			return nil, fmt.Errorf("declare: read schema folder %s: %w", schemaDir, err)
		}
		for _, te := range tableEntries {
			tname := te.Name()
			if isHidden(tname) {
				continue
			}
			if !te.IsDir() {
				return nil, fmt.Errorf("declare: schemas/%s/%s is not a table folder; a schema folder holds one folder per table", name, tname)
			}
			tbl, err := validateTableFolder(name, tname, filepath.Join(schemaDir, tname))
			if err != nil {
				return nil, err
			}
			tables = append(tables, tbl)
		}
	}
	return tables, nil
}

// validateTableFolder validates one table folder: it must hold table.yaml whose
// schema:/table: keys match the schema and table folder names.
func validateTableFolder(schemaName, tableName, dir string) (DiscoveredTable, error) {
	tablePath := filepath.Join(dir, tableFile)
	data, err := readFile(tablePath)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return DiscoveredTable{}, fmt.Errorf("declare: table folder schemas/%s/%s has no %s (its desired-state file)", schemaName, tableName, tableFile)
		}
		return DiscoveredTable{}, fmt.Errorf("declare: read %s: %w", tablePath, err)
	}
	spec, err := ParseTable(data)
	if err != nil {
		return DiscoveredTable{}, err
	}
	if spec.Schema != schemaName {
		return DiscoveredTable{}, fmt.Errorf("declare: %s schema key %q does not match its folder %q; folder names are authoritative", tablePath, spec.Schema, schemaName)
	}
	if spec.Table != tableName {
		return DiscoveredTable{}, fmt.Errorf("declare: %s table key %q does not match its folder %q; folder names are authoritative", tablePath, spec.Table, tableName)
	}

	hasMigrations := false
	if info, err := os.Stat(filepath.Join(dir, migrationsDirName)); err == nil && info.IsDir() {
		hasMigrations = true
	}
	return DiscoveredTable{
		Schema:        schemaName,
		Table:         tableName,
		Dir:           dir,
		Spec:          spec,
		Raw:           data,
		HasMigrations: hasMigrations,
	}, nil
}
