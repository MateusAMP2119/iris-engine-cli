package daemon_test

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/MateusAMP2119/iris-engine-cli/internal/daemon"
)

// TestDaemonLogRotation proves the daemon log rotates by SIZE only, at a bounded
// threshold, keeping a fixed number of generations and dropping the oldest, with
// no time-based rotation anywhere (specification section 2: "Size-based rotation
// only, never time-based: daemon log 10 MB, 5 generations"). The rotator takes
// the threshold and generation count as parameters so the test can force many
// rotations with a tiny threshold; the production constants (DaemonLogMaxBytes,
// DaemonLogGenerations) are asserted separately to be 10 MB and 5.
func TestDaemonLogRotation(t *testing.T) {
	// spec: S02/daemon-log-rotation
	t.Run("S02/daemon-log-rotation", func(t *testing.T) {
		t.Run("rotates by size keeping exactly N generations, oldest dropped", func(t *testing.T) {
			dir := t.TempDir()
			path := filepath.Join(dir, "daemon.log")

			const (
				maxBytes    = int64(100)
				generations = 3
				lineLen     = 50 // each write is exactly 50 bytes
				writes      = 40 // far more than enough to roll past N generations
			)
			rot, err := daemon.NewSizeRotator(path, maxBytes, generations)
			if err != nil {
				t.Fatalf("NewSizeRotator: %v", err)
			}
			line := []byte(strings.Repeat("A", lineLen-1) + "\n")
			for i := 0; i < writes; i++ {
				if n, err := rot.Write(line); err != nil || n != len(line) {
					t.Fatalf("write %d: n=%d err=%v", i, n, err)
				}
			}
			if err := rot.Close(); err != nil {
				t.Fatalf("Close: %v", err)
			}

			// The active file plus exactly `generations` backups exist; the
			// generation past the cap was dropped.
			if _, err := os.Stat(path); err != nil {
				t.Errorf("active daemon log missing after rotation: %v", err)
			}
			for g := 1; g <= generations; g++ {
				if _, err := os.Stat(fmt.Sprintf("%s.%d", path, g)); err != nil {
					t.Errorf("generation %s.%d missing: %v", path, g, err)
				}
			}
			if _, err := os.Stat(fmt.Sprintf("%s.%d", path, generations+1)); !os.IsNotExist(err) {
				t.Errorf("generation %s.%d present but must have been dropped (stat err=%v)",
					path, generations+1, err)
			}

			// No file ever grows unbounded: the active file never exceeds the size
			// threshold (rotation happens before a write would cross it).
			info, err := os.Stat(path)
			if err == nil && info.Size() > maxBytes {
				t.Errorf("active log size = %d, exceeds threshold %d (rotation is not size-bounded)", info.Size(), maxBytes)
			}
		})

		t.Run("production constants are 10MB and 5 generations", func(t *testing.T) {
			if daemon.DaemonLogMaxBytes != 10*1024*1024 {
				t.Errorf("DaemonLogMaxBytes = %d, want 10485760 (10 MB)", daemon.DaemonLogMaxBytes)
			}
			if daemon.DaemonLogGenerations != 5 {
				t.Errorf("DaemonLogGenerations = %d, want 5", daemon.DaemonLogGenerations)
			}
		})

		t.Run("rotation is size-based by design: no time-based logic in the rotator", func(t *testing.T) {
			// The doctrine forbids time-based rotation ever; assert it structurally
			// -- the rotator source carries no time.Ticker / time.Timer / time.After
			// and does not import the time package, so rotation can only be triggered
			// by size.
			src, err := os.ReadFile("rotate.go")
			if err != nil {
				t.Fatalf("read rotate.go: %v", err)
			}
			for _, banned := range []string{"time.Ticker", "time.Timer", "time.After", "time.NewTicker", "time.NewTimer", "\"time\""} {
				if strings.Contains(string(src), banned) {
					t.Errorf("rotate.go references %q; the rotator must have no time-based rotation", banned)
				}
			}
		})

		t.Run("concurrent writes are safe", func(t *testing.T) {
			dir := t.TempDir()
			path := filepath.Join(dir, "daemon.log")
			rot, err := daemon.NewSizeRotator(path, 200, 4)
			if err != nil {
				t.Fatalf("NewSizeRotator: %v", err)
			}
			defer func() { _ = rot.Close() }()

			const goroutines, perG = 8, 200
			var wg sync.WaitGroup
			errs := make(chan error, goroutines)
			for g := 0; g < goroutines; g++ {
				wg.Add(1)
				go func(id int) {
					defer wg.Done()
					msg := []byte(fmt.Sprintf("writer-%d-line\n", id))
					for i := 0; i < perG; i++ {
						if _, err := rot.Write(msg); err != nil {
							errs <- err
							return
						}
					}
				}(g)
			}
			wg.Wait()
			close(errs)
			for err := range errs {
				t.Errorf("concurrent write failed: %v", err)
			}
		})
	})
}
