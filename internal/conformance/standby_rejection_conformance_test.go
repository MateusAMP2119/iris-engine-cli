//go:build conformance

package conformance

import (
	"context"
	"os"
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
// rejects them with leader guidance and exit 6 (specification sections 7, 8 and
// 15). It stands up two real daemon candidates sharing one external meta: the
// first wins the advisory lock and leads; the second, blocked on that lock, stays
// a standby. A mutation run through the shipped CLI against the standby's socket
// must exit 6, and its message (and --json envelope) must carry the leader
// guidance for retargeting.
//
// The scenario needs two candidates on one shared meta, so it runs only in
// external mode (IRIS_PG_DSN set, the conformance/CI configuration): under
// managed Postgres each daemon owns a private cluster and both would lead, so
// there is no standby to reject against. It is skipped when no external DSN is
// present.
func TestStandbyMutationRejection(t *testing.T) {
	if os.Getenv("IRIS_PG_DSN") == "" {
		t.Skip("standby rejection needs two candidates on one shared meta; set IRIS_PG_DSN (external mode) to run it")
	}
	// Freshen the shared external cluster first: FORCE-dropping meta/data evicts a prior
	// test's lingering daemon sessions (including a still-held leader advisory lock), so
	// the first candidate here wins the lock and leads instead of timing out behind a
	// stale leader (the second candidate then contends this run's own leader as intended).
	freshDatabases(t)
	bin := Build(t)

	// Leader candidate: install (external no-op that ensures the shared meta) and
	// start detached, then wait until it holds the advisory lock and leads.
	leaderWS := shortWorkspace(t)
	leaderSock := filepath.Join(leaderWS, ".iris", "iris.sock")
	bin.Run(t, RunOptions{Args: []string{"engine", "install"}, Dir: leaderWS, Timeout: 5 * time.Minute}).RequireExit(t, 0)
	bin.Run(t, RunOptions{Args: []string{"engine", "start", "-d"}, Dir: leaderWS, Timeout: 2 * time.Minute}).RequireExit(t, 0)
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

	// spec: S15/standby-mutation-exit-6
	t.Run("S15/standby-mutation-exit-6", func(t *testing.T) {
		// A control mutation POSTed to the standby's socket is gated to the leader:
		// the shipped CLI maps the not_leader rejection to exit 6.
		res := bin.Run(t, RunOptions{Args: []string{"--socket", standbySock, "pipeline", "promote", "any_pipeline"}, Dir: standbyWS})
		res.RequireExit(t, 6)
	})

	// spec: S07/standby-mutations-rejected-exit-6
	t.Run("S07/standby-mutations-rejected-exit-6", func(t *testing.T) {
		res := bin.Run(t, RunOptions{Args: []string{"--socket", standbySock, "pipeline", "promote", "any_pipeline"}, Dir: standbyWS})
		res.RequireExit(t, 6)
		// The rejection guides the operator to the leader (only the leader accepts
		// mutations): the message names the leader for retargeting.
		out := strings.ToLower(string(res.Stdout) + string(res.Stderr))
		if !strings.Contains(out, "leader") {
			t.Errorf("exit-6 rejection did not point to the leader:\nstdout:\n%s\nstderr:\n%s", res.Stdout, res.Stderr)
		}
	})

	// spec: S08/exit6-names-leader
	t.Run("S08/exit6-names-leader", func(t *testing.T) {
		res := bin.Run(t, RunOptions{Args: []string{"--socket", standbySock, "--json", "pipeline", "promote", "any_pipeline"}, Dir: standbyWS})
		res.RequireExit(t, 6)
		// Under --json the single stdout document is the not_leader error envelope:
		// its machine code is not_leader and its message names the leader for
		// retargeting.
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
	})
}
