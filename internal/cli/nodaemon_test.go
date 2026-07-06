package cli

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/MateusAMP2119/iris-engine-cli/internal/api"
	"github.com/MateusAMP2119/iris-engine-cli/internal/config"
	"github.com/MateusAMP2119/iris-engine-cli/internal/daemon"
)

// shortSocket returns a unix-socket path under a fresh short temp dir, kept short
// so it stays under the platform's sockaddr_un limit.
func shortSocket(t *testing.T) string {
	t.Helper()
	dir, err := os.MkdirTemp("", "iris")
	if err != nil {
		t.Fatalf("temp dir: %v", err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	return filepath.Join(dir, "iris.sock")
}

// TestNoDaemonFailFast proves a daemon-touching command with no reachable daemon
// fails fast and never auto-starts one (specification section 2): the command
// actually dials the resolved socket; a refused/absent socket is exit 3 with
// start guidance and no side effect (no socket created, no daemon spawned), while
// a reachable daemon lets the command past the reachability gate (it no longer
// reports no-daemon).
func TestNoDaemonFailFast(t *testing.T) {
	// spec: S02/no-daemon-fail-fast
	t.Run("S02/no-daemon-fail-fast", func(t *testing.T) {
		// Unreachable: point --socket at a path with nothing listening.
		sock := shortSocket(t)
		var out, errb bytes.Buffer
		code := newApp(&out, &errb).run([]string{"--socket", sock, "pipeline", "list"})
		if code != exitNoDaemon {
			t.Fatalf("unreachable daemon: exit = %d, want %d\nstdout: %s\nstderr: %s", code, exitNoDaemon, out.String(), errb.String())
		}
		if !strings.Contains(errb.String(), "engine start") {
			t.Errorf("no-daemon guidance to start the engine missing from stderr:\n%s", errb.String())
		}
		// Never auto-starts: the command created no socket and started nothing.
		if _, err := os.Stat(sock); !os.IsNotExist(err) {
			t.Errorf("a socket appeared at %s; the command auto-started a daemon (stat err = %v)", sock, err)
		}

		// Reachable: an in-process daemon on the resolved socket lets the command
		// past the no-daemon gate. It no longer reports exit 3.
		liveSock := shortSocket(t)
		srv := daemon.NewServer(config.Settings{Socket: liveSock}, api.NewMux())
		if err := srv.Start(context.Background()); err != nil {
			t.Fatalf("start in-process daemon: %v", err)
		}
		t.Cleanup(func() { _ = srv.Shutdown() })

		var out2, errb2 bytes.Buffer
		code2 := newApp(&out2, &errb2).run([]string{"--socket", liveSock, "pipeline", "list"})
		if code2 == exitNoDaemon {
			t.Fatalf("reachable daemon still reported no-daemon (exit %d)\nstdout: %s\nstderr: %s", code2, out2.String(), errb2.String())
		}
	})
}
