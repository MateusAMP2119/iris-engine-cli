//go:build conformance

package conformance

import (
	"context"
	"os"
	"path/filepath"
	"syscall"
	"testing"
	"time"

	"github.com/MateusAMP2119/iris-engine-cli/internal/store"
)

// This file is the session-death failover leg: the leader's meta SESSION dies
// while its daemon process stays alive (a Postgres restart, a network drop, a
// terminated backend -- pg_terminate_backend here), the complement of the
// process-kill leg in failover_conformance_test.go. Postgres frees the advisory
// lock, so the standby is promoted as usual; what this leg proves is the deposed
// side: the live process detects the loss through the leader lock's session
// watchdog, self-demotes (honest role reporting, no phantom leader), and
// re-enters standby on a fresh session -- all with no process restart.

// terminateLeaderSessionSQL terminates the one backend holding the granted leader
// advisory lock on the meta database: the leader's session-pinned connection. The
// standby's blocked pg_advisory_lock shows as an UNgranted advisory lock row and
// the reader pools hold no advisory lock at all, so both survive; only the
// leader's session dies. pg_terminate_backend needs no superuser here: the
// session was opened by the same IRIS_PG_DSN admin role this test connects as.
const terminateLeaderSessionSQL = `
SELECT pg_terminate_backend(l.pid)
FROM pg_locks l
JOIN pg_database d ON d.oid = l.database
WHERE l.locktype = 'advisory' AND l.granted AND d.datname = $1`

// TestFailoverSessionLossSelfDemotes drives two real daemons sharing one meta,
// terminates the LEADER'S META SESSION (never its process), and proves both
// failover halves: the standby takes over, and the still-running deposed daemon
// self-demotes to an honest standby instead of reporting leader forever.
func TestFailoverSessionLossSelfDemotes(t *testing.T) {
	if os.Getenv("IRIS_PG_DSN") == "" {
		t.Skip("session-loss failover needs two daemons sharing one external meta; set IRIS_PG_DSN (managed mode gives each daemon its own Postgres, so there is no shared advisory lock to contend for)")
	}
	freshDatabases(t)
	bin := Build(t)

	// Two workspaces: each daemon has its own socket, pidfile, objects_path, and
	// workspace tree -- distinct hosts sharing one meta.
	wsA := shortWorkspace(t)
	wsB := shortWorkspace(t)
	socketA := filepath.Join(wsA, ".iris", "iris.sock")
	socketB := filepath.Join(wsB, ".iris", "iris.sock")
	pidPathA := filepath.Join(wsA, ".iris", "iris.pid")

	// Install once against the shared external cluster; the second daemon shares
	// those databases, so it does not install.
	bin.Run(t, RunOptions{Args: []string{"engine", "install"}, Dir: wsA, Timeout: 5 * time.Minute}).RequireExit(t, 0)

	// Daemon A starts first and wins the free advisory lock: it becomes the leader.
	bin.Run(t, RunOptions{Args: []string{"engine", "start", "-d"}, Dir: wsA, Timeout: 2 * time.Minute}).RequireExit(t, 0)
	t.Cleanup(func() {
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

	// Daemon B starts second, contends for the held lock, and blocks: a standby.
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
		t.Fatalf("daemon B never became standby while A held the lock (role=%q)", healthzRole(t, socketB))
	}

	// Kill the leader's META SESSION, not its process: terminate the backend
	// holding the granted advisory lock. Connection death is not process death --
	// daemon A keeps running and must notice on its own.
	pidA := readDaemonPID(t, pidPathA)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	admin := connectPG(t, os.Getenv("IRIS_PG_DSN"))
	defer func() { _ = admin.Close(ctx) }()
	tag, err := admin.Exec(ctx, terminateLeaderSessionSQL, store.MetaDatabase)
	if err != nil {
		t.Fatalf("terminate the leader's meta session: %v", err)
	}
	if tag.RowsAffected() != 1 {
		t.Fatalf("terminated %d backends holding the granted leader lock, want exactly 1 (the leader's pinned session)", tag.RowsAffected())
	}

	// Postgres freed the lock: the standby's blocked pg_advisory_lock returns and
	// B becomes the dispatching leader.
	t.Run("sessionloss-standby-takes-over", func(t *testing.T) {
		if !waitForLeader(t, socketB) {
			t.Fatalf("standby B did not take over after the leader's session died (role=%q)", healthzRole(t, socketB))
		}
	})

	// The deposed daemon's session watchdog notices the dead session and
	// self-demotes: it reports standby (honest role, no phantom leader) while its
	// process stays alive -- same pid, no restart.
	t.Run("sessionloss-live-leader-self-demotes", func(t *testing.T) {
		if !waitForRole(t, socketA, "standby") {
			t.Fatalf("deposed daemon A still reports %q after its meta session died; the session watchdog must self-demote it", healthzRole(t, socketA))
		}
		if err := syscall.Kill(pidA, 0); err != nil {
			t.Fatalf("daemon A (pid %d) is not running after self-demotion; session loss must never kill the process: %v", pidA, err)
		}
		if got := readDaemonPID(t, pidPathA); got != pidA {
			t.Fatalf("daemon A pid changed %d -> %d; self-demotion must not restart the process", pidA, got)
		}
	})
}
