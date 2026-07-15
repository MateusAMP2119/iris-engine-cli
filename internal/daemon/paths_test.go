package daemon

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/MateusAMP2119/iris-lakehouse/internal/config"
)

// TestIrisDirDefaultPaths proves the engine-home path defaults: the unix socket
// defaults to <engine home>/iris.sock, the optional iris.toml sits at
// <engine home>/iris.toml and carries engine/connection settings only, and the
// daemon log plus per-run logs live under <engine home>/logs as daemon.log and
// run-<id>.log.
func TestIrisDirDefaultPaths(t *testing.T) {
	t.Run("iris-dir-default-paths", func(t *testing.T) {
		home := t.TempDir()
		settings := config.Resolve(config.Defaults(home), config.Layer{}, config.Layer{}, config.Layer{})

		// The socket defaults to <engine home>/iris.sock.
		wantSock := filepath.Join(home, "iris.sock")
		if settings.Socket != wantSock {
			t.Errorf("default socket = %q, want %q", settings.Socket, wantSock)
		}

		// The optional iris.toml sits under the engine home and carries
		// engine/connection settings only: a recognized engine key is honored, a
		// project-manifest key is ignored (never a project manifest).
		wantTOML := filepath.Join(home, "iris.toml")
		if got := filepath.Join(home, config.FileName); got != wantTOML {
			t.Errorf("iris.toml path = %q, want %q", got, wantTOML)
		}
		parsed, err := config.ParseTOML([]byte("tcp = \"127.0.0.1:7100\"\nname = \"my-project\"\n"))
		if err != nil {
			t.Fatalf("parse iris.toml: %v", err)
		}
		if parsed.Layer.TCP == nil || *parsed.Layer.TCP != "127.0.0.1:7100" {
			t.Errorf("engine setting tcp not honored from iris.toml: %+v", parsed.Layer.TCP)
		}
		if len(parsed.Ignored) != 1 || parsed.Ignored[0] != "name" {
			t.Errorf("project key not ignored; ignored = %v, want [name]", parsed.Ignored)
		}

		// The daemon log lands at <engine home>/logs/daemon.log and is really
		// created under that tree.
		wantLog := filepath.Join(home, "logs", "daemon.log")
		if got := LogPath(settings); got != wantLog {
			t.Errorf("daemon log path = %q, want %q", got, wantLog)
		}
		f, err := OpenDaemonLog(settings)
		if err != nil {
			t.Fatalf("open daemon log: %v", err)
		}
		_ = f.Close()
		if _, err := os.Stat(wantLog); err != nil {
			t.Errorf("daemon.log not created under the engine home's logs: %v", err)
		}

		// Per-run logs follow the run-<id>.log convention under the same tree.
		wantRun := filepath.Join(home, "logs", "run-42.log")
		if got := RunLogPath(settings, "42"); got != wantRun {
			t.Errorf("per-run log path = %q, want %q", got, wantRun)
		}
	})
}

// TestCandidateRequiresWorkspaceTree proves the per-host prerequisite: a daemon
// candidate started on a host lacking the workspace tree the leader dispatches from
// (pipeline folders, dev source, env_files) refuses to start.
func TestCandidateRequiresWorkspaceTree(t *testing.T) {
	t.Run("candidate-requires-workspace-tree", func(t *testing.T) {
		missing := filepath.Join(t.TempDir(), "no-tree-here")
		err := requireWorkspaceTree(missing)
		if err == nil {
			t.Fatal("requireWorkspaceTree on missing path succeeded, want refusal error")
		}
		if !strings.Contains(err.Error(), "workspace tree") || !strings.Contains(err.Error(), "refuses to start") {
			t.Errorf("error %q does not mention workspace tree refusal", err)
		}

		// Existing dir is accepted (candidate may start; declarations may be absent until apply).
		good := t.TempDir()
		if err := requireWorkspaceTree(good); err != nil {
			t.Errorf("requireWorkspaceTree on existing dir: %v", err)
		}
	})
}
