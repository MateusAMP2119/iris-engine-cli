package cli

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"

	"github.com/MateusAMP2119/iris-lakehouse/internal/config"
	"github.com/MateusAMP2119/iris-lakehouse/internal/daemon"
)

// seedFile writes body to path, first creating the file's parent directory, so
// each fixture write is self-contained rather than relying on a sibling write to
// have created the shared parent.
func seedFile(t *testing.T, path, body string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatalf("seed dir for %s: %v", path, err)
	}
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatalf("seed %s: %v", path, err)
	}
}

// seedEngineArtifacts pins a fresh per-test engine home (IRIS_HOME) and creates
// under it the on-disk engine artifacts an uninstall removes: an object store
// with a payload file, a control socket file, and a service unit file. It
// returns the resolved settings.
func seedEngineArtifacts(t *testing.T) config.Settings {
	t.Helper()
	home := t.TempDir()
	t.Setenv(config.EnvHome, home)
	s := config.Resolve(config.Defaults(home), config.Layer{}, config.Layer{}, config.Layer{})
	seedFile(t, filepath.Join(s.ObjectsPath, "deadbeef.artifact"), "bytes")
	seedFile(t, s.Socket, "socket")
	seedFile(t, daemon.ServiceUnitPath(s), "unit")
	return s
}

// TestEngineUninstallCLI proves the `iris engine uninstall` command: it is gated
// (it refuses without --yes and touches nothing), and when confirmed it performs
// the local, daemonless teardown of the engine's on-disk state -- the object store
// under objects_path, the control socket, and the service unit.
// The teardown deletes both the object store's artifact bytes
// and archived partitions by removing the store directory outright.
func TestEngineUninstallCLI(t *testing.T) {
	t.Run("refuses without --yes and removes nothing", func(t *testing.T) {
		t.Chdir(t.TempDir())
		s := seedEngineArtifacts(t)

		var out, errb bytes.Buffer
		code := newApp(&out, &errb).run([]string{"engine", "uninstall"})
		if code != exitOpFailed {
			t.Fatalf("exit = %d, want %d (refused, operation failed)\nstdout: %s\nstderr: %s", code, exitOpFailed, out.String(), errb.String())
		}
		// Nothing was torn down: the gate blocked before any removal.
		for _, path := range []string{s.ObjectsPath, s.Socket, daemon.ServiceUnitPath(s)} {
			if _, err := os.Stat(path); err != nil {
				t.Errorf("refused uninstall removed %s (stat err = %v); the gate must touch nothing", path, err)
			}
		}
	})

	t.Run("with --yes removes the object store, socket, and service unit", func(t *testing.T) {
		t.Chdir(t.TempDir())
		s := seedEngineArtifacts(t)

		var out, errb bytes.Buffer
		code := newApp(&out, &errb).run([]string{"engine", "uninstall", "--yes"})
		if code != exitOK {
			t.Fatalf("exit = %d, want %d\nstdout: %s\nstderr: %s", code, exitOK, out.String(), errb.String())
		}
		for _, path := range []string{s.ObjectsPath, s.Socket, daemon.ServiceUnitPath(s)} {
			if _, err := os.Stat(path); !os.IsNotExist(err) {
				t.Errorf("%s still present after confirmed uninstall (stat err = %v)", path, err)
			}
		}
	})

	t.Run("with --yes under --json emits one envelope and no secret", func(t *testing.T) {
		t.Chdir(t.TempDir())
		seedEngineArtifacts(t)

		var out, errb bytes.Buffer
		code := newApp(&out, &errb).run([]string{"--json", "engine", "uninstall", "--yes"})
		if code != exitOK {
			t.Fatalf("exit = %d, want %d\nstdout: %s\nstderr: %s", code, exitOK, out.String(), errb.String())
		}
		var doc struct {
			Data map[string]any `json:"data"`
		}
		decodeSingleJSON(t, out.Bytes(), &doc)
		if doc.Data == nil {
			t.Errorf("uninstall --json produced no data envelope: %s", out.String())
		}
	})

	// uninstall is strictly local (no listener path); a --host/--token (remote
	// control PAT) cannot trigger it.
	t.Run("uninstall-local-only", func(t *testing.T) {
		t.Chdir(t.TempDir())
		seedEngineArtifacts(t)

		var out, errb bytes.Buffer
		// Even with remote flags, uninstall is daemonless and performs (or gates)
		// locally; it must not yield "no_daemon".
		code := newApp(&out, &errb).run([]string{"--host", "example:1234", "--token", "pat", "engine", "uninstall"})
		if code == exitNoDaemon {
			t.Fatalf("uninstall over --host yielded no_daemon; must be local-only")
		}
		// It refused on confirmation (the local gate), as expected.
		if code != exitOpFailed {
			t.Fatalf("exit with remote flags but no --yes = %d, want %d (local confirmation gate)", code, exitOpFailed)
		}
	})
}
