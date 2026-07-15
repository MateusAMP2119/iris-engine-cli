package daemon_test

import (
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/MateusAMP2119/iris-lakehouse/internal/daemon"
)

// TestPGVersionMismatchFails proves the managed-Postgres version guard: when a data
// directory records a Postgres major version that differs from the engine release's
// pinned version, the guard fails fast with a typed error naming both versions, and
// it never rewrites or upgrades the data directory (never a silent auto-upgrade). A
// matching version passes, and a fresh data directory with no recorded version yet
// is not a mismatch (nothing to conflict with). A corrupt PG_VERSION also fails
// fast rather than being silently treated as a match (data directory records
// version; mismatch fails fast, never silent auto-upgrade).
func TestPGVersionMismatchFails(t *testing.T) {
	cases := []struct {
		name         string
		pgVersion    string // PG_VERSION file content; "" means the file is absent.
		want         int
		wantMismatch bool // expect ErrPGVersionMismatch
		wantErr      bool // expect any error (superset of wantMismatch)
	}{
		{"exact match", "18\n", 18, false, false},
		{"match without trailing newline", "18", 18, false, false},
		{"one major behind", "17\n", 18, true, true},
		{"several majors behind", "15\n", 18, true, true},
		{"one major ahead", "19\n", 18, true, true},
		{"legacy 9.6 dotted version", "9.6\n", 18, true, true},
		{"absent PG_VERSION is a fresh dir, not a mismatch", "", 18, false, false},
		{"corrupt version fails fast, never a silent match", "not-a-number\n", 18, false, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			pgVersionPath := filepath.Join(dir, "PG_VERSION")
			if tc.pgVersion != "" {
				if err := os.WriteFile(pgVersionPath, []byte(tc.pgVersion), 0o600); err != nil {
					t.Fatalf("seed PG_VERSION: %v", err)
				}
			}

			err := daemon.CheckDataDirVersion(dir, tc.want)

			switch {
			case tc.wantMismatch:
				if !errors.Is(err, daemon.ErrPGVersionMismatch) {
					t.Fatalf("CheckDataDirVersion = %v, want ErrPGVersionMismatch", err)
				}
				// The error must name both the recorded and the pinned major so the
				// operator sees exactly what mismatched -- never a silent proceed.
				msg := err.Error()
				recordedMajor := strings.TrimSpace(strings.SplitN(tc.pgVersion, ".", 2)[0])
				if !strings.Contains(msg, recordedMajor) {
					t.Errorf("mismatch error %q does not name the recorded version %q", msg, recordedMajor)
				}
				if !strings.Contains(msg, strconv.Itoa(tc.want)) {
					t.Errorf("mismatch error %q does not name the pinned version %d", msg, tc.want)
				}
			case tc.wantErr:
				if err == nil {
					t.Fatalf("CheckDataDirVersion = nil, want a fail-fast error for %q", tc.pgVersion)
				}
			default:
				if err != nil {
					t.Fatalf("CheckDataDirVersion = %v, want nil", err)
				}
			}

			// Never a silent auto-upgrade: the guard is read-only and must never
			// rewrite the recorded version. Whatever we seeded stays byte-for-byte.
			if tc.pgVersion != "" {
				after, readErr := os.ReadFile(pgVersionPath) //nolint:gosec // G304: pgVersionPath is a path under the test's own temp dir.
				if readErr != nil {
					t.Fatalf("re-read PG_VERSION: %v", readErr)
				}
				if string(after) != tc.pgVersion {
					t.Errorf("guard rewrote PG_VERSION from %q to %q; it must never touch the data dir", tc.pgVersion, after)
				}
			}
		})
	}
}

// TestPinnedMajorVersionExposed proves the engine pins a Postgres major version as
// a package constant (major version pinned per engine release). The guard compares
// a data directory against this pin, so the pin must be a real, positive major the
// rest of the engine can reference.
func TestPinnedMajorVersionExposed(t *testing.T) {
	if daemon.PinnedMajorVersion <= 0 {
		t.Fatalf("PinnedMajorVersion = %d, want a positive pinned major version", daemon.PinnedMajorVersion)
	}
	// The pin round-trips through the guard: a data dir recording exactly the
	// pinned major passes, proving the constant is the value the guard enforces.
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "PG_VERSION"), []byte(strconv.Itoa(daemon.PinnedMajorVersion)+"\n"), 0o600); err != nil {
		t.Fatalf("seed PG_VERSION: %v", err)
	}
	if err := daemon.CheckDataDirVersion(dir, daemon.PinnedMajorVersion); err != nil {
		t.Errorf("a data dir recording the pinned major %d failed the guard: %v", daemon.PinnedMajorVersion, err)
	}
}
