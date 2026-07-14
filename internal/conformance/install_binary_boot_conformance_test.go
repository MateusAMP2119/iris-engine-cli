//go:build conformance

package conformance

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/MateusAMP2119/iris-engine-cli/internal/fixtures"
)

// requireMetaAndData connects to the cluster the admin DSN dsn points at and fails
// the test unless both the meta and data databases exist. It is the install-only
// probe: it reaches the databases through an independent admin connection, never
// through a running daemon, so it proves `iris engine install` itself created them
// (not a later `engine start`).
func requireMetaAndData(t *testing.T, dsn string) {
	t.Helper()
	conn, err := pgx.Connect(context.Background(), dsn)
	if err != nil {
		t.Fatalf("connect admin cluster for pg_database probe: %v", err)
	}
	defer conn.Close(context.Background())

	rows, err := conn.Query(context.Background(),
		"SELECT datname FROM pg_database WHERE datname IN ('meta', 'data')")
	if err != nil {
		t.Fatalf("query pg_database: %v", err)
	}
	defer rows.Close()

	found := map[string]bool{}
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			t.Fatalf("scan datname: %v", err)
		}
		found[name] = true
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("pg_database rows: %v", err)
	}
	if !found["meta"] || !found["data"] {
		t.Errorf("after iris engine install, expected both meta and data databases; got %v", found)
	}
}

// TestInstallCreatesMetaAndData drives the real binary and proves that
// `iris engine install` creates the meta database alongside the data database
// (acceptance step 1: "Install creates meta alongside the
// data database"). The assertion is install-only: it probes the cluster through an
// independent admin connection, before any daemon runs, so it proves install itself
// created both databases rather than a later start.
//
// The probe needs an admin connection to the cluster while no daemon is running.
// External mode (IRIS_PG_DSN, the CI path) provides exactly that, so the assertion
// runs there. Managed mode leaves the local Postgres stopped after install, so
// reaching it install-only would mean re-supervising the engine's own managed
// cluster; the managed one-code-path bring-up is proven end to end in
// TestInstallStartOneCodepath instead. When no external DSN is configured this test
// skips rather than assert against a cluster it cannot reach.
func TestInstallCreatesMetaAndData(t *testing.T) {
	t.Run("install-creates-meta-and-data", func(t *testing.T) {
		dsn := os.Getenv("IRIS_PG_DSN")
		if dsn == "" {
			t.Skip("install-creates-meta-and-data: set IRIS_PG_DSN to probe the install-created databases through an independent admin connection (managed-mode bring-up is covered by TestInstallStartOneCodepath)")
		}

		bin := Build(t)
		ws := shortWorkspace(t)

		bin.Run(t, RunOptions{
			Args:    []string{"engine", "install"},
			Dir:     ws,
			Timeout: 5 * time.Minute,
		}).RequireExit(t, 0)

		requireMetaAndData(t, dsn)
	})
}

// TestInstallStartOneCodepath proves that install plus start brings up the engine
// for managed Postgres (no IRIS_PG_DSN) and for external mode (IRIS_PG_DSN present)
// through one shared code path (acceptance step 1). Both
// legs install, start a detached daemon, and require the daemon to reach the leader
// role -- the single writer only a genuinely connected engine attains -- so the same
// install+start sequence is proven functional in each mode.
func TestInstallStartOneCodepath(t *testing.T) {
	cases := []struct {
		name string
		set  func(*testing.T)
	}{
		{name: "managed", set: func(t *testing.T) { t.Setenv("IRIS_PG_DSN", "") }},
		{name: "external", set: func(_ *testing.T) { /* leave IRIS_PG_DSN as provided */ }},
	}
	for _, c := range cases {
		c := c
		t.Run("install-start-one-codepath/"+c.name, func(t *testing.T) {
			c.set(t)
			// Freshen the shared external cluster first (managed leg sets IRIS_PG_DSN="",
			// where freshDatabases is a no-op): FORCE-dropping meta/data evicts a prior
			// test's lingering daemon sessions -- including a still-held leader advisory
			// lock -- so this daemon elects promptly instead of timing out behind a stale
			// leader.
			freshDatabases(t)
			bin := Build(t)
			ws := shortWorkspace(t)

			bin.Run(t, RunOptions{
				Args:    []string{"engine", "install"},
				Dir:     ws,
				Timeout: 5 * time.Minute,
			}).RequireExit(t, 0)

			bin.Run(t, RunOptions{
				Args:    []string{"engine", "start", "-d"},
				Dir:     ws,
				Timeout: 2 * time.Minute,
			}).RequireExit(t, 0)
			t.Cleanup(func() {
				bin.Run(t, RunOptions{
					Args:    []string{"engine", "stop"},
					Dir:     ws,
					Timeout: 30 * time.Second,
				})
			})

			socket := filepath.Join(ws, ".iris", "iris.sock")
			readyCtx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			if err := WaitForSocket(readyCtx, socket); err != nil {
				cancel()
				t.Fatalf("daemon socket never ready after start in %s mode: %v", c.name, err)
			}
			cancel()

			if !waitForLeader(t, socket) {
				t.Fatalf("daemon did not become leader after start in %s mode", c.name)
			}
		})
	}
}

// TestStaticCrossCompileBoot builds the engine with explicit static cross-compile env
// (CGO_ENABLED=0) for the host platform and proves the resulting binary boots with no
// host runtime -- a bare invocation exits 0 -- and that the static binary drives a
// full engine install: install exits 0 and, in external mode, both the meta and data
// databases exist afterward (probed through an independent admin connection).
func TestStaticCrossCompileBoot(t *testing.T) {
	t.Run("static-cross-compile-boot", func(t *testing.T) {
		tmp := t.TempDir()
		out := filepath.Join(tmp, binName())
		cmd := exec.Command("go", "build", "-o", out, irisPkg)
		cmd.Env = append(os.Environ(),
			"CGO_ENABLED=0",
			"GOOS="+runtime.GOOS,
			"GOARCH="+runtime.GOARCH,
		)
		if combined, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("static cross-compile build failed: %v\n%s", err, combined)
		}

		// Boot the produced static binary (no cgo, no host runtime beyond kernel): a
		// bare invocation prints help and exits 0.
		c := exec.Command(out)
		err := c.Run()
		exit := 0
		if ee, ok := err.(*exec.ExitError); ok {
			exit = ee.ExitCode()
		} else if err != nil {
			t.Fatalf("static binary failed to start: %v", err)
		}
		if exit != 0 {
			t.Errorf("static cross-compiled binary exited %d on bare boot, want 0", exit)
		}

		// The static binary drives a real engine install: it boots its full logic and
		// creates the engine's databases.
		ws := shortWorkspace(t)
		sbin := &Binary{path: out}
		sbin.Run(t, RunOptions{
			Args:    []string{"engine", "install"},
			Dir:     ws,
			Timeout: 5 * time.Minute,
		}).RequireExit(t, 0)

		// External mode: prove the static binary's install created both databases,
		// probed through an independent admin connection (no running daemon). Managed
		// mode leaves the local cluster stopped after install, so its bring-up is
		// proven end to end in TestInstallStartOneCodepath.
		if dsn := os.Getenv("IRIS_PG_DSN"); dsn != "" {
			requireMetaAndData(t, dsn)
		} else {
			t.Log("static-cross-compile-boot: no IRIS_PG_DSN; the static binary booted and installed, meta+data probe skipped (managed bring-up covered by TestInstallStartOneCodepath)")
		}
	})
}

// TestCrossCompileSmoke builds cross-compiled static binaries for linux and macos on
// amd64 and arm64. Every target must build (the static-binary cross-compile
// guarantee). For the target matching the current host it exercises the full smoke
// cycle: boot the binary, install, start detached, apply the golden sample graph,
// assert leadership, and tear down -- asserting exit codes throughout. Non-host
// targets cannot be executed here (cross-arch), so they are build-only; in a matrix
// CI each job runs its native target's full cycle.
func TestCrossCompileSmoke(t *testing.T) {
	targets := []struct{ goos, goarch string }{
		{"linux", "amd64"},
		{"linux", "arm64"},
		{"darwin", "amd64"},
		{"darwin", "arm64"},
	}
	for _, tgt := range targets {
		tgt := tgt
		name := tgt.goos + "/" + tgt.goarch
		t.Run("cross-compile-smoke/"+name, func(t *testing.T) {
			tmp := t.TempDir()
			out := filepath.Join(tmp, "iris")
			if tgt.goos == "windows" {
				out += ".exe"
			}
			build := exec.Command("go", "build", "-o", out, irisPkg)
			build.Env = append(os.Environ(),
				"CGO_ENABLED=0",
				"GOOS="+tgt.goos,
				"GOARCH="+tgt.goarch,
			)
			if combined, err := build.CombinedOutput(); err != nil {
				t.Fatalf("cross build %s: %v\n%s", name, err, combined)
			}

			// Only the host-matching target can be executed here. In a matrix CI each
			// job exercises its native target end-to-end.
			if tgt.goos != runtime.GOOS || tgt.goarch != runtime.GOARCH {
				t.Logf("cross-compile only (host %s/%s cannot exec %s); build succeeded",
					runtime.GOOS, runtime.GOARCH, name)
				return
			}

			// Freshen the shared external cluster first: FORCE-dropping meta/data
			// evicts a prior test's lingering daemon sessions (including a still-held
			// leader advisory lock), so this daemon elects promptly instead of timing
			// out behind a stale leader.
			freshDatabases(t)
			bin := &Binary{path: out}
			ws := shortWorkspace(t)
			copyGoldenWorkspace(t, ws)
			socket := filepath.Join(ws, ".iris", "iris.sock")

			// Boot + install + start the cross-built static binary.
			bin.Run(t, RunOptions{Args: []string{"engine", "install"}, Dir: ws, Timeout: 5 * time.Minute}).RequireExit(t, 0)
			bin.Run(t, RunOptions{Args: []string{"engine", "start", "-d"}, Dir: ws, Timeout: 2 * time.Minute}).RequireExit(t, 0)
			t.Cleanup(func() {
				bin.Run(t, RunOptions{Args: []string{"engine", "stop"}, Dir: ws, Timeout: 30 * time.Second})
			})

			readyCtx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			if err := WaitForSocket(readyCtx, socket); err != nil {
				cancel()
				t.Fatalf("smoke %s: daemon socket never became ready: %v", name, err)
			}
			cancel()
			if !waitForLeader(t, socket) {
				t.Fatalf("smoke %s: daemon never became leader; cannot apply against the single writer", name)
			}

			// Apply the golden sample graph, upstream-first (acceptance step 2),
			// each apply exit 0.
			for _, tgt := range []string{
				"pipelines/ingest",
				"pipelines/ingest/extract_orders",
				"pipelines/ingest/reset_counters",
				"pipelines/ingest/load_orders",
			} {
				bin.Run(t, RunOptions{Args: []string{"declare", "apply", tgt}, Dir: ws}).RequireExit(t, 0)
			}

			// Teardown: stop the daemon and uninstall, each exit 0.
			bin.Run(t, RunOptions{Args: []string{"engine", "stop"}, Dir: ws, Timeout: 30 * time.Second}).RequireExit(t, 0)
			bin.Run(t, RunOptions{Args: []string{"engine", "uninstall", "--yes"}, Dir: ws, Timeout: 2 * time.Minute}).RequireExit(t, 0)
		})
	}
}

// referenced to keep the fixtures import used regardless of build path.
var _ = fixtures.WorkspaceGolden
