//go:build conformance

package conformance

import (
	"context"
	"encoding/json"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// replayLeaderStub stands up a leader endpoint over a unix socket that answers POST
// /deadletter/replay with the given status and JSON body, so the shipped binary's
// replay client is driven end to end (resolve target, POST scope, classify reply)
// against a real socket and real HTTP. The daemon's leader-side replay mints each root
// cause a replacement for real, but a minted replacement is QUEUED rather than executed
// inline, so a live replay never reports a re-dead-letter in its own reply: its
// dead_lettered list is always empty, and a replacement that later fails parks a fresh
// worklist entry through the lane loop instead. This stub is what hands the binary a
// dead-lettering-replay reply, so the conformance leg can prove the SHIPPED BINARY's
// exit-5 contract. The dispatch-internal correctness (root walk, atomic mint + worklist
// exit, replayed_from chaining) is proven at the unit and integration tiers.
func replayLeaderStub(t *testing.T, socket string, status int, body any) {
	t.Helper()
	ln, err := net.Listen("unix", socket)
	if err != nil {
		t.Fatalf("conformance: listen unix %s: %v", socket, err)
	}
	mux := http.NewServeMux()
	mux.HandleFunc("/deadletter/replay", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			w.WriteHeader(http.StatusMethodNotAllowed)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_ = json.NewEncoder(w).Encode(body)
	})
	srv := &http.Server{Handler: mux, ReadHeaderTimeout: 5 * time.Second}
	go func() { _ = srv.Serve(ln) }()
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		_ = srv.Shutdown(ctx)
	})
}

// shortSocketPath returns a unix-socket path under a fresh short temp dir, kept short
// so it stays under the platform sockaddr_un limit (t.TempDir paths can be too long).
func shortSocketPath(t *testing.T) string {
	t.Helper()
	dir, err := os.MkdirTemp("", "iris")
	if err != nil {
		t.Fatalf("conformance: temp dir: %v", err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	return filepath.Join(dir, "iris.sock")
}

// TestDeadletterReplayDeadLetterExit5 drives the real iris binary and proves the
// exit-5 contract for `iris deadletter replay`: it exits 5 when a replayed
// run dead-letters again. It also confirms the
// reachable neighbors of that path are real in the shipped binary: a clean replay
// exits 0 and a bare (unscoped) replay is a usage error (exit 2), so exit 5 is a
// distinct, deliberate category, not a catch-all.
func TestDeadletterReplayDeadLetterExit5(t *testing.T) {
	bin := Build(t)

	t.Run("deadletter-replay-deadletter-exit5", func(t *testing.T) {
		start := time.Now()

		// A leader reports that replaying run 10 minted run 40, which dead-lettered
		// again (chained to the original via replayed_from).
		sock := shortSocketPath(t)
		replayLeaderStub(t, sock, http.StatusOK, map[string]any{
			"data": map[string]any{
				"replayed": []any{},
				"dead_lettered": []any{
					map[string]any{"replaced_run": "10", "replacement_run": "40", "replayed_from": "10"},
				},
			},
		})

		res := bin.Run(t, RunOptions{Args: []string{"--socket", sock, "deadletter", "replay", "10"}})
		res.RequireExit(t, 5)
		if !strings.Contains(string(res.Stderr), "40") {
			t.Errorf("exit-5 message does not name the re-dead-lettered replacement run:\n%s", res.Stderr)
		}
		t.Logf("deadletter replay exit-5 leg runtime: %s", time.Since(start))
	})

	// A clean replay (no re-dead-letter) exits 0: exit 5 is reserved for the
	// dead-lettering case.
	t.Run("clean replay exits 0", func(t *testing.T) {
		sock := shortSocketPath(t)
		replayLeaderStub(t, sock, http.StatusOK, map[string]any{
			"data": map[string]any{
				"replayed": []any{
					map[string]any{"replaced_run": "10", "replacement_run": "40", "replayed_from": "10"},
				},
				"dead_lettered": []any{},
			},
		})
		bin.Run(t, RunOptions{Args: []string{"--socket", sock, "deadletter", "replay", "--all"}}).RequireExit(t, 0)
	})

	// A bare replay names no scope: usage error (exit 2), nothing defaults to
	// everything -- so exit 5 is never reached by an unscoped invocation.
	t.Run("bare replay is a usage error (exit 2)", func(t *testing.T) {
		bin.Run(t, RunOptions{Args: []string{"deadletter", "replay"}}).RequireExit(t, 2)
	})
}
