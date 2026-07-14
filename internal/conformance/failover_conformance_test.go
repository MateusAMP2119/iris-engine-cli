//go:build conformance

package conformance

import (
	"context"
	"os"
	"path/filepath"
	"syscall"
	"testing"
	"time"
)

// This file is the live-daemon failover leg (E11.4):
// two real daemon candidates share ONE meta (the external cluster's single meta
// database), so exactly one holds the leader advisory lock and the other blocks on
// it as a standby. Killing the leader process abruptly (a host-loss simulation, not
// a graceful stop) drops the leader's Postgres session, releasing the session-level
// advisory lock, and the blocked standby's pg_advisory_lock returns: it acquires,
// runs startup reconciliation, and becomes the dispatching leader. It needs a shared
// external Postgres (IRIS_PG_DSN); managed mode gives each daemon its OWN Postgres,
// so there is no shared lock to contend for.

// waitForRole polls a daemon's /healthz until it reports want or the deadline
// passes. Readiness is the reported role (a condition), never elapsed time; the poll
// interval only keeps the loop from spinning.
func waitForRole(t *testing.T, socket, want string) bool {
	t.Helper()
	deadline := time.Now().Add(30 * time.Second)
	for time.Now().Before(deadline) {
		if healthzRole(t, socket) == want {
			return true
		}
		time.Sleep(100 * time.Millisecond)
	}
	return false
}

// TestFailoverStandbyTakesOver drives two real daemons sharing one meta, kills the
// leader, and proves the standby takes over as the dispatching leader: one
// scenario, one real kill, both observable outcomes.
func TestFailoverStandbyTakesOver(t *testing.T) {
	if os.Getenv("IRIS_PG_DSN") == "" {
		t.Skip("failover needs two daemons sharing one external meta; set IRIS_PG_DSN (managed mode gives each daemon its own Postgres, so there is no shared advisory lock to contend for)")
	}
	freshDatabases(t)
	bin := Build(t)

	// Two workspaces: each daemon has its own socket, pidfile, objects_path, and
	// workspace tree -- distinct hosts sharing one meta. Distinct objects roots are
	// the leader-own-objects-path property the integration leg proves; the
	// takeover proves the standby leads at all.
	wsA := shortWorkspace(t)
	wsB := shortWorkspace(t)
	socketA := filepath.Join(wsA, ".iris", "iris.sock")
	socketB := filepath.Join(wsB, ".iris", "iris.sock")
	pidPathA := filepath.Join(wsA, ".iris", "iris.pid")

	// Install once against the shared external cluster (creates meta + data); the
	// second daemon shares those databases, so it does not install.
	bin.Run(t, RunOptions{Args: []string{"engine", "install"}, Dir: wsA, Timeout: 5 * time.Minute}).RequireExit(t, 0)

	// Daemon A starts first and wins the free advisory lock: it becomes the leader.
	bin.Run(t, RunOptions{Args: []string{"engine", "start", "-d"}, Dir: wsA, Timeout: 2 * time.Minute}).RequireExit(t, 0)
	stoppedA := false
	t.Cleanup(func() {
		if stoppedA {
			return
		}
		bin.Run(t, RunOptions{Args: []string{"engine", "stop"}, Dir: wsA, Timeout: 30 * time.Second})
	})

	readyA, cancelA := context.WithTimeout(context.Background(), 20*time.Second)
	if err := WaitForSocket(readyA, socketA); err != nil {
		cancelA()
		t.Fatalf("daemon A socket never became ready: %v", err)
	}
	cancelA()
	if !waitForLeader(t, socketA) {
		t.Fatal("daemon A never became leader; the first candidate must win the free lock")
	}

	// Daemon B starts second, contends for the held lock, and blocks: it is a standby
	// while A leads.
	bin.Run(t, RunOptions{Args: []string{"engine", "start", "-d"}, Dir: wsB, Timeout: 2 * time.Minute}).RequireExit(t, 0)
	t.Cleanup(func() {
		bin.Run(t, RunOptions{Args: []string{"engine", "stop"}, Dir: wsB, Timeout: 30 * time.Second})
	})

	readyB, cancelB := context.WithTimeout(context.Background(), 20*time.Second)
	if err := WaitForSocket(readyB, socketB); err != nil {
		cancelB()
		t.Fatalf("daemon B socket never became ready: %v", err)
	}
	cancelB()
	if !waitForRole(t, socketB, "standby") {
		t.Fatalf("daemon B never became standby while A held the lock (role=%q); two candidates must share one leader", healthzRole(t, socketB))
	}

	// Kill the leader abruptly: SIGKILL the process (a host-loss simulation), NOT a
	// graceful engine stop. Its Postgres session drops, releasing the session-level
	// advisory lock.
	pidA := readDaemonPID(t, pidPathA)
	if err := syscall.Kill(pidA, syscall.SIGKILL); err != nil {
		t.Fatalf("SIGKILL leader A (pid %d): %v", pidA, err)
	}
	stoppedA = true // A is dead; its cleanup engine-stop is unnecessary and would only race.

	// The standby's blocked pg_advisory_lock returns once A's session is gone: B
	// acquires the lock, runs startup reconciliation, and becomes the leader.
	tookOver := waitForLeader(t, socketB)

	t.Run("failover-standby-takes-over", func(t *testing.T) {
		if !tookOver {
			t.Fatalf("standby B did not take over after leader A was killed (role=%q); the freed advisory lock must promote the standby", healthzRole(t, socketB))
		}
	})
	t.Run("failover-real-leader-kill", func(t *testing.T) {
		if !tookOver {
			t.Fatalf("killing the real leader did not result in the standby taking over (role=%q)", healthzRole(t, socketB))
		}
	})
}
