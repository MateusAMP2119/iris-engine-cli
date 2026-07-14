//go:build conformance

package conformance

import (
	"context"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// waitForStandby polls healthz until the daemon reports the standby role or the
// deadline passes. Readiness is a condition (the daemon confirmed it is a
// contended standby), never elapsed time; the poll interval only keeps the loop
// from spinning.
func waitForStandby(t *testing.T, socket string) bool {
	t.Helper()
	deadline := time.Now().Add(30 * time.Second)
	for time.Now().Before(deadline) {
		if healthzRole(t, socket) == "standby" {
			return true
		}
		time.Sleep(100 * time.Millisecond)
	}
	return false
}

// TestStandbyMutationRejection is the end-to-end proof, against a real Postgres
// and the shipped binary, that mutations are the leader's alone and a standby
// rejects them with leader guidance and exit 6. It stands up two real daemon
// candidates sharing one external meta: the first wins the advisory lock and
// leads; the second, blocked on that lock, stays a standby. A mutation run
// through the shipped CLI against the standby's socket must exit 6, and its
// message (and --json envelope) must carry the leader guidance for retargeting.
//
// The scenario needs two candidates on one shared meta, so it runs only in
// external mode (IRIS_PG_DSN set, the conformance/CI configuration): under
// managed Postgres each daemon owns a private cluster and both would lead, so
// there is no standby to reject against. It is skipped when no external DSN is
// present.
func TestStandbyMutationRejection(t *testing.T) {
	// Two candidates share one meta: the suite-owned embedded cluster (or an
	// ambient IRIS_PG_DSN).
	requireSharedCluster(t)
	// Freshen the shared external cluster first: FORCE-dropping meta/data evicts a prior
	// test's lingering daemon sessions (including a still-held leader advisory lock), so
	// the first candidate here wins the lock and leads instead of timing out behind a
	// stale leader (the second candidate then contends this run's own leader as intended).
	freshDatabases(t)
	bin := Build(t)

	// Leader candidate: install (external no-op that ensures the shared meta) and
	// start detached with a TCP listener, so the leader advertises a concrete address
	// the standby's exit-6 guidance can name for retargeting, then wait until it holds
	// the advisory lock and leads.
	leaderAddr := freeTCPAddr(t)
	leaderWS := shortWorkspace(t)
	leaderSock := filepath.Join(leaderWS, ".iris", "iris.sock")
	bin.Run(t, RunOptions{Args: []string{"engine", "install"}, Dir: leaderWS, Timeout: 5 * time.Minute}).RequireExit(t, 0)
	bin.Run(t, RunOptions{Args: []string{"engine", "start", "-d", "--tcp", leaderAddr}, Dir: leaderWS, Timeout: 2 * time.Minute}).RequireExit(t, 0)
	t.Cleanup(func() {
		bin.Run(t, RunOptions{Args: []string{"engine", "stop"}, Dir: leaderWS, Timeout: 30 * time.Second})
	})
	readyCtx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	if err := WaitForSocket(readyCtx, leaderSock); err != nil {
		cancel()
		t.Fatalf("leader socket never became ready: %v", err)
	}
	cancel()
	if !waitForLeader(t, leaderSock) {
		t.Fatal("first candidate never became leader; cannot contend a standby against it")
	}

	// Standby candidate: a second daemon on the same external meta. In external
	// mode start does not require a prior install; it connects, contends for the
	// leader lock the first candidate holds, and stays a standby.
	standbyWS := shortWorkspace(t)
	standbySock := filepath.Join(standbyWS, ".iris", "iris.sock")
	bin.Run(t, RunOptions{Args: []string{"engine", "start", "-d"}, Dir: standbyWS, Timeout: 2 * time.Minute}).RequireExit(t, 0)
	t.Cleanup(func() {
		bin.Run(t, RunOptions{Args: []string{"engine", "stop"}, Dir: standbyWS, Timeout: 30 * time.Second})
	})
	standbyReadyCtx, cancelStandby := context.WithTimeout(context.Background(), 20*time.Second)
	if err := WaitForSocket(standbyReadyCtx, standbySock); err != nil {
		cancelStandby()
		t.Fatalf("standby socket never became ready: %v", err)
	}
	cancelStandby()
	if !waitForStandby(t, standbySock) {
		t.Fatalf("second candidate never reported standby (role=%q); it must block on the held lock", healthzRole(t, standbySock))
	}

	// A read works on the standby regardless of role: the standby serves reads,
	// so the socket is genuinely up and the rejection below is a mutation gate,
	// not a dead listener.
	requireHealthzOK(t, standbySock)

	// The standby reads the leader's advertisement from the shared meta and names the
	// leader's concrete TCP address on GET /leader -- the guidance is a real retarget
	// address, not the bare "unknown" a standby falls back to with no advertisement to
	// read. Wait for the poll to pick it up before asserting the exit-6 envelope
	// carries the same address.
	if !waitLeaderReport(t, standbySock, leaderAddr) {
		_, got := leaderReport(t, standbySock)
		t.Fatalf("standby GET /leader named %q, want the live leader's advertised address %q", got, leaderAddr)
	}

	t.Run("standby-mutation-exit-6", func(t *testing.T) {
		// A control mutation POSTed to the standby's socket is gated to the leader:
		// the shipped CLI maps the not_leader rejection to exit 6.
		res := bin.Run(t, RunOptions{Args: []string{"--socket", standbySock, "pipeline", "promote", "any_pipeline"}, Dir: standbyWS})
		res.RequireExit(t, 6)
	})

	t.Run("standby-mutations-rejected-exit-6", func(t *testing.T) {
		res := bin.Run(t, RunOptions{Args: []string{"--socket", standbySock, "pipeline", "promote", "any_pipeline"}, Dir: standbyWS})
		res.RequireExit(t, 6)
		// The rejection guides the operator to the leader (only the leader accepts
		// mutations): the message names the leader for retargeting.
		out := strings.ToLower(string(res.Stdout) + string(res.Stderr))
		if !strings.Contains(out, "leader") {
			t.Errorf("exit-6 rejection did not point to the leader:\nstdout:\n%s\nstderr:\n%s", res.Stdout, res.Stderr)
		}
	})

	t.Run("exit6-names-leader", func(t *testing.T) {
		res := bin.Run(t, RunOptions{Args: []string{"--socket", standbySock, "--json", "pipeline", "promote", "any_pipeline"}, Dir: standbyWS})
		res.RequireExit(t, 6)
		// Under --json the single stdout document is the not_leader error envelope: its
		// machine code is not_leader and its message names the leader by its concrete
		// advertised TCP address for retargeting -- not the bare "unknown" hint, since
		// the leader advertises its address into the shared meta and the standby polls
		// it. The CLI folds the daemon's leader hint into the retarget guidance, so the
		// address appears in the message text.
		var env struct {
			Error struct {
				Code    string `json:"code"`
				Message string `json:"message"`
			} `json:"error"`
		}
		res.DecodeJSON(t, &env)
		if env.Error.Code != "not_leader" {
			t.Errorf("--json error code = %q, want not_leader", env.Error.Code)
		}
		if !strings.Contains(strings.ToLower(env.Error.Message), "leader") {
			t.Errorf("--json message did not name the leader for retargeting: %q", env.Error.Message)
		}
		if !strings.Contains(env.Error.Message, leaderAddr) {
			t.Errorf("--json message did not carry the leader's concrete address %q for retargeting: %q", leaderAddr, env.Error.Message)
		}
	})
}
