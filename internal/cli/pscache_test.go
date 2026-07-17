package cli

import (
	"bytes"
	"strings"
	"testing"
	"time"

	"github.com/spf13/cobra"

	"github.com/MateusAMP2119/iris-lakehouse/internal/api"
	"github.com/MateusAMP2119/iris-lakehouse/internal/config"
)

// TestPsCacheRoundTrip proves the last-known-state cache: a saved snapshot
// loads back payload and listing intact with a recent save moment, the log
// tail is dropped, a different target misses, and a nil handle stays inert.
func TestPsCacheRoundTrip(t *testing.T) {
	t.Run("ps-cache-roundtrip", func(t *testing.T) {
		t.Setenv("IRIS_HOME", t.TempDir())
		c := newPsCache("unix:///tmp/iris-test.sock")
		if c == nil {
			t.Fatal("newPsCache resolved no handle under IRIS_HOME")
		}
		if _, _, ok := c.load(); ok {
			t.Fatal("an empty cache must miss")
		}

		snap := psSnapshot{ps: psFixture(), pipelines: []api.PipelineListItem{{Name: "extract", Lane: "ingest"}}}
		snap.logs, snap.logsRun = []string{"secret line"}, "7"
		c.save(snap)

		got, savedAt, ok := c.load()
		if !ok {
			t.Fatal("a saved cache must load")
		}
		if got.ps.Engine.PID != snap.ps.Engine.PID || len(got.ps.Runs) != len(snap.ps.Runs) {
			t.Errorf("payload did not round-trip: %+v", got.ps.Engine)
		}
		if len(got.pipelines) != 1 || got.pipelines[0].Name != "extract" {
			t.Errorf("listing did not round-trip: %+v", got.pipelines)
		}
		if len(got.logs) != 0 || got.logsRun != "" {
			t.Errorf("the log tail must never be cached: %v", got.logs)
		}
		if age := time.Since(savedAt); age < 0 || age > time.Minute {
			t.Errorf("save moment = %v ago, want just now", age)
		}

		if _, _, ok := newPsCache("unix:///tmp/other.sock").load(); ok {
			t.Error("a different target must miss the cache")
		}

		var nilCache *psCache
		nilCache.save(snap) // must not panic
		if _, _, ok := nilCache.load(); ok {
			t.Error("a nil handle must miss")
		}
	})
}

// TestPsStaleOpen proves the unreachable-at-open revival: a dead engine with a
// cached last known state opens the live view on the cached snapshot marked
// stale, and without a cache keeps the docker-shaped no-daemon fault.
func TestPsStaleOpen(t *testing.T) {
	t.Setenv("IRIS_HOST", "")
	t.Setenv("IRIS_SOCKET", "")
	t.Setenv("IRIS_TOKEN", "")

	t.Run("ps-stale-open", func(t *testing.T) {
		t.Setenv("IRIS_HOME", t.TempDir())
		sock := shortSocket(t) // nothing ever listens here

		// Seed the cache exactly as a healthy view would for this target.
		var seed bytes.Buffer
		seedApp := newApp(&seed, &seed)
		seedClient := seedApp.newPsDaemonClient(config.Settings{Socket: sock})
		seedClient.cache.save(psSnapshot{ps: psFixture()})

		var out, errb bytes.Buffer
		a := newApp(&out, &errb)
		a.isTTY = func() bool { return true }
		a.stdinIsTTY = func() bool { return true }
		var opened psSnapshot
		a.psLive = func(_ *cobra.Command, _ *psDaemonClient, first psSnapshot, _ string) (bool, error) {
			opened = first
			return true, nil
		}
		if code := a.run([]string{"--socket", sock, "ps"}); code != exitOK {
			t.Fatalf("stale-open ps exit = %d, want %d\nstderr: %s", code, exitOK, errb.String())
		}
		if opened.staleAge <= 0 {
			t.Fatal("the revived snapshot must be marked stale")
		}
		if opened.ps.Engine.PID != psFixture().Engine.PID {
			t.Errorf("revived payload = %+v, want the cached fixture", opened.ps.Engine)
		}
		// The model opens it under the unreachable banner.
		m := newPsModel(opened, "local "+sock)
		if !strings.Contains(m.warn, "engine unreachable") || !strings.Contains(m.warn, "cached") {
			t.Errorf("stale open warn = %q, want the unreachable banner with the cache age", m.warn)
		}
	})

	t.Run("ps-stale-open-without-cache-faults", func(t *testing.T) {
		t.Setenv("IRIS_HOME", t.TempDir())
		sock := shortSocket(t)
		var out, errb bytes.Buffer
		a := newApp(&out, &errb)
		a.isTTY = func() bool { return true }
		a.stdinIsTTY = func() bool { return true }
		a.psLive = func(*cobra.Command, *psDaemonClient, psSnapshot, string) (bool, error) {
			t.Fatal("the view must not open with neither engine nor cache")
			return true, nil
		}
		if code := a.run([]string{"--socket", sock, "ps"}); code != exitNoDaemon {
			t.Fatalf("cacheless dead-engine ps exit = %d, want %d", code, exitNoDaemon)
		}
	})
}

