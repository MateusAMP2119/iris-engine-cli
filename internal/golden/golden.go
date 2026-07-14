// Package golden compares generated artifacts against checked-in golden files
// byte-for-byte and regenerates them in place under the -update flag.
//
// It is the executable form of the fixtures doctrine: every generated artifact
// -- SQL/DDL, migration YAML, --dry-run previews, --json output -- is diffed
// byte-for-byte against a checked-in golden and any difference fails the test.
// Running the suite with -update rewrites the goldens in place instead of
// failing on a diff. A golden diff is therefore a contract diff: a changed
// golden ships only with the deliberate change that produced it.
//
// This is test-support infrastructure imported only by _test.go files, so the
// -update flag it registers never reaches the production iris binary.
package golden

import (
	"bytes"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// update, bound to the -update test flag, makes Assert rewrite the golden file
// in place instead of failing on a diff.
var update = flag.Bool("update", false, "rewrite golden files in place instead of failing on a diff")

// UpdateEnabled reports whether the -update flag was set for this test run.
func UpdateEnabled() bool { return *update }

// Assert compares got byte-for-byte against the checked-in golden file at
// goldenPath and fails t on any difference. When the -update flag is set it
// rewrites the golden in place and does not fail.
func Assert(t testing.TB, got []byte, goldenPath string) {
	t.Helper()
	if err := check(got, goldenPath, *update); err != nil {
		t.Fatalf("%v", err)
	}
}

// check compares got against the golden at goldenPath, or (when update is true)
// rewrites the golden with got. It returns a non-nil error describing any
// mismatch. Splitting the pure logic out of Assert lets tests exercise both the
// match and mismatch paths without a fake testing.TB, which the testing package
// forbids implementing.
func check(got []byte, goldenPath string, update bool) error {
	if update {
		if err := os.MkdirAll(filepath.Dir(goldenPath), 0o755); err != nil {
			return fmt.Errorf("golden %s: create dir: %w", goldenPath, err)
		}
		if err := os.WriteFile(goldenPath, got, 0o644); err != nil {
			return fmt.Errorf("golden %s: rewrite: %w", goldenPath, err)
		}
		return nil
	}
	want, err := os.ReadFile(goldenPath) //nolint:gosec // G304: goldenPath is the harness-controlled path of a checked-in golden file, not user or network input.
	if err != nil {
		return fmt.Errorf("golden %s: %w (run the suite with -update to create it)", goldenPath, err)
	}
	if !bytes.Equal(got, want) {
		return fmt.Errorf(
			"golden %s: generated artifact differs from the checked-in golden "+
				"(a golden diff is a contract diff; run -update only when the change is deliberate)\n%s",
			goldenPath, diff(want, got))
	}
	return nil
}

// diff renders a short, dependency-free description of the first difference
// between want and got: the two lengths, the first differing byte offset, and
// the line straddling that offset on each side.
func diff(want, got []byte) string {
	var b strings.Builder
	fmt.Fprintf(&b, "  want %d bytes, got %d bytes\n", len(want), len(got))
	off := firstDiff(want, got)
	fmt.Fprintf(&b, "  first difference at byte %d\n", off)
	fmt.Fprintf(&b, "  want line: %q\n", lineAround(want, off))
	fmt.Fprintf(&b, "  got  line: %q", lineAround(got, off))
	return b.String()
}

// firstDiff returns the index of the first byte at which a and b differ, or the
// length of the shorter slice when one is a prefix of the other.
func firstDiff(a, b []byte) int {
	n := len(a)
	if len(b) < n {
		n = len(b)
	}
	for i := 0; i < n; i++ {
		if a[i] != b[i] {
			return i
		}
	}
	return n
}

// lineAround returns the line of data that contains byte offset off.
func lineAround(data []byte, off int) string {
	if off > len(data) {
		off = len(data)
	}
	start := bytes.LastIndexByte(data[:off], '\n') + 1
	end := len(data)
	if idx := bytes.IndexByte(data[off:], '\n'); idx >= 0 {
		end = off + idx
	}
	if start > end {
		start = end
	}
	return string(data[start:end])
}
