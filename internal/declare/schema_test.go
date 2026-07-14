package declare_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/MateusAMP2119/iris-engine-cli/internal/declare"
	"github.com/MateusAMP2119/iris-engine-cli/internal/fixtures"
)

// writeTree writes each path->contents entry under root, creating parent
// directories. A path ending in "/" creates an empty directory. It returns
// root for convenience.
func writeTree(t *testing.T, root string, files map[string]string) string {
	t.Helper()
	for rel, contents := range files {
		full := filepath.Join(root, rel)
		if strings.HasSuffix(rel, "/") {
			if err := os.MkdirAll(full, 0o755); err != nil {
				t.Fatalf("mkdir %s: %v", full, err)
			}
			continue
		}
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			t.Fatalf("mkdir parent of %s: %v", full, err)
		}
		if err := os.WriteFile(full, []byte(contents), 0o644); err != nil {
			t.Fatalf("write %s: %v", full, err)
		}
	}
	return root
}

// tableIndex maps "schema.table" to the discovered table for lookups.
func tableIndex(tables []declare.DiscoveredTable) map[string]declare.DiscoveredTable {
	idx := make(map[string]declare.DiscoveredTable, len(tables))
	for _, tbl := range tables {
		idx[tbl.Schema+"."+tbl.Table] = tbl
	}
	return idx
}

// TestSchemasTreeShape proves the engine reads a schemas/ tree shaped as a
// folder per schema and per table, each table folder holding table.yaml plus an
// optional engine-written migrations/ ledger.
func TestSchemasTreeShape(t *testing.T) {
	t.Run("schemas-tree-shape", func(t *testing.T) {
		// Accept: the golden schemas/ tree resolves to its two tables, and the
		// table.yaml column shape (name, type, the four modifiers) parses.
		tables, err := declare.ValidateSchemaTree(filepath.Join(fixtures.WorkspaceGolden(), "schemas"))
		if err != nil {
			t.Fatalf("golden schemas tree rejected: %v", err)
		}
		idx := tableIndex(tables)
		for _, key := range []string{"raw.orders_staging", "analytics.orders"} {
			if _, ok := idx[key]; !ok {
				t.Errorf("golden schemas tree did not discover %s", key)
			}
		}
		orders, ok := idx["analytics.orders"]
		if !ok {
			t.Fatal("analytics.orders missing")
		}
		if orders.Spec == nil || len(orders.Spec.Columns) != 4 {
			t.Fatalf("analytics.orders columns = %v, want 4", orders.Spec)
		}
		id := orders.Spec.Columns[0]
		if id.Name != "id" || id.Type != "uuid" || !id.PrimaryKey {
			t.Errorf("first column = %+v, want id uuid primary_key", id)
		}
		created := orders.Spec.Columns[3]
		if created.Name != "created_at" || created.Default != "now()" {
			t.Errorf("created_at column = %+v, want default now()", created)
		}
		// The golden tables ship no migrations/ folder (engine-written, absent
		// until provisioning), so the shape accepts its absence.
		if orders.HasMigrations {
			t.Error("golden analytics.orders reports a migrations ledger; fixture has none")
		}

		// Accept: a table folder that also carries an engine-written migrations/.
		withMig := writeTree(t, t.TempDir(), map[string]string{
			"analytics/orders/table.yaml":                  "schema: analytics\ntable: orders\ncolumns:\n  - name: id\n    type: uuid\n    primary_key: true\n",
			"analytics/orders/migrations/0001_create.yaml": "id: \"0001\"\n",
		})
		got, err := declare.ValidateSchemaTree(withMig)
		if err != nil {
			t.Fatalf("schemas tree with migrations rejected: %v", err)
		}
		if len(got) != 1 || !got[0].HasMigrations {
			t.Errorf("HasMigrations not reported for a table folder holding migrations/: %+v", got)
		}

		reject := []struct {
			name  string
			files map[string]string
		}{
			{"table-folder-missing-table-yaml", map[string]string{
				"analytics/orders/": "",
			}},
			{"stray-file-at-schema-level", map[string]string{
				"analytics/orders/table.yaml": "schema: analytics\ntable: orders\ncolumns: []\n",
				"README.txt":                  "notes\n",
			}},
			{"stray-file-at-table-level", map[string]string{
				"analytics/orders/table.yaml": "schema: analytics\ntable: orders\ncolumns: []\n",
				"analytics/notes.txt":         "notes\n",
			}},
		}
		for _, tc := range reject {
			t.Run(tc.name, func(t *testing.T) {
				dir := writeTree(t, t.TempDir(), tc.files)
				if _, err := declare.ValidateSchemaTree(dir); err == nil {
					t.Errorf("%s accepted; expected rejection", tc.name)
				}
			})
		}
	})
}

// TestTableKeysMatchFolders proves that folder names under schemas/ are
// authoritative: the schema:/table: keys in table.yaml are validated against
// them and a mismatch is rejected.
func TestTableKeysMatchFolders(t *testing.T) {
	t.Run("table-keys-match-folders", func(t *testing.T) {
		// Accept: golden tables agree with their folders.
		if _, err := declare.ValidateSchemaTree(filepath.Join(fixtures.WorkspaceGolden(), "schemas")); err != nil {
			t.Fatalf("golden schemas tree rejected: %v", err)
		}

		reject := []struct {
			name  string
			files map[string]string
			want  string
		}{
			{
				name:  "schema-key-mismatch",
				files: map[string]string{"analytics/orders/table.yaml": "schema: wrong\ntable: orders\ncolumns: []\n"},
				want:  "analytics",
			},
			{
				name:  "table-key-mismatch",
				files: map[string]string{"analytics/orders/table.yaml": "schema: analytics\ntable: wrong\ncolumns: []\n"},
				want:  "orders",
			},
		}
		for _, tc := range reject {
			t.Run(tc.name, func(t *testing.T) {
				dir := writeTree(t, t.TempDir(), tc.files)
				_, err := declare.ValidateSchemaTree(dir)
				if err == nil {
					t.Fatalf("%s accepted; expected rejection", tc.name)
				}
				if !strings.Contains(err.Error(), tc.want) {
					t.Errorf("mismatch error %q does not name the authoritative folder %q", err, tc.want)
				}
			})
		}
	})
}
