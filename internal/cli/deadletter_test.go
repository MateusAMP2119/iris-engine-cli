package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"net"
	"net/http"
	"strings"
	"testing"
	"time"
)

// startReplayStub stands up an in-process daemon over a unix socket that answers POST
// /deadletter/replay with the given status and body, so the real replay client is
// driven end to end (resolve target, POST the scope, classify the reply) with no live
// daemon. It is the integration-tier "in-process daemon over a socket" the coder
// doctrine names, standing in for the leader's replay route until E05.10/E05.12 wire
// the daemon-side lane runner that mints and runs the replacement.
func startReplayStub(t *testing.T, sock string, status int, body any) {
	t.Helper()
	ln, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatalf("listen unix %s: %v", sock, err)
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
	t.Cleanup(func() { _ = srv.Shutdown(context.Background()) })
}

// TestDeadletterReplayExit5 proves the dead-lettering-replay exit contract:
// `iris deadletter replay` exits 5 when the leader reports a replay whose fresh
// run dead-lettered again, exits 0 for a clean
// replay, requires a scope (bare is a usage error, exit 2), and reports exit 6 when
// the daemon is not the leader. The re-dead-lettered run (chained to the original via
// replayed_from) is what drives exit 5.
func TestDeadletterReplayExit5(t *testing.T) {
	// Isolate the ambient IRIS_* config so --socket is authoritative: a real
	// IRIS_HOST in the environment would otherwise win over the flag socket
	// (daemonHTTPClient prefers a configured host) and the test would dial elsewhere.
	t.Setenv("IRIS_HOST", "")
	t.Setenv("IRIS_SOCKET", "")
	t.Setenv("IRIS_TOKEN", "")

	t.Run("bare invocation is a usage error (exit 2)", func(t *testing.T) {
		var out, errb bytes.Buffer
		code := newApp(&out, &errb).run([]string{"deadletter", "replay"})
		if code != exitUsage {
			t.Fatalf("bare replay exit = %d, want %d (usage; nothing defaults to everything)\nstderr: %s", code, exitUsage, errb.String())
		}
	})

	t.Run("a re-dead-lettering replay exits 5", func(t *testing.T) {
		sock := shortSocket(t)
		// The leader replayed run 10 as run 40, and run 40 dead-lettered again (chained
		// to the original 10 via replayed_from).
		startReplayStub(t, sock, http.StatusOK, map[string]any{
			"data": replayOutcome{
				DeadLettered: []replayedRun{
					{ReplacedRun: "10", ReplacementRun: "40", ReplayedFrom: "10"},
				},
			},
		})
		var out, errb bytes.Buffer
		code := newApp(&out, &errb).run([]string{"--socket", sock, "deadletter", "replay", "10"})
		if code != exitDeadLettered {
			t.Fatalf("re-dead-lettering replay exit = %d, want %d\nstdout: %s\nstderr: %s", code, exitDeadLettered, out.String(), errb.String())
		}
		// The re-dead-lettered replacement is named for the operator.
		if !strings.Contains(errb.String(), "40") {
			t.Errorf("exit-5 message does not name the re-dead-lettered replacement run:\n%s", errb.String())
		}
	})

	t.Run("a clean replay exits 0", func(t *testing.T) {
		sock := shortSocket(t)
		startReplayStub(t, sock, http.StatusOK, map[string]any{
			"data": replayOutcome{
				Replayed: []replayedRun{
					{ReplacedRun: "10", ReplacementRun: "40", ReplayedFrom: "10"},
				},
			},
		})
		var out, errb bytes.Buffer
		code := newApp(&out, &errb).run([]string{"--socket", sock, "deadletter", "replay", "--all"})
		if code != exitOK {
			t.Fatalf("clean replay exit = %d, want %d\nstdout: %s\nstderr: %s", code, exitOK, out.String(), errb.String())
		}
		if !strings.Contains(out.String(), "replayed 10 as 40") {
			t.Errorf("clean replay did not report the replacement on stdout:\n%s", out.String())
		}
	})

	t.Run("not the leader exits 6", func(t *testing.T) {
		sock := shortSocket(t)
		startReplayStub(t, sock, http.StatusMisdirectedRequest, map[string]any{
			"error": map[string]any{"code": "not_leader", "message": "this daemon is not the leader", "leader": "host-b"},
		})
		var out, errb bytes.Buffer
		code := newApp(&out, &errb).run([]string{"--socket", sock, "deadletter", "replay", "--pipeline", "extract"})
		if code != exitNotLeader {
			t.Fatalf("not-leader replay exit = %d, want %d\nstderr: %s", code, exitNotLeader, errb.String())
		}
		if !strings.Contains(errb.String(), "host-b") {
			t.Errorf("not-leader message does not name the leader for retargeting:\n%s", errb.String())
		}
	})

	t.Run("no daemon reachable exits 3", func(t *testing.T) {
		sock := shortSocket(t) // nothing listening
		var out, errb bytes.Buffer
		code := newApp(&out, &errb).run([]string{"--socket", sock, "deadletter", "replay", "10"})
		if code != exitNoDaemon {
			t.Fatalf("no-daemon replay exit = %d, want %d\nstderr: %s", code, exitNoDaemon, errb.String())
		}
	})
}
