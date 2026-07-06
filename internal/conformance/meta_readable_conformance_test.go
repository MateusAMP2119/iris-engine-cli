//go:build conformance

package conformance

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/MateusAMP2119/iris-engine-cli/internal/daemon"
	"github.com/MateusAMP2119/iris-engine-cli/internal/store"
)

// healthzRole issues GET /healthz over the daemon's socket and returns the reported
// leadership role, so a test can wait for the daemon to become leader before it
// asserts against meta.
func healthzRole(t *testing.T, socket string) string {
	t.Helper()
	resp, err := HTTPOverSocket(socket).Get("http://iris/healthz")
	if err != nil {
		return ""
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		return ""
	}
	var env struct {
		Data struct {
			Role string `json:"role"`
		} `json:"data"`
	}
	if err := json.Unmarshal(body, &env); err != nil {
		return ""
	}
	return env.Data.Role
}

// waitForLeader polls healthz until the daemon reports the leader role or the
// deadline passes. Readiness is a condition (leadership confirmed), never elapsed
// time; the poll interval only keeps the loop from spinning.
func waitForLeader(t *testing.T, socket string) bool {
	t.Helper()
	deadline := time.Now().Add(30 * time.Second)
	for time.Now().Before(deadline) {
		if healthzRole(t, socket) == "leader" {
			return true
		}
		time.Sleep(100 * time.Millisecond)
	}
	return false
}

// metaDSN returns a connection string an independent Postgres client can use to read
// the meta database of the running daemon, plus whether the mode is managed. In
// external mode it points IRIS_PG_DSN at the meta database; in managed mode it
// reconstructs the local managed-Postgres DSN from the port the running instance
// records in postmaster.pid and the engine-minted superuser credential the daemon
// persisted.
func metaDSN(t *testing.T, ws string) string {
	t.Helper()
	if ext := os.Getenv("IRIS_PG_DSN"); ext != "" {
		cfg, err := pgx.ParseConfig(ext)
		if err != nil {
			t.Fatalf("parse IRIS_PG_DSN: %v", err)
		}
		cfg.Database = store.MetaDatabase
		return pgxConnString(cfg)
	}
	// Managed mode: read the running instance's port and the persisted superuser
	// password, and build a localhost DSN to meta.
	pgDir := filepath.Join(ws, ".iris", "pg")
	port := readPostmasterPort(t, filepath.Join(pgDir, "data", "postmaster.pid"))
	pwBytes, err := os.ReadFile(filepath.Join(pgDir, "superuser.pw")) //nolint:gosec // G304: engine-owned managed credential under the test's own workspace.
	if err != nil {
		t.Fatalf("read managed superuser credential: %v", err)
	}
	pw := strings.TrimSpace(string(pwBytes))
	return fmt.Sprintf("postgres://%s:%s@localhost:%s/%s?sslmode=disable",
		daemon.ManagedSuperuser, pw, port, store.MetaDatabase)
}

// pgxConnString reconstructs a plain DSN from a parsed pgx config (host, port,
// user, password, database), enough for an independent client to reach meta.
func pgxConnString(cfg *pgx.ConnConfig) string {
	return fmt.Sprintf("postgres://%s:%s@%s:%d/%s?sslmode=disable",
		cfg.User, cfg.Password, cfg.Host, cfg.Port, cfg.Database)
}

// readPostmasterPort returns the TCP port a running Postgres records on line 4 of
// its postmaster.pid.
func readPostmasterPort(t *testing.T, pidPath string) string {
	t.Helper()
	raw, err := os.ReadFile(pidPath) //nolint:gosec // G304: managed-Postgres pid file under the test's own workspace.
	if err != nil {
		t.Fatalf("read postmaster.pid: %v", err)
	}
	lines := strings.Split(strings.TrimSpace(string(raw)), "\n")
	if len(lines) < 4 {
		t.Fatalf("postmaster.pid has %d lines, want at least 4 (port on line 4)", len(lines))
	}
	return strings.TrimSpace(lines[3])
}

// TestMetaReadableWhileRunning proves any Postgres client can read the meta database
// read-only while the daemon runs, unblocked (specification section 4): the
// leader-election advisory lock guards leadership, not rows, so an independent MVCC
// client reads the control tables without contending with the leader's held lock or
// its single-writer session. It drives the real binary end to end -- engine install,
// engine start -d, an independent pgx client reading meta while the daemon holds the
// lock, engine stop.
//
// The journal read the contract also names lands with the journal DDL (E06); E02.6
// creates only the meta database, so the meta-readability half -- the property the
// advisory lock could threaten -- is what this leg proves.
//
// spec: S04/meta-readable-while-running
func TestMetaReadableWhileRunning(t *testing.T) {
	t.Run("S04/meta-readable-while-running", func(t *testing.T) {
		bin := Build(t)
		ws := shortWorkspace(t)
		socket := filepath.Join(ws, ".iris", "iris.sock")

		// Install (external: no-op under IRIS_PG_DSN; managed: cached download), then
		// start the daemon detached so it connects to a real Postgres and elects.
		bin.Run(t, RunOptions{Args: []string{"engine", "install"}, Dir: ws, Timeout: 5 * time.Minute}).RequireExit(t, 0)
		bin.Run(t, RunOptions{Args: []string{"engine", "start", "-d"}, Dir: ws, Timeout: 2 * time.Minute}).RequireExit(t, 0)
		t.Cleanup(func() {
			bin.Run(t, RunOptions{Args: []string{"engine", "stop"}, Dir: ws, Timeout: 30 * time.Second})
		})

		readyCtx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
		if err := WaitForSocket(readyCtx, socket); err != nil {
			cancel()
			t.Fatalf("daemon socket never became ready: %v", err)
		}
		cancel()

		// Wait until the daemon is the confirmed leader: it now holds the advisory lock
		// on its session-pinned connection and has ensured the meta schema.
		if !waitForLeader(t, socket) {
			t.Fatal("daemon never became leader; cannot assert meta is readable under a held lock")
		}

		// An independent Postgres client -- not the daemon -- reads meta while the
		// daemon holds the leader lock. The read must complete promptly (unblocked):
		// the advisory lock never blocks MVCC reads.
		dsn := metaDSN(t, ws)
		readCtx, cancelRead := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancelRead()

		conn, err := pgx.Connect(readCtx, dsn)
		if err != nil {
			t.Fatalf("independent client could not connect to meta while the daemon runs: %v", err)
		}
		defer func() { _ = conn.Close(context.Background()) }()

		// Read several control tables read-only. They exist because the leader ensured
		// the schema at election; the reads return without blocking on the leader.
		for _, table := range []string{"runs", "pipelines", "dead_letters", "artifacts"} {
			var n int
			if err := conn.QueryRow(readCtx, "SELECT count(*) FROM "+table).Scan(&n); err != nil {
				t.Errorf("independent read of meta.%s while the daemon runs: %v", table, err)
			}
		}

		// The read session is still alive and unblocked after reading: reads never
		// contended with the leader's lock or its single-writer session.
		if err := conn.Ping(readCtx); err != nil {
			t.Errorf("independent meta reader was blocked/unhealthy under the running daemon: %v", err)
		}
	})
}
