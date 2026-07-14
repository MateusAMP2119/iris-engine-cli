//go:build conformance

package conformance

import (
	"context"
	"path/filepath"
	"testing"
	"time"
)

// TestDaemonServiceReady drives the real iris binary and proves the detached
// daemon is service-ready: it runs without a controlling
// TTY and exits with sane exit codes, so a systemd/launchd unit can wrap
// `iris engine start --detach` directly. The daemon is started detached from a
// TTY-less environment (the go-test process has no controlling terminal and the
// detached child is itself a session leader via Setsid, so it holds no controlling
// TTY), served over its socket, and stopped -- every leg asserting a sane exit
// code.
//
// The install leg is cheap here: an external pg_dsn makes install a no-op in the
// conformance job, and locally the managed Postgres download is cached from the
// earlier E02 runs.
func TestDaemonServiceReady(t *testing.T) {
	bin := Build(t)

	t.Run("daemon-service-ready", func(t *testing.T) {
		ws := shortWorkspace(t)
		socket := filepath.Join(ws, ".iris", "iris.sock")

		// Install first so managed mode has a Postgres (no-op under an external
		// pg_dsn); a clean install exits 0.
		bin.Run(t, RunOptions{Args: []string{"engine", "install"}, Dir: ws, Timeout: 5 * time.Minute}).RequireExit(t, 0)

		// Detach from a session with no controlling TTY: the CLI's own stdio here are
		// pipes (never a terminal), and the detached child is Setsid, so the running
		// daemon has no controlling terminal. A clean detach exits 0.
		bin.Run(t, RunOptions{Args: []string{"engine", "start", "--detach"}, Dir: ws, Timeout: 2 * time.Minute}).RequireExit(t, 0)

		readyCtx, cancelReady := context.WithTimeout(context.Background(), 20*time.Second)
		if err := WaitForSocket(readyCtx, socket); err != nil {
			cancelReady()
			t.Fatalf("detached (TTY-free) daemon socket not reachable: %v", err)
		}
		cancelReady()

		// The TTY-free daemon serves the control plane.
		requireHealthzOK(t, socket)

		// Stop exits with a sane code (0) and releases the socket.
		bin.Run(t, RunOptions{Args: []string{"engine", "stop"}, Dir: ws, Timeout: 30 * time.Second}).RequireExit(t, 0)
		goneCtx, cancelGone := context.WithTimeout(context.Background(), 15*time.Second)
		if err := waitSocketGone(goneCtx, socket); err != nil {
			t.Errorf("socket still reachable after engine stop: %v", err)
		}
		cancelGone()
	})
}
