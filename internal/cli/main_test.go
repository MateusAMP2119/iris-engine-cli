package cli

import (
	"os"
	"testing"

	"github.com/MateusAMP2119/iris-lakehouse/internal/config"
)

// TestMain pins the engine home to a throwaway directory for the whole package:
// production resolves the engine target from the fixed per-user ~/.iris
// (IRIS_HOME relocates it), and no CLI test may read a developer's real engine
// home -- a live iris.toml there would retarget every daemonless assertion.
// Tests that write engine-home state get per-test isolation on top via
// clearTargetEnv, which points IRIS_HOME at a fresh t.TempDir.
func TestMain(m *testing.M) {
	dir, err := os.MkdirTemp("", "iris-cli-test-home-*")
	if err == nil {
		_ = os.Setenv(config.EnvHome, dir)
	}
	code := m.Run()
	if err == nil {
		_ = os.RemoveAll(dir)
	}
	os.Exit(code)
}
