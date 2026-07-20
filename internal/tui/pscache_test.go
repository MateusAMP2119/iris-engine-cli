package tui

import (
	"testing"
	"time"

	"github.com/MateusAMP2119/iris-lakehouse/internal/api"
)

// TestPsCacheRoundTrip proves the last-known-state cache: a saved snapshot
// loads back payload and listing intact with a recent save moment, the log
// tail is dropped, a different target misses, and a nil handle stays inert.
func TestPsCacheRoundTrip(t *testing.T) {
	t.Run("ps-cache-roundtrip", func(t *testing.T) {
		t.Setenv("IRIS_HOME", t.TempDir())
		c := newCache("unix:///tmp/iris-test.sock")
		if c == nil {
			t.Fatal("newCache resolved no handle under IRIS_HOME")
		}
		if _, _, ok := c.load(); ok {
			t.Fatal("an empty cache must miss")
		}

		snap := Snapshot{Ps: psFixture(), Pipelines: []api.PipelineListItem{{Name: "extract", Lane: "ingest"}}}
		snap.Logs, snap.LogsRun = []string{"secret line"}, "7"
		c.save(snap)

		got, savedAt, ok := c.load()
		if !ok {
			t.Fatal("a saved cache must load")
		}
		if got.Ps.Engine.PID != snap.Ps.Engine.PID || len(got.Ps.Runs) != len(snap.Ps.Runs) {
			t.Errorf("payload did not round-trip: %+v", got.Ps.Engine)
		}
		if len(got.Pipelines) != 1 || got.Pipelines[0].Name != "extract" {
			t.Errorf("listing did not round-trip: %+v", got.Pipelines)
		}
		if len(got.Logs) != 0 || got.LogsRun != "" {
			t.Errorf("the log tail must never be cached: %v", got.Logs)
		}
		if age := time.Since(savedAt); age < 0 || age > time.Minute {
			t.Errorf("save moment = %v ago, want just now", age)
		}

		if _, _, ok := newCache("unix:///tmp/other.sock").load(); ok {
			t.Error("a different target must miss the cache")
		}

		var nilCache *cache
		nilCache.save(snap) // must not panic
		if _, _, ok := nilCache.load(); ok {
			t.Error("a nil handle must miss")
		}
	})
}
