package declare

import (
	"crypto/sha256"
	"fmt"

	"github.com/goccy/go-yaml"
)

// This file holds the migration-file format of specification section 5: the
// immutable additive-migration ledger file an engine writes under a table
// folder's migrations/ directory (e.g. migrations/0002_add_status.yaml). Only
// the file format lives here -- its fields, its canonical YAML shape, and its
// checksum semantics. Diffing table.yaml against the ledger and emitting the
// files (sync) is E03.7's; this leaf owns the durable on-disk shape those files
// must take.

// MigrationColumn is the column definition a migration file records: the name,
// YAML type, and optional raw-SQL default of the added column (specification
// section 5). It is the minimal shape the add_column op needs; the default is
// omitted when empty, matching the section 5 example.
type MigrationColumn struct {
	// Name is the added column's name.
	Name string `yaml:"name"`
	// Type is the added column's YAML type (mapped to Postgres by ResolveType).
	Type string `yaml:"type"`
	// Default is the added column's raw SQL default expression, verbatim; omitted
	// when empty.
	Default string `yaml:"default,omitempty"`
}

// MigrationFile is one immutable additive-migration ledger file (specification
// section 5). It records the migration id, its parent id, the operation, the
// column definition, and the checksum of table.yaml at this revision. The id and
// parent are zero-padded sequence strings (e.g. "0002", "0001") matching the
// migration filename's numeric prefix; string preserves the padding a bare
// integer would drop.
type MigrationFile struct {
	// ID is the zero-padded migration sequence id (e.g. "0002").
	ID string `yaml:"id"`
	// Parent is the predecessor migration's id (e.g. "0001").
	Parent string `yaml:"parent"`
	// Op is the migration operation; additive sync writes "add_column".
	Op string `yaml:"op"`
	// Column is the column definition the operation adds.
	Column MigrationColumn `yaml:"column"`
	// Checksum is the checksum of table.yaml at this revision (ChecksumTableYAML).
	Checksum string `yaml:"checksum"`
}

// MarshalMigration renders a migration file as its canonical YAML bytes, the
// on-disk form written under migrations/ (specification section 5). Field order
// is fixed by the struct: id, parent, op, column, checksum.
func MarshalMigration(m MigrationFile) ([]byte, error) {
	data, err := yaml.Marshal(m)
	if err != nil {
		return nil, fmt.Errorf("declare: marshal migration %q: %w", m.ID, err)
	}
	return data, nil
}

// ParseMigration parses a migration-file YAML document into a MigrationFile. It
// is pure over bytes and is the inverse of MarshalMigration.
func ParseMigration(data []byte) (MigrationFile, error) {
	var m MigrationFile
	if err := yaml.Unmarshal(data, &m); err != nil {
		return MigrationFile{}, fmt.Errorf("declare: parse migration file: %w", err)
	}
	return m, nil
}

// ChecksumTableYAML returns a migration file's checksum of table.yaml at a
// revision: the SHA-256 of the file's raw bytes, hex-encoded (specification
// section 5). The bytes are hashed verbatim, with no canonicalization or
// re-serialization, so the checksum pins the exact table.yaml revision the
// migration was cut from.
func ChecksumTableYAML(raw []byte) string {
	sum := sha256.Sum256(raw)
	return fmt.Sprintf("%x", sum)
}
