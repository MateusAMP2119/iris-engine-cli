//go:build conformance

package conformance

import (
	"context"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

// TestSignalGracefulShutdown drives the real iris binary and proves the daemon
// shuts down gracefully on SIGTERM and SIGINT (specification section 2): the
// running daemon stops dispatching, drains its listeners and managed Postgres,
// flushes state, and exits cleanly -- releasing its socket and pidfile and
// recording a shutdown line in daemon.log. No runs exist yet, so the drained path
// is the listener + dispatcher + managed-PG stop; the assertion is a clean exit
// with the socket and pidfile gone. Both signals are exercised.
func TestSignalGracefulShutdown(t *testing.T) {
	bin := Build(t)

	// spec: S02/signal-graceful-shutdown
	t.Run("S02/signal-graceful-shutdown", func(t *testing.T) {
		signals := []struct {
			name string
			sig  syscall.Signal
		}{
			{"SIGTERM", syscall.SIGTERM},
			{"SIGINT", syscall.SIGINT},
		}
		for _, tc := range signals {
			t.Run(tc.name, func(t *testing.T) {
				ws := shortWorkspace(t)
				socket := filepath.Join(ws, ".iris", "iris.sock")
				pidPath := filepath.Join(ws, ".iris", "iris.pid")
				logPath := filepath.Join(ws, ".iris", "logs", "daemon.log")

				bin.Run(t, RunOptions{Args: []string{"engine", "install"}, Dir: ws, Timeout: 5 * time.Minute}).RequireExit(t, 0)
				bin.Run(t, RunOptions{Args: []string{"engine", "start", "--detach"}, Dir: ws, Timeout: 2 * time.Minute}).RequireExit(t, 0)

				readyCtx, cancelReady := context.WithTimeout(context.Background(), 20*time.Second)
				if err := WaitForSocket(readyCtx, socket); err != nil {
					cancelReady()
					t.Fatalf("daemon socket not reachable before signalling: %v", err)
				}
				cancelReady()

				pid := readDaemonPID(t, pidPath)

				// Signal the daemon directly (not via engine stop): the daemon's own
				// signal handler must drive the graceful shutdown.
				if err := syscall.Kill(pid, tc.sig); err != nil {
					t.Fatalf("kill %d with %s: %v", pid, tc.name, err)
				}

				// It exits on its own within the grace window.
				if err := waitPIDGone(pid, 15*time.Second); err != nil {
					t.Fatalf("daemon did not exit cleanly on %s: %v", tc.name, err)
				}

				// The socket and pidfile are released on a clean shutdown.
				goneCtx, cancelGone := context.WithTimeout(context.Background(), 10*time.Second)
				if err := waitSocketGone(goneCtx, socket); err != nil {
					t.Errorf("socket still reachable after %s: %v", tc.name, err)
				}
				cancelGone()
				if _, err := os.Stat(pidPath); !os.IsNotExist(err) {
					t.Errorf("pidfile survived %s (stat err=%v)", tc.name, err)
				}

				// The shutdown is recorded in the structured daemon log.
				body, err := os.ReadFile(logPath) //nolint:gosec // G304: logPath is the engine-owned daemon.log under the test's throwaway workspace, never user or network input.
				if err != nil {
					t.Fatalf("read daemon.log: %v", err)
				}
				if !strings.Contains(string(body), "shut down") {
					t.Errorf("daemon.log has no shutdown line after %s:\n%s", tc.name, body)
				}
			})
		}
	})
}

// readDaemonPID reads and parses the pid a detached daemon recorded at pidPath.
func readDaemonPID(t *testing.T, pidPath string) int {
	t.Helper()
	raw, err := os.ReadFile(pidPath) //nolint:gosec // G304: pidPath is the engine-owned pidfile under the test's throwaway workspace, never user or network input.
	if err != nil {
		t.Fatalf("read pidfile %s: %v", pidPath, err)
	}
	pid, err := strconv.Atoi(strings.TrimSpace(string(raw)))
	if err != nil {
		t.Fatalf("pidfile %s does not hold a pid: %v", pidPath, err)
	}
	return pid
}

// waitPIDGone polls until the process with pid is no longer alive or the deadline
// passes, so a test can confirm a signalled daemon has actually exited. Liveness
// is probed with the null signal; the brief backoff between probes only keeps the
// loop from spinning and never stands in for a readiness signal.
func waitPIDGone(pid int, within time.Duration) error {
	deadline := time.Now().Add(within)
	for {
		if err := syscall.Kill(pid, 0); err != nil {
			return nil // the process is gone
		}
		if time.Now().After(deadline) {
			return context.DeadlineExceeded
		}
		time.Sleep(50 * time.Millisecond)
	}
}
