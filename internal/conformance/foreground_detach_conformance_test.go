//go:build conformance

package conformance

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"syscall"
	"testing"
	"time"
)

// shortWorkspace returns a fresh workspace directory with a short path, so the
// daemon's <ws>/.iris/iris.sock stays under the platform's tight sockaddr_un
// limit (t.TempDir embeds the long test name and can overflow it).
func shortWorkspace(t *testing.T) string {
	t.Helper()
	dir, err := os.MkdirTemp("", "iris")
	if err != nil {
		t.Fatalf("temp workspace: %v", err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	return dir
}

// waitSocketGone polls until the unix socket at path no longer accepts a
// connection or ctx is done, so a test can confirm a stopped daemon has released
// its socket.
func waitSocketGone(ctx context.Context, path string) error {
	var d net.Dialer
	for {
		conn, err := d.DialContext(ctx, "unix", path)
		if err != nil {
			return nil
		}
		_ = conn.Close()
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(50 * time.Millisecond):
		}
	}
}

// requireHealthzOK issues GET /healthz over the daemon's socket and fails unless
// it answers 200 with the ok liveness envelope.
func requireHealthzOK(t *testing.T, socket string) {
	t.Helper()
	resp, err := HTTPOverSocket(socket).Get("http://iris/healthz")
	if err != nil {
		t.Fatalf("GET /healthz over %s: %v", socket, err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET /healthz: status = %d, want 200 (body %s)", resp.StatusCode, body)
	}
	var env struct {
		Data struct {
			Status string `json:"status"`
		} `json:"data"`
	}
	if err := json.Unmarshal(body, &env); err != nil {
		t.Fatalf("healthz body is not a JSON envelope: %v (%s)", err, body)
	}
	if env.Data.Status != "ok" {
		t.Errorf("healthz status = %q, want ok", env.Data.Status)
	}
}

// TestForegroundDefaultDetach drives the real iris binary and proves the
// foreground/detach lifecycle: `iris engine start`
// runs the daemon attached in the foreground by default (it blocks, serving the
// socket, until signalled), and `-d` detaches so the daemon survives the CLI's
// exit and is stopped by `iris engine stop`. It also proves the managed fail-fast:
// `engine start` with no installed managed Postgres exits fast with install
// guidance.
//
// The install leg is cheap here: in the conformance job an external pg_dsn makes
// install a no-op, and locally the managed Postgres download is cached from the
// E02.3 runs.
func TestForegroundDefaultDetach(t *testing.T) {
	bin := Build(t)

	t.Run("foreground-default-detach", func(t *testing.T) {
		t.Run("managed with no install fails fast", func(t *testing.T) {
			// Force managed mode regardless of a shared-Postgres CI env: an empty
			// IRIS_PG_DSN reads as unset, selecting the managed path with nothing
			// installed, so start must fail fast with install guidance.
			t.Setenv("IRIS_PG_DSN", "")
			ws := t.TempDir()
			res := bin.Run(t, RunOptions{
				Args:    []string{"engine", "start"},
				Dir:     ws,
				Timeout: 30 * time.Second,
			})
			res.RequireExit(t, 4)
			if !bytes.Contains(res.Stderr, []byte("engine install")) {
				t.Errorf("managed no-install start: missing install guidance on stderr:\n%s", res.Stderr)
			}
		})

		t.Run("attached by default, then detach survives CLI exit", func(t *testing.T) {
			ws := shortWorkspace(t)
			socket := filepath.Join(ws, ".iris", "iris.sock")

			// Install first so managed mode has a Postgres (no-op under an external
			// pg_dsn); this makes `engine start` proceed past the install guard.
			bin.Run(t, RunOptions{Args: []string{"engine", "install"}, Dir: ws, Timeout: 5 * time.Minute}).RequireExit(t, 0)

			// Foreground (default): the CLI stays attached, blocking while it serves
			// the socket, until SIGTERM brings it down cleanly.
			cmd := exec.Command(bin.Path(), "engine", "start")
			cmd.Dir = ws
			var stderr bytes.Buffer
			cmd.Stderr = &stderr
			if err := cmd.Start(); err != nil {
				t.Fatalf("start foreground daemon: %v", err)
			}

			readyCtx, cancelReady := context.WithTimeout(context.Background(), 20*time.Second)
			if err := WaitForSocket(readyCtx, socket); err != nil {
				cancelReady()
				_ = cmd.Process.Kill()
				t.Fatalf("foreground daemon socket never became ready: %v\nstderr:\n%s", err, stderr.String())
			}
			cancelReady()
			// Attached: the CLI process is still running (it blocked, did not return).
			if err := cmd.Process.Signal(syscall.Signal(0)); err != nil {
				t.Errorf("foreground daemon exited instead of staying attached: %v\nstderr:\n%s", err, stderr.String())
			}
			requireHealthzOK(t, socket)

			// SIGTERM stops the attached daemon; it exits cleanly (graceful shutdown).
			_ = cmd.Process.Signal(syscall.SIGTERM)
			waitErr := make(chan error, 1)
			go func() { waitErr <- cmd.Wait() }()
			select {
			case err := <-waitErr:
				if err != nil {
					t.Errorf("foreground daemon did not exit cleanly on SIGTERM: %v\nstderr:\n%s", err, stderr.String())
				}
			case <-time.After(15 * time.Second):
				_ = cmd.Process.Kill()
				t.Fatalf("foreground daemon did not stop within the grace period on SIGTERM\nstderr:\n%s", stderr.String())
			}
			goneCtx, cancelGone := context.WithTimeout(context.Background(), 10*time.Second)
			if err := waitSocketGone(goneCtx, socket); err != nil {
				t.Errorf("socket still reachable after the foreground daemon exited: %v", err)
			}
			cancelGone()

			// Detach (-d): the CLI returns 0 promptly, the daemon keeps running in the
			// background, and its socket is reachable after the CLI has exited.
			res := bin.Run(t, RunOptions{Args: []string{"engine", "start", "-d"}, Dir: ws, Timeout: 2 * time.Minute})
			res.RequireExit(t, 0)
			readyCtx2, cancelReady2 := context.WithTimeout(context.Background(), 20*time.Second)
			if err := WaitForSocket(readyCtx2, socket); err != nil {
				cancelReady2()
				t.Fatalf("detached daemon socket not reachable after the CLI exited: %v", err)
			}
			cancelReady2()
			requireHealthzOK(t, socket)

			// engine stop stops the detached daemon and releases the socket.
			bin.Run(t, RunOptions{Args: []string{"engine", "stop"}, Dir: ws, Timeout: 30 * time.Second}).RequireExit(t, 0)
			goneCtx2, cancelGone2 := context.WithTimeout(context.Background(), 15*time.Second)
			if err := waitSocketGone(goneCtx2, socket); err != nil {
				t.Errorf("socket still reachable after engine stop: %v", err)
			}
			cancelGone2()
		})
	})
}
