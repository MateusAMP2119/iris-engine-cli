package declare_test

import (
	"crypto/sha256"
	"fmt"
	"path/filepath"
	"testing"

	"github.com/MateusAMP2119/iris-engine-cli/internal/declare"
	"github.com/MateusAMP2119/iris-engine-cli/internal/golden"
)

// ordersWithStatus is the analytics.orders table.yaml at the revision the
// 0002_add_status migration advances to: the golden orders table plus the added
// status column. Its raw bytes are what the migration checksum pins.
const ordersWithStatus = `schema: analytics
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
  - name: status
    type: text
    default: "'pending'"
`

// TestMigrationFileFormat proves a generated migration file records id, parent,
// op, the column definition, and a checksum of table.yaml at that revision,
// marshaling to the specification section 5 example (golden) and round-tripping
// back losslessly.
func TestMigrationFileFormat(t *testing.T) {
	t.Run("S05/migration-file-format", func(t *testing.T) {
		raw := []byte(ordersWithStatus)

		// The checksum is the SHA-256 of the raw table.yaml bytes, hex-encoded:
		// the exact bytes at this revision, with no canonicalization.
		checksum := declare.ChecksumTableYAML(raw)
		if want := fmt.Sprintf("%x", sha256.Sum256(raw)); checksum != want {
			t.Errorf("ChecksumTableYAML = %q, want sha256 hex %q", checksum, want)
		}
		if len(checksum) != 64 {
			t.Errorf("checksum length = %d, want 64 hex chars", len(checksum))
		}

		// The migration file records id, parent, op, the column definition, and the
		// checksum, and marshals to the section 5 example shape byte-for-byte.
		m := declare.MigrationFile{
			ID:       "0002",
			Parent:   "0001",
			Op:       "add_column",
			Column:   declare.MigrationColumn{Name: "status", Type: "text", Default: "'pending'"},
			Checksum: checksum,
		}
		out, err := declare.MarshalMigration(m)
		if err != nil {
			t.Fatalf("MarshalMigration: %v", err)
		}
		golden.Assert(t, out, filepath.Join("testdata", "0002_add_status.yaml"))

		// The file round-trips: unmarshaling reproduces every recorded field.
		back, err := declare.ParseMigration(out)
		if err != nil {
			t.Fatalf("ParseMigration: %v", err)
		}
		if back != m {
			t.Errorf("round-trip migration = %+v, want %+v", back, m)
		}
	})
}
