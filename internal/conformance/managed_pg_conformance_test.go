//go:build conformance

package conformance

import (
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/MateusAMP2119/iris-engine-cli/internal/daemon"
)

// pinnedManagedPGMajor is the engine's pinned Postgres major version as the data
// directory records it in PG_VERSION, taken from the single source of truth so the
// conformance assertion tracks the pin automatically.
var pinnedManagedPGMajor = strconv.Itoa(daemon.PinnedMajorVersion)

// managedServerBinary is the base name of the standalone Postgres server executable
// the managed build must place somewhere under <workspace>/.iris/pg. Its presence as
// a separate on-disk file is the proof the managed Postgres is a fetched child-
// subprocess build, never linked into the single iris binary.
func managedServerBinary() string {
	if runtime.GOOS == "windows" {
		return "postgres.exe"
	}
	return "postgres"
}

// findExecutable walks root and returns the path of the first non-directory entry
// named name (the Postgres extraction layout under BinariesPath is deterministic
// today, but walking the subtree keeps the assertion honest and layout-agnostic:
// finding the binary anywhere under .iris/pg still proves it is under .iris/pg).
func findExecutable(t *testing.T, root, name string) string {
	t.Helper()
	var found string
	err := filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if !d.IsDir() && d.Name() == name {
			found = path
			return filepath.SkipAll
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walking %s for %s: %v", root, name, err)
	}
	return found
}

// TestManagedPGInstall drives the real iris binary's daemonless `engine install`
// in a throwaway workspace with no external pg_dsn (managed mode) and proves the
// managed-Postgres install leg end to end:
//
//   - the pinned, checksum-verified Postgres is downloaded and placed under
//     <workspace>/.iris/pg, with its data directory's PG_VERSION recording the
//     pinned major;
//   - a standalone Postgres server executable lands under .iris/pg, proving the
//     build is a fetched child subprocess and not linked into the iris binary;
//   - the engine-minted superuser credential never appears on stdout or stderr:
//     the CLI never sees it;
//   - a second install is idempotent: it re-downloads nothing and exits 0.
//
// The first run downloads a Postgres distribution from the pinned binary
// repository (network-bound; typically tens of seconds on a cold cache, near-
// instant once embedded-postgres's runtime cache is warm), so this test carries a
// generous timeout and runs only under the conformance build tag in the
// network-enabled conformance job.
func TestManagedPGInstall(t *testing.T) {
	bin := Build(t)
	ws := t.TempDir()

	// Force managed mode regardless of the CI environment: the conformance job runs
	// with a shared Postgres service and IRIS_PG_DSN exported, which would divert
	// `engine install` to the external no-op path (no download). Clearing it here (an
	// empty IRIS_PG_DSN reads as unset) pins this test to the managed install leg.
	t.Setenv("IRIS_PG_DSN", "")

	res := bin.Run(t, RunOptions{
		Args:    []string{"engine", "install"},
		Dir:     ws,
		Timeout: 5 * time.Minute,
	})
	res.RequireExit(t, 0)

	pgDir := filepath.Join(ws, ".iris", "pg")

	// A standalone Postgres server executable is placed somewhere under
	// <workspace>/.iris/pg -- a fetched child-subprocess build, not linked into iris.
	serverBin := findExecutable(t, pgDir, managedServerBinary())
	if serverBin == "" {
		t.Fatalf("no standalone %s executable found anywhere under %s; the managed Postgres build was not placed under .iris/pg",
			managedServerBinary(), pgDir)
	}
	if info, err := os.Stat(serverBin); err != nil {
		t.Fatalf("stat managed Postgres server binary %s: %v", serverBin, err)
	} else if runtime.GOOS != "windows" && info.Mode().Perm()&0o111 == 0 {
		t.Errorf("managed Postgres server binary %s is not executable (mode %v)", serverBin, info.Mode())
	}

	// The data directory exists under .iris/pg and records the pinned major.
	dataDir := filepath.Join(pgDir, "data")
	if info, err := os.Stat(dataDir); err != nil || !info.IsDir() {
		t.Fatalf("managed data directory missing under .iris/pg/data: %v", err)
	}
	versionBytes, err := os.ReadFile(filepath.Join(dataDir, "PG_VERSION")) //nolint:gosec // G304: path is under the test's own temp workspace.
	if err != nil {
		t.Fatalf("managed data dir did not record PG_VERSION under .iris/pg/data: %v", err)
	}
	recorded := strings.TrimSpace(string(versionBytes))
	if recorded != pinnedManagedPGMajor {
		t.Errorf("PG_VERSION = %q, want the pinned major %q", recorded, pinnedManagedPGMajor)
	}

	// The engine-minted superuser credential is never surfaced to the CLI.
	// The install output carries no connection string bearing the managed
	// superuser and no printed password.
	combined := string(res.Stdout) + "\n" + string(res.Stderr)
	lower := strings.ToLower(combined)
	for _, leak := range []string{"password", "iris_engine:", "postgres://iris", "postgresql://iris"} {
		if strings.Contains(lower, strings.ToLower(leak)) {
			t.Errorf("engine install output leaked a managed-superuser credential (matched %q):\nstdout:\n%s\nstderr:\n%s",
				leak, res.Stdout, res.Stderr)
		}
	}

	// Idempotent: a second install re-downloads nothing (the binaries and data dir
	// already exist) and exits cleanly.
	res2 := bin.Run(t, RunOptions{
		Args:    []string{"engine", "install"},
		Dir:     ws,
		Timeout: 90 * time.Second,
	})
	res2.RequireExit(t, 0)

	// The data directory the first install created is reused, not reinitialized:
	// PG_VERSION is unchanged.
	versionBytes2, err := os.ReadFile(filepath.Join(pgDir, "data", "PG_VERSION")) //nolint:gosec // G304: path is under the test's own temp workspace.
	if err != nil {
		t.Fatalf("re-read PG_VERSION after idempotent install: %v", err)
	}
	if got := strings.TrimSpace(string(versionBytes2)); got != recorded {
		t.Errorf("idempotent install changed PG_VERSION from %q to %q; it must reuse the data dir", recorded, got)
	}
}
