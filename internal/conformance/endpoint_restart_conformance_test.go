//go:build conformance

package conformance

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
)

// This file proves persisted read endpoints survive a daemon restart end to end
// against the shipped binary, a running daemon, and real Postgres (endpoints
// persist to meta; a restart or failover serves every applied
// endpoint with no re-apply). It also proves the shared read-pool login credential is
// stable across a restart -- the HA fix that stopped every start from minting a fresh
// secret and resetting the shared login's password. The restart is a genuinely new
// daemon process: `engine stop` (managed mode also stops the local Postgres), then
// `engine start` again, with NO `iris endpoint apply` in between.

// TestEndpointsSurviveRestart applies an endpoint, restarts the daemon without
// re-applying, and proves /q still serves the same data with the same data-PAT, and
// that the read-pool credential in meta did not change across the restart.
func TestEndpointsSurviveRestart(t *testing.T) {
	t.Run("endpoints-reload-on-restart", func(t *testing.T) {
		env := startOrdersEndpointEnv(t)
		token := mintEndpointPAT(t, env, "orders_by_customer")

		// Baseline: the endpoint serves before the restart.
		if code, e := env.tcpGet(t, "/q/orders_by_customer", token); code != http.StatusOK || e.Error != nil {
			t.Fatalf("pre-restart /q = %d %+v, want 200", code, e.Error)
		}

		// The persisted read-pool credential before the restart, read while the daemon
		// (and its Postgres) is up.
		credBefore := readPoolCredentialFromMeta(t, env.ws)
		if credBefore == "" {
			t.Fatal("no read-pool credential persisted in meta before the restart")
		}

		// Restart: a genuinely new daemon process, no re-apply of the endpoint.
		tcpAddr := strings.TrimPrefix(env.tcpBase, "http://")
		env.bin.Run(t, RunOptions{Args: []string{"engine", "stop"}, Dir: env.ws, Timeout: 30 * time.Second}).RequireExit(t, 0)
		waitForSocketGone(t, env.socket, 30*time.Second)
		env.bin.Run(t, RunOptions{Args: []string{"engine", "start", "-d", "--tcp", tcpAddr}, Dir: env.ws, Timeout: 2 * time.Minute}).RequireExit(t, 0)
		readyCtx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		if err := WaitForSocket(readyCtx, env.socket); err != nil {
			cancel()
			t.Fatalf("restarted daemon socket never became ready: %v", err)
		}
		cancel()
		if !waitForLeader(t, env.socket) {
			t.Fatal("restarted daemon never became leader")
		}

		// The endpoint serves after the restart WITHOUT any re-apply: the daemon
		// reloaded the persisted shape from meta into the live serving registry, and the
		// read pool authenticated with the SAME persisted credential.
		code, e := env.tcpGet(t, "/q/orders_by_customer", token)
		if code != http.StatusOK || e.Error != nil {
			t.Fatalf("post-restart /q = %d %+v, want 200 with no re-apply (endpoint reload)", code, e.Error)
		}
		if len(e.Data) != 3 {
			t.Fatalf("post-restart /q served %d rows, want the 3 seeded", len(e.Data))
		}

		// The read-pool credential is stable across the restart: a fresh start reused
		// the persisted secret rather than minting a new one and resetting the login.
		credAfter := readPoolCredentialFromMeta(t, env.ws)
		if credAfter != credBefore {
			t.Errorf("read-pool credential changed across restart; a restart must reuse the persisted secret (E13.7 HA fix)")
		}
	})
}

// readPoolCredentialFromMeta reads the single persisted read-pool secret from the
// engine-owned read_pool_credential meta table (id pinned to 1).
func readPoolCredentialFromMeta(t *testing.T, ws string) string {
	t.Helper()
	conn, err := pgx.Connect(context.Background(), metaDSN(t, ws))
	if err != nil {
		t.Fatalf("connect meta for the read-pool credential: %v", err)
	}
	defer conn.Close(context.Background())
	var secret string
	err = conn.QueryRow(context.Background(), `SELECT secret FROM read_pool_credential WHERE id = 1`).Scan(&secret)
	if errors.Is(err, pgx.ErrNoRows) {
		return ""
	}
	if err != nil {
		t.Fatalf("read read-pool credential from meta: %v", err)
	}
	return secret
}
