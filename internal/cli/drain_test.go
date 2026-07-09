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

// startDrainStub stands up an in-process daemon over a unix socket that answers
// POST /deadletter/drain with the given status and body, so the real drain client is
// driven end to end (resolve/validate scope, POST it, classify the reply) with no
// live daemon -- the integration-tier "in-process daemon over a socket" the coder
// doctrine names, standing in for the leader's drain route until the daemon-routes
// wiring pass connects it to the real worklist write (store.DrainDeadLetters).
func startDrainStub(t *testing.T, sock string, status int, body any) {
	t.Helper()
	ln, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatalf("listen unix %s: %v", sock, err)
	}
	mux := http.NewServeMux()
	mux.HandleFunc("/deadletter/drain", func(w http.ResponseWriter, r *http.Request) {
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

// TestDeadletterDrainRequiresExplicitScope proves `iris deadletter drain` refuses a
// bare invocation as a usage error (S12/drain-requires-explicit-scope) and, once
// given an explicit scope, reaches the leader for every one of the three forms
// (<run>, --pipeline, --all): drain has no re-dead-letter outcome (it is a pure
// discard, never a re-run), so a clean drain always exits 0.
//
// spec: S12/drain-requires-explicit-scope
func TestDeadletterDrainRequiresExplicitScope(t *testing.T) {
	// Isolate ambient IRIS_* config so --socket is authoritative (a real IRIS_HOST
	// would otherwise win over the flag socket and the test would dial elsewhere).
	t.Setenv("IRIS_HOST", "")
	t.Setenv("IRIS_SOCKET", "")
	t.Setenv("IRIS_TOKEN", "")

	t.Run("bare invocation is a usage error (exit 2)", func(t *testing.T) {
		var out, errb bytes.Buffer
		code := newApp(&out, &errb).run([]string{"deadletter", "drain"})
		if code != exitUsage {
			t.Fatalf("bare drain exit = %d, want %d (usage; nothing defaults to everything)\nstderr: %s", code, exitUsage, errb.String())
		}
	})

	t.Run("<run> scope reaches the leader and reports success", func(t *testing.T) {
		sock := shortSocket(t)
		startDrainStub(t, sock, http.StatusOK, map[string]any{
			"data": drainOutcome{Drained: []string{"10"}},
		})
		var out, errb bytes.Buffer
		code := newApp(&out, &errb).run([]string{"--socket", sock, "deadletter", "drain", "10"})
		if code != exitOK {
			t.Fatalf("<run> drain exit = %d, want %d\nstdout: %s\nstderr: %s", code, exitOK, out.String(), errb.String())
		}
		if !strings.Contains(out.String(), "10") {
			t.Errorf("<run> drain did not report the drained run on stdout:\n%s", out.String())
		}
	})

	t.Run("--pipeline scope reaches the leader", func(t *testing.T) {
		sock := shortSocket(t)
		startDrainStub(t, sock, http.StatusOK, map[string]any{
			"data": drainOutcome{Drained: []string{"20", "21"}},
		})
		var out, errb bytes.Buffer
		code := newApp(&out, &errb).run([]string{"--socket", sock, "deadletter", "drain", "--pipeline", "load_orders"})
		if code != exitOK {
			t.Fatalf("--pipeline drain exit = %d, want %d\nstderr: %s", code, exitOK, errb.String())
		}
	})

	t.Run("--all scope reaches the leader", func(t *testing.T) {
		sock := shortSocket(t)
		startDrainStub(t, sock, http.StatusOK, map[string]any{
			"data": drainOutcome{Drained: []string{"10", "20", "21"}},
		})
		var out, errb bytes.Buffer
		code := newApp(&out, &errb).run([]string{"--socket", sock, "deadletter", "drain", "--all"})
		if code != exitOK {
			t.Fatalf("--all drain exit = %d, want %d\nstderr: %s", code, exitOK, errb.String())
		}
	})

	t.Run("no outstanding entries reports cleanly", func(t *testing.T) {
		sock := shortSocket(t)
		startDrainStub(t, sock, http.StatusOK, map[string]any{
			"data": drainOutcome{},
		})
		var out, errb bytes.Buffer
		code := newApp(&out, &errb).run([]string{"--socket", sock, "deadletter", "drain", "--all"})
		if code != exitOK {
			t.Fatalf("empty drain exit = %d, want %d\nstderr: %s", code, exitOK, errb.String())
		}
	})

	t.Run("not the leader exits 6", func(t *testing.T) {
		sock := shortSocket(t)
		startDrainStub(t, sock, http.StatusMisdirectedRequest, map[string]any{
			"error": map[string]any{"code": "not_leader", "message": "this daemon is not the leader", "leader": "host-b"},
		})
		var out, errb bytes.Buffer
		code := newApp(&out, &errb).run([]string{"--socket", sock, "deadletter", "drain", "--all"})
		if code != exitNotLeader {
			t.Fatalf("not-leader drain exit = %d, want %d\nstderr: %s", code, exitNotLeader, errb.String())
		}
		if !strings.Contains(errb.String(), "host-b") {
			t.Errorf("not-leader message does not name the leader for retargeting:\n%s", errb.String())
		}
	})

	t.Run("no daemon reachable exits 3", func(t *testing.T) {
		sock := shortSocket(t) // nothing listening
		var out, errb bytes.Buffer
		code := newApp(&out, &errb).run([]string{"--socket", sock, "deadletter", "drain", "10"})
		if code != exitNoDaemon {
			t.Fatalf("no-daemon drain exit = %d, want %d\nstderr: %s", code, exitNoDaemon, errb.String())
		}
	})
}

// TestDeadletterReplayAndDrainRequireExplicitScope proves both dead-letter
// dispositions refuse a bare invocation as a usage error (exit 2), never defaulting
// the missing scope to --all (specification sections 6.2, 8, and 12): both `iris
// deadletter replay` and `iris deadletter drain` require an explicit <run>,
// --pipeline <name>, or --all. Neither command dials the daemon to decide this: with
// nothing listening at sock, a scope silently defaulted to --all would surface as
// exit 3 (no daemon reachable), never exit 2 -- so exit 2 here is the proof the bare
// form never reaches the network.
//
// spec: S08/deadletter-scope-required
// spec: S06.2/replay-drain-bare-usage-error
func TestDeadletterReplayAndDrainRequireExplicitScope(t *testing.T) {
	t.Setenv("IRIS_HOST", "")
	t.Setenv("IRIS_SOCKET", "")
	t.Setenv("IRIS_TOKEN", "")

	sock := shortSocket(t) // nothing listening throughout this test

	t.Run("bare replay is a usage error, not a silent --all", func(t *testing.T) {
		var out, errb bytes.Buffer
		code := newApp(&out, &errb).run([]string{"--socket", sock, "deadletter", "replay"})
		if code != exitUsage {
			t.Fatalf("bare replay exit = %d, want %d\nstderr: %s", code, exitUsage, errb.String())
		}
	})

	t.Run("bare drain is a usage error, not a silent --all", func(t *testing.T) {
		var out, errb bytes.Buffer
		code := newApp(&out, &errb).run([]string{"--socket", sock, "deadletter", "drain"})
		if code != exitUsage {
			t.Fatalf("bare drain exit = %d, want %d\nstderr: %s", code, exitUsage, errb.String())
		}
	})
}
