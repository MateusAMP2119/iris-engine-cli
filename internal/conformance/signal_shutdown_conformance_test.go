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
// shuts down gracefully on SIGTERM and SIGINT: the
// running daemon stops dispatching, drains its listeners and managed Postgres,
// flushes state, and exits cleanly -- releasing its socket and pidfile and
// recording a shutdown line in daemon.log. No runs exist yet, so the drained path
// is the listener + dispatcher + managed-PG stop; the assertion is a clean exit
// with the socket and pidfile gone. Both signals are exercised.
func TestSignalGracefulShutdown(t *testing.T) {
	bin := Build(t)

	t.Run("signal-graceful-shutdown", func(t *testing.T) {
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

				// The socket and pidfile are released as the daemon shuts down --
				// asynchronously: the process can be gone (waitPIDGone above) a hair
				// before it has finished releasing either artifact, so poll each with a
				// generous deadline and fail only once it is truly overdue, never on the
				// instant after the signal is sent. Independent deadlines keep a lingering
				// socket from starving the pidfile check into a misleading cascade.
				socketCtx, cancelSocket := context.WithTimeout(context.Background(), 10*time.Second)
				if err := waitSocketGone(socketCtx, socket); err != nil {
					t.Errorf("socket still reachable after %s: %v", tc.name, err)
				}
				cancelSocket()

				pidGoneCtx, cancelPIDGone := context.WithTimeout(context.Background(), 10*time.Second)
				if err := waitFileGone(pidGoneCtx, pidPath); err != nil {
					t.Errorf("pidfile survived %s: %v", tc.name, err)
				}
				cancelPIDGone()

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

// waitFileGone polls until the file at path no longer exists or ctx is done, so a
// test can confirm a stopped daemon has removed an artifact -- its pidfile --
// whose removal rides the asynchronous shutdown and is not synchronous with
// process exit. It returns nil once the file is gone, ctx.Err() when the deadline
// passes with the file still present, and any stat error other than not-exist.
// The brief backoff between probes only keeps the loop from spinning and never
// stands in for a readiness signal.
func waitFileGone(ctx context.Context, path string) error {
	for {
		if _, err := os.Stat(path); err != nil {
			if os.IsNotExist(err) {
				return nil
			}
			return err
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(50 * time.Millisecond):
		}
	}
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
