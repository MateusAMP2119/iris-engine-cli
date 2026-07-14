package socketio_test

import (
	"encoding/json"
	"io"
	"net/http"
	"testing"

	"github.com/MateusAMP2119/iris-engine-cli/internal/socketio"
)

// TestUnixSocketHTTPRoundTrip is the real socket-HTTP proof: an in-process
// net/http server bound to a throwaway unix socket in a temp dir, hit over the
// socket by an http.Client with a custom unix dialer. It exercises the real
// listener, real HTTP framing, and real socket I/O with no database -- the
// database-free convention E02's daemon control plane and the CLI's socket
// client reuse.
func TestUnixSocketHTTPRoundTrip(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]string{"role": "leader"})
	})

	srv := socketio.Serve(t, mux)

	// The unix socket exists on disk in a temp dir.
	if srv.Path == "" {
		t.Fatal("server has no socket path")
	}

	resp, err := srv.Client.Get("http://iris/healthz")
	if err != nil {
		t.Fatalf("GET over unix socket: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		t.Errorf("status = %d, want 200", resp.StatusCode)
	}
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read body: %v", err)
	}
	var got map[string]string
	if err := json.Unmarshal(body, &got); err != nil {
		t.Fatalf("decode JSON body %q: %v", body, err)
	}
	if got["role"] != "leader" {
		t.Errorf("role = %q, want leader (JSON round-tripped over the socket)", got["role"])
	}
}

// TestUnixSocketHTTPStatusPropagates proves the socket transport is a faithful
// HTTP channel, not a happy-path stub: a handler's non-200 status and error body
// arrive unchanged at the client.
func TestUnixSocketHTTPStatusPropagates(t *testing.T) {
	srv := socketio.Serve(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "no leader", http.StatusServiceUnavailable)
	}))

	resp, err := srv.Client.Get("http://iris/anything")
	if err != nil {
		t.Fatalf("GET over unix socket: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusServiceUnavailable {
		t.Errorf("status = %d, want 503", resp.StatusCode)
	}
	body, _ := io.ReadAll(resp.Body)
	if got := string(body); got != "no leader\n" {
		t.Errorf("body = %q, want the handler's error text", got)
	}
}
