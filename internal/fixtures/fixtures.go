// Package fixtures resolves the absolute paths of Iris's shared test fixtures
// so any consumer package -- E03 declaration parsing, E13 acceptance, the
// golden and round-trip harnesses -- reaches the same checked-in inventory
// without duplicating it. Paths are resolved from this source file's location,
// so they are stable regardless of the caller's working directory.
//
// The inventory (the S16/fixture-inventory convention) lives under testdata/:
//
//	workspace/golden/            the golden sample workspace (spec sections 3, 5, 7, 10, 13)
//	  pipelines/ingest/          the ingest lane
//	    iris-declare.yaml        lane composer: lane + order (extract_orders, reset_counters, load_orders)
//	    extract_orders/          reads + writes raw.orders_staging; no depends_on
//	      iris-declare.yaml
//	      main.py
//	    reset_counters/          composer-ordered only, no reads/writes, no depends_on
//	      iris-declare.yaml
//	      main.py
//	    load_orders/             depends_on extract_orders; reads staging, writes analytics.orders
//	      iris-declare.yaml
//	      main.py
//	  schemas/
//	    raw/orders_staging/table.yaml
//	    analytics/orders/table.yaml
//	  endpoints/orders_by_customer.yaml
//	declarations/invalid/        deliberately invalid declarations E03's validator must reject
//	  unknown_field/             a field outside the eight allowed keys
//	  missing_name/              no name
//	  missing_run/               no run
//	  name_folder_mismatch/      name does not match its folder
//	  cycle/                     a depends_on cycle (a -> b -> a)
//	  lane_no_composer/          a 2+ pipeline lane with no composer
//
// A golden diff is a contract diff: any change to these fixtures ships with its
// specification delta.
package fixtures

import (
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
	"testing"
)

// resolveRoot returns the absolute path to this package's testdata directory,
// derived from this source file's own location.
func resolveRoot() string {
	_, file, _, ok := runtime.Caller(0)
	if !ok {
		return "testdata"
	}
	return filepath.Join(filepath.Dir(file), "testdata")
}

// WorkspaceGolden returns the absolute path to the golden sample workspace: the
// complete sample project (ingest lane + composer, raw and analytics schemas,
// the orders_by_customer endpoint) from specification sections 3, 5, 7, 10 and
// 13.
func WorkspaceGolden() string {
	return filepath.Join(resolveRoot(), "workspace", "golden")
}

// InvalidDeclarations returns the absolute path to the root of the deliberately
// invalid declaration fixtures, one subdirectory per defect.
func InvalidDeclarations() string {
	return filepath.Join(resolveRoot(), "declarations", "invalid")
}

// InvalidDeclaration returns the absolute path to the named invalid declaration
// fixture directory under InvalidDeclarations.
func InvalidDeclaration(name string) string {
	return filepath.Join(InvalidDeclarations(), name)
}

// Path returns the absolute path to elem joined under the fixtures testdata
// root.
func Path(elem ...string) string {
	return filepath.Join(append([]string{resolveRoot()}, elem...)...)
}

// MaterializeGolden copies the golden sample workspace into a fresh temp
// directory and returns its absolute path. The returned tree is writable and
// includes the pipelines/, schemas/, and endpoints/ trees exactly as checked
// in, with traversable directories (0755) and regular files (0644). The
// testing.TB's cleanup removes the tree on test exit.
func MaterializeGolden(t testing.TB) string {
	t.Helper()
	dir, err := os.MkdirTemp("", "iris-golden-*")
	if err != nil {
		t.Fatalf("materialize golden workspace: %v", err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })

	golden := WorkspaceGolden()
	// Copy the three top-level trees that make a workspace: pipelines, schemas, endpoints.
	for _, sub := range []string{"pipelines", "schemas", "endpoints"} {
		src := filepath.Join(golden, sub)
		// If the subdir does not exist in this golden snapshot, skip (future-proof).
		if _, err := os.Stat(src); err != nil {
			if os.IsNotExist(err) {
				continue
			}
			t.Fatalf("stat golden %s: %v", sub, err)
		}
		if err := copyTree(src, filepath.Join(dir, sub)); err != nil {
			t.Fatalf("copy golden %s: %v", sub, err)
		}
	}
	return dir
}

// copyTree copies src tree to dst, creating parents, using 0755 for dirs and
// 0644 for files (workspace artifact convention).
func copyTree(src, dst string) error {
	return filepath.WalkDir(src, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(src, path)
		if err != nil {
			return err
		}
		target := filepath.Join(dst, rel)
		if d.IsDir() {
			return os.MkdirAll(target, 0o755)
		}
		data, err := os.ReadFile(path) //nolint:gosec // G304: repo-controlled golden fixture
		if err != nil {
			return err
		}
		if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
			return err
		}
		return os.WriteFile(target, data, 0o644) //nolint:gosec // G306: workspace file content
	})
}
