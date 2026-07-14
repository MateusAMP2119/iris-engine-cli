//go:build conformance

package conformance

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"syscall"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
)

// This file is the end-to-end proof of cross-host leader advertisement: a real
// leader advertises its address into
// the shared meta, a real standby names that live address for retargeting, and after
// the leader is killed and the standby takes over, the advertisement converges on the
// new leader. It reuses the two-daemon-on-one-meta pattern the E11 failover legs
// established, adding TCP listeners so the advertised address is a concrete,
// routable value -- the whole point flagged by E11.2 (guidance had the shape but no
// concrete address).
//
// It needs two candidates on ONE shared meta, so it runs only in external mode
// (IRIS_PG_DSN set, the conformance/CI configuration); managed Postgres gives each
// daemon its own cluster, so there is no shared advisory lock to contend for and no
// standby. It skips when no external DSN is present.

// leaderReport issues GET /leader over the daemon's socket and returns the reported
// role and the leader's advertised address as that node knows it.
func leaderReport(t *testing.T, socket string) (role, leader string) {
	t.Helper()
	resp, err := HTTPOverSocket(socket).Get("http://iris/leader")
	if err != nil {
		return "", ""
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		return "", ""
	}
	var env struct {
		Data struct {
			Role   string `json:"role"`
			Leader string `json:"leader"`
		} `json:"data"`
	}
	if err := json.Unmarshal(body, &env); err != nil {
		return "", ""
	}
	return env.Data.Role, env.Data.Leader
}

// waitLeaderReport polls GET /leader on the standby until it names want as the
// leader's advertised address, proving the standby read the meta advertisement.
func waitLeaderReport(t *testing.T, socket, want string) bool {
	t.Helper()
	deadline := time.Now().Add(30 * time.Second)
	for time.Now().Before(deadline) {
		if _, leader := leaderReport(t, socket); leader == want {
			return true
		}
		time.Sleep(100 * time.Millisecond)
	}
	return false
}

// scLeadershipAddr reads the advertised_addr the leader recorded in the single-row
// leadership meta table (empty when no row yet or a socket-only leader).
func scLeadershipAddr(conn *pgx.Conn) string {
	var addr string
	_ = conn.QueryRow(context.Background(),
		"SELECT coalesce(advertised_addr, '') FROM leadership WHERE id = 1").Scan(&addr)
	return addr
}

// waitLeadershipAddr polls the leadership table until advertised_addr equals want,
// returning whether it converged. Readiness is the observed advertisement, never
// elapsed time.
func waitLeadershipAddr(t *testing.T, conn *pgx.Conn, want string, deadline time.Duration) bool {
	t.Helper()
	dl := time.Now().Add(deadline)
	for time.Now().Before(dl) {
		if scLeadershipAddr(conn) == want {
			return true
		}
		time.Sleep(150 * time.Millisecond)
	}
	return false
}

// startDaemonTCP starts a detached daemon on ws with a TCP listener at tcpAddr,
// optionally installing first (the shared meta is installed once, by the leader),
// waits for its socket, and registers a stop cleanup. It returns the socket path.
func startDaemonTCP(t *testing.T, bin *Binary, ws, tcpAddr string, install bool) string {
	t.Helper()
	socket := filepath.Join(ws, ".iris", "iris.sock")
	if install {
		bin.Run(t, RunOptions{Args: []string{"engine", "install"}, Dir: ws, Timeout: 5 * time.Minute}).RequireExit(t, 0)
	}
	bin.Run(t, RunOptions{Args: []string{"engine", "start", "-d", "--tcp", tcpAddr}, Dir: ws, Timeout: 2 * time.Minute}).RequireExit(t, 0)
	t.Cleanup(func() {
		bin.Run(t, RunOptions{Args: []string{"engine", "stop"}, Dir: ws, Timeout: 30 * time.Second})
	})
	readyCtx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	if err := WaitForSocket(readyCtx, socket); err != nil {
		t.Fatalf("daemon socket never became ready: %v", err)
	}
	return socket
}

// TestLeaderAdvertisementFailover is the end-to-end proof that a standby names the
// live leader's concrete address and that the advertisement follows a failover.
// Two real daemon candidates share one
// external meta, each with its own TCP listener: the leader advertises its TCP
// address into the shared meta, the standby reads it and names it on GET /leader.
// When the leader is SIGKILLed (host-loss) the standby acquires the freed lock, takes
// over, and re-advertises its OWN address -- so the leadership table converges on the
// new leader, never stranding the dead leader's address.
func TestLeaderAdvertisementFailover(t *testing.T) {
	if os.Getenv("IRIS_PG_DSN") == "" {
		t.Skip("leader advertisement failover needs two candidates on one shared meta; set IRIS_PG_DSN (external mode) to run it")
	}
	freshDatabases(t)
	bin := Build(t)

	addrLeader := freeTCPAddr(t)
	addrStandby := freeTCPAddr(t)

	wsLeader := shortWorkspace(t)
	wsStandby := shortWorkspace(t)

	// Leader: install the shared meta and start with its own TCP listener, then wait
	// for confirmed leadership.
	leaderSock := startDaemonTCP(t, bin, wsLeader, addrLeader, true)
	if !waitForLeader(t, leaderSock) {
		t.Fatalf("first candidate never became leader (role=%q)", healthzRole(t, leaderSock))
	}

	// The leader advertised its TCP address into the shared meta on winning the lock.
	meta := scMetaConn(t, wsLeader)
	if !waitLeadershipAddr(t, meta, addrLeader, 30*time.Second) {
		t.Fatalf("leader did not advertise its address %q into meta (leadership.advertised_addr = %q)",
			addrLeader, scLeadershipAddr(meta))
	}

	// Standby: a second daemon on the shared meta with its own TCP listener. It blocks
	// on the held lock and stays a standby.
	standbySock := startDaemonTCP(t, bin, wsStandby, addrStandby, false)
	if !waitForStandby(t, standbySock) {
		t.Fatalf("second candidate never reported standby (role=%q)", healthzRole(t, standbySock))
	}

	// The standby names the live leader's concrete address on GET /leader: it read the
	// meta advertisement, so the guidance is no longer "unknown".
	if !waitLeaderReport(t, standbySock, addrLeader) {
		_, got := leaderReport(t, standbySock)
		t.Fatalf("standby GET /leader named %q, want the live leader's advertised address %q", got, addrLeader)
	}

	// Kill the leader abruptly (host loss): its Postgres session drops and frees the
	// advisory lock.
	leaderPID := readDaemonPID(t, filepath.Join(wsLeader, ".iris", "iris.pid"))
	if err := syscall.Kill(leaderPID, syscall.SIGKILL); err != nil {
		t.Fatalf("SIGKILL leader (pid %d): %v", leaderPID, err)
	}

	// The standby acquires the freed lock and takes over.
	if !waitForLeader(t, standbySock) {
		t.Fatalf("standby did not take over after the leader was killed (role=%q)", healthzRole(t, standbySock))
	}

	// The advertisement converges on the NEW leader: it re-advertised its own address,
	// superseding the dead leader's, so the leadership table now names the standby's
	// TCP address -- never leaving the killed leader's stale address behind.
	if !waitLeadershipAddr(t, meta, addrStandby, 30*time.Second) {
		t.Fatalf("advertisement did not update to the new leader %q after failover (leadership.advertised_addr = %q)",
			addrStandby, scLeadershipAddr(meta))
	}
}
