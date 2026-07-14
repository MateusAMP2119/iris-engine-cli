package cli

import (
	"bytes"
	"os"
	"os/exec"
	"strings"
	"testing"

	"github.com/MateusAMP2119/iris-lakehouse/internal/api"
	"github.com/MateusAMP2119/iris-lakehouse/internal/config"
	"github.com/MateusAMP2119/iris-lakehouse/internal/daemon"
)

// This file proves `iris engine uninstall`'s live-candidate guard bites: a
// confirmed uninstall refuses while a daemon is live -- reachable on the
// resolved socket, or recorded in a pidfile naming a running process -- and
// touches nothing, while a stale pidfile (process gone) never blocks.

func TestEngineUninstallRefusesLiveDaemon(t *testing.T) {
	clearTargetEnv(t)

	t.Run("engine-uninstall-live-guard", func(t *testing.T) {
		t.Run("a reachable daemon refuses the uninstall and removes nothing", func(t *testing.T) {
			t.Chdir(t.TempDir())
			s := seedEngineArtifacts(t)
			// A live in-process daemon serving /healthz on a probe-able socket. The
			// seeded socket file is replaced by the real listener.
			_ = os.Remove(s.Socket)
			sock := shortSocket(t)
			srv := daemon.NewServer(config.Settings{Socket: sock}, api.NewMux())
			startInProcess(t, srv)

			var out, errb bytes.Buffer
			code := newApp(&out, &errb).run([]string{"--socket", sock, "engine", "uninstall", "--yes"})
			if code != exitOpFailed {
				t.Fatalf("exit = %d, want %d (live daemon must refuse)\nstdout: %s\nstderr: %s", code, exitOpFailed, out.String(), errb.String())
			}
			combined := out.String() + errb.String()
			if !strings.Contains(combined, "engine stop") {
				t.Errorf("refusal should guide to `iris engine stop`: %s", combined)
			}
			// Nothing was torn down.
			if _, err := os.Stat(s.ObjectsPath); err != nil {
				t.Errorf("refused uninstall removed the object store (stat err = %v)", err)
			}
		})

		t.Run("a pidfile naming a running process refuses", func(t *testing.T) {
			t.Chdir(t.TempDir())
			s := seedEngineArtifacts(t)
			// A detached daemon's pidfile naming a live process (this test's own).
			if err := daemon.WritePIDFile(s, os.Getpid()); err != nil {
				t.Fatalf("write pidfile: %v", err)
			}

			var out, errb bytes.Buffer
			code := newApp(&out, &errb).run([]string{"engine", "uninstall", "--yes"})
			if code != exitOpFailed {
				t.Fatalf("exit = %d, want %d (live pidfile must refuse)\nstdout: %s\nstderr: %s", code, exitOpFailed, out.String(), errb.String())
			}
			if _, err := os.Stat(s.ObjectsPath); err != nil {
				t.Errorf("refused uninstall removed the object store (stat err = %v)", err)
			}
		})

		t.Run("a stale pidfile never blocks", func(t *testing.T) {
			t.Chdir(t.TempDir())
			s := seedEngineArtifacts(t)
			// A pidfile naming a process that has already exited.
			cmd := exec.Command("true")
			if err := cmd.Run(); err != nil {
				t.Fatalf("run short-lived process: %v", err)
			}
			if err := daemon.WritePIDFile(s, cmd.Process.Pid); err != nil {
				t.Fatalf("write stale pidfile: %v", err)
			}

			var out, errb bytes.Buffer
			code := newApp(&out, &errb).run([]string{"engine", "uninstall", "--yes"})
			if code != exitOK {
				t.Fatalf("exit = %d, want %d (a stale pidfile must not block)\nstdout: %s\nstderr: %s", code, exitOK, out.String(), errb.String())
			}
			if _, err := os.Stat(s.ObjectsPath); !os.IsNotExist(err) {
				t.Errorf("object store still present after uninstall past a stale pidfile (stat err = %v)", err)
			}
		})
	})
}
