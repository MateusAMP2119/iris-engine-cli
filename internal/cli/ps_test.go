package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/MateusAMP2119/iris-lakehouse/internal/api"

	"github.com/spf13/cobra"
)

// psFunc adapts a function to the api.PsHandler interface for the
// integration-tier daemon fakes.
type psFunc func(ctx context.Context, all, history bool) (api.PsPayload, error)

func (f psFunc) Ps(ctx context.Context, all, history bool) (api.PsPayload, error) {
	return f(ctx, all, history)
}

// startPsDaemon stands up an in-process daemon over a unix socket serving the
// REAL api mux with the given ps handler -- the integration-tier "in-process
// daemon over a socket" pattern -- so `iris ps` and a raw GET /ps read the
// same engine state through the same route.
func startPsDaemon(t *testing.T, sock string, handler api.PsHandler) {
	t.Helper()
	ln, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatalf("listen unix %s: %v", sock, err)
	}
	srv := &http.Server{Handler: api.NewMux(api.WithPs(handler)), ReadHeaderTimeout: 5 * time.Second}
	go func() { _ = srv.Serve(ln) }()
	t.Cleanup(func() { _ = srv.Shutdown(context.Background()) })
}

// psFixture is the canned readout the tests serve: a leader engine with load,
// one running run with load, and one queued run without.
func psFixture() api.PsPayload {
	return api.PsPayload{
		Engine: api.PsEngine{
			Version: "dev", Role: "leader", PID: 4242, Uptime: "1h2m3s",
			QueuedRuns: 1, RunningRuns: 1,
			Load: &api.PsLoad{CPUPercent: 2.5, RSSBytes: 150 << 20},
		},
		Runs: []api.PsRun{
			{ID: "8", Pipeline: "load_orders", Lane: "ingest", State: "queued"},
			{ID: "7", Pipeline: "extract", Lane: "ingest", State: "running",
				Load: &api.PsLoad{CPUPercent: 51.0, RSSBytes: 24 << 20}},
		},
	}
}

// TestPsParity proves GET /ps and `iris ps` return the identical payload for
// the same engine state: both surfaces read the one mux route backed by the
// one handler, and the CLI's --json data envelope carries exactly the route's
// data document.
func TestPsParity(t *testing.T) {
	t.Setenv("IRIS_HOST", "")
	t.Setenv("IRIS_SOCKET", "")
	t.Setenv("IRIS_TOKEN", "")

	t.Run("ps-cli-http-parity", func(t *testing.T) {
		sock := shortSocket(t)
		startPsDaemon(t, sock, psFunc(func(context.Context, bool, bool) (api.PsPayload, error) {
			return psFixture(), nil
		}))

		var out, errb bytes.Buffer
		code := newApp(&out, &errb).run([]string{"--socket", sock, "ps", "--json"})
		if code != exitOK {
			t.Fatalf("ps --json exit = %d, want %d\nstderr: %s", code, exitOK, errb.String())
		}
		var cliEnv struct {
			Data any `json:"data"`
		}
		if err := json.Unmarshal(out.Bytes(), &cliEnv); err != nil {
			t.Fatalf("decode CLI ps envelope: %v\nstdout: %s", err, out.String())
		}

		client := &http.Client{Transport: &http.Transport{
			DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
				var d net.Dialer
				return d.DialContext(ctx, "unix", sock)
			},
		}}
		resp, err := client.Get("http://iris/ps")
		if err != nil {
			t.Fatalf("GET /ps: %v", err)
		}
		defer resp.Body.Close()
		raw, err := io.ReadAll(resp.Body)
		if err != nil {
			t.Fatalf("read /ps body: %v", err)
		}
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("GET /ps status = %d, want 200\nbody: %s", resp.StatusCode, raw)
		}
		var httpEnv struct {
			Data any `json:"data"`
		}
		if err := json.Unmarshal(raw, &httpEnv); err != nil {
			t.Fatalf("decode HTTP ps envelope: %v\nbody: %s", err, raw)
		}

		if !reflect.DeepEqual(cliEnv.Data, httpEnv.Data) {
			t.Errorf("CLI and HTTP ps payloads differ\nCLI:  %v\nHTTP: %v", cliEnv.Data, httpEnv.Data)
		}
		if cliEnv.Data == nil {
			t.Fatal("ps payload is empty; parity proven over nothing")
		}
	})
}

// TestPsOutputMode proves the one output rule: stdout a TTY and --json absent
// opens the live view; every other invocation -- piped, --json, a refused raw
// mode -- emits the route's data envelope once. The removed -a/-q flags fail
// loud, --all shapes the JSON document only, and a live view losing its
// engine exits no-daemon with reachability guidance.
func TestPsOutputMode(t *testing.T) {
	t.Setenv("IRIS_HOST", "")
	t.Setenv("IRIS_SOCKET", "")
	t.Setenv("IRIS_TOKEN", "")

	t.Run("bare ps on a non-TTY stdout emits the route's JSON envelope", func(t *testing.T) {
		sock := shortSocket(t)
		startPsDaemon(t, sock, psFunc(func(context.Context, bool, bool) (api.PsPayload, error) {
			return psFixture(), nil
		}))

		var bare, jsonOut, errb bytes.Buffer
		if code := newApp(&bare, &errb).run([]string{"--socket", sock, "ps"}); code != exitOK {
			t.Fatalf("bare ps exit = %d, want %d\nstderr: %s", code, exitOK, errb.String())
		}
		if code := newApp(&jsonOut, &errb).run([]string{"--socket", sock, "ps", "--json"}); code != exitOK {
			t.Fatalf("ps --json exit = %d, want %d\nstderr: %s", code, exitOK, errb.String())
		}
		if bare.String() != jsonOut.String() {
			t.Errorf("piped bare ps and ps --json must emit identical bytes\nbare: %s\njson: %s", bare.String(), jsonOut.String())
		}
		var env struct {
			Data *api.PsPayload `json:"data"`
		}
		if err := json.Unmarshal(bare.Bytes(), &env); err != nil || env.Data == nil {
			t.Fatalf("piped bare ps is not the data envelope (err %v):\n%s", err, bare.String())
		}
		if env.Data.Engine.Role != "leader" || len(env.Data.Runs) != 2 {
			t.Errorf("envelope did not round-trip: %+v", env.Data)
		}
	})

	t.Run("a TTY without --json enters the live view", func(t *testing.T) {
		sock := shortSocket(t)
		startPsDaemon(t, sock, psFunc(func(context.Context, bool, bool) (api.PsPayload, error) {
			return psFixture(), nil
		}))

		var out, errb bytes.Buffer
		a := newApp(&out, &errb)
		a.isTTY = func() bool { return true }
		a.stdinIsTTY = func() bool { return true }
		var called atomic.Int32
		var gotFirst psSnapshot
		var gotTarget string
		a.psLive = func(_ *cobra.Command, _ *psDaemonClient, first psSnapshot, target string) (bool, error) {
			called.Add(1)
			gotFirst, gotTarget = first, target
			return true, nil
		}
		if code := a.run([]string{"--socket", sock, "ps"}); code != exitOK {
			t.Fatalf("live ps exit = %d, want %d\nstderr: %s", code, exitOK, errb.String())
		}
		if called.Load() != 1 {
			t.Fatalf("live view entered %d times, want once", called.Load())
		}
		if out.Len() != 0 {
			t.Errorf("the live path wrote to stdout outside the view: %q", out.String())
		}
		if gotFirst.ps.Engine.PID != 4242 {
			t.Errorf("live view first snapshot = %+v, want the fetched readout", gotFirst.ps.Engine)
		}
		if gotTarget != "local "+sock {
			t.Errorf("live view target = %q, want %q", gotTarget, "local "+sock)
		}
	})

	t.Run("--json on a TTY stays JSON, never the view", func(t *testing.T) {
		sock := shortSocket(t)
		startPsDaemon(t, sock, psFunc(func(context.Context, bool, bool) (api.PsPayload, error) {
			return psFixture(), nil
		}))

		var out, errb bytes.Buffer
		a := newApp(&out, &errb)
		a.isTTY = func() bool { return true }
		a.stdinIsTTY = func() bool { return true }
		a.psLive = func(*cobra.Command, *psDaemonClient, psSnapshot, string) (bool, error) {
			t.Error("--json must never enter the live view")
			return true, nil
		}
		if code := a.run([]string{"--socket", sock, "ps", "--json"}); code != exitOK {
			t.Fatalf("ps --json exit = %d, want %d\nstderr: %s", code, exitOK, errb.String())
		}
		if !strings.Contains(out.String(), `"engine"`) {
			t.Errorf("--json on a TTY did not emit the envelope: %s", out.String())
		}
	})

	t.Run("a refused raw mode falls back to the JSON emit", func(t *testing.T) {
		sock := shortSocket(t)
		startPsDaemon(t, sock, psFunc(func(context.Context, bool, bool) (api.PsPayload, error) {
			return psFixture(), nil
		}))

		var out, errb bytes.Buffer
		a := newApp(&out, &errb)
		a.isTTY = func() bool { return true }
		a.stdinIsTTY = func() bool { return true }
		a.psLive = func(*cobra.Command, *psDaemonClient, psSnapshot, string) (bool, error) {
			return false, nil // stdin refused raw mode
		}
		if code := a.run([]string{"--socket", sock, "ps"}); code != exitOK {
			t.Fatalf("fallback ps exit = %d, want %d\nstderr: %s", code, exitOK, errb.String())
		}
		var env struct {
			Data *api.PsPayload `json:"data"`
		}
		if err := json.Unmarshal(out.Bytes(), &env); err != nil || env.Data == nil {
			t.Fatalf("fallback did not emit the envelope (err %v):\n%s", err, out.String())
		}
	})

	t.Run("--all rides the JSON request as ?all=true, bare stays default", func(t *testing.T) {
		sock := shortSocket(t)
		var sawAll atomic.Bool
		startPsDaemon(t, sock, psFunc(func(_ context.Context, all, _ bool) (api.PsPayload, error) {
			sawAll.Store(all)
			return psFixture(), nil
		}))

		var out, errb bytes.Buffer
		if code := newApp(&out, &errb).run([]string{"--socket", sock, "ps"}); code != exitOK {
			t.Fatalf("ps exit = %d, want %d\nstderr: %s", code, exitOK, errb.String())
		}
		if sawAll.Load() {
			t.Error("piped bare ps asked the daemon for all=true, want the default document")
		}
		if code := newApp(&out, &errb).run([]string{"--socket", sock, "ps", "--all"}); code != exitOK {
			t.Fatalf("ps --all exit = %d, want %d\nstderr: %s", code, exitOK, errb.String())
		}
		if !sawAll.Load() {
			t.Error("ps --all asked the daemon for all=false, want true")
		}
	})

	t.Run("the live view polls the whole history", func(t *testing.T) {
		sock := shortSocket(t)
		var sawAll atomic.Bool
		startPsDaemon(t, sock, psFunc(func(_ context.Context, all, _ bool) (api.PsPayload, error) {
			sawAll.Store(all)
			return psFixture(), nil
		}))

		var out, errb bytes.Buffer
		a := newApp(&out, &errb)
		a.isTTY = func() bool { return true }
		a.stdinIsTTY = func() bool { return true }
		a.psLive = func(*cobra.Command, *psDaemonClient, psSnapshot, string) (bool, error) { return true, nil }
		if code := a.run([]string{"--socket", sock, "ps"}); code != exitOK {
			t.Fatalf("live ps exit = %d, want %d\nstderr: %s", code, exitOK, errb.String())
		}
		if !sawAll.Load() {
			t.Error("the live view's first fetch must read the whole history")
		}
	})

	t.Run("--all under the live view is a usage error", func(t *testing.T) {
		var out, errb bytes.Buffer
		a := newApp(&out, &errb)
		a.isTTY = func() bool { return true }
		a.stdinIsTTY = func() bool { return true }
		code := a.run([]string{"ps", "--all"})
		if code != exitUsage {
			t.Fatalf("live ps --all exit = %d, want %d", code, exitUsage)
		}
		if s := errb.String(); !strings.Contains(s, `"a"`) || !strings.Contains(s, "--json") {
			t.Errorf("usage error must point at the a key and --json: %s", s)
		}
	})

	t.Run("the removed -a and -q flags fail loud", func(t *testing.T) {
		for _, flag := range []string{"-a", "-q"} {
			var out, errb bytes.Buffer
			if code := newApp(&out, &errb).run([]string{"ps", flag}); code != exitUsage {
				t.Errorf("ps %s exit = %d, want %d (unknown flag)", flag, code, exitUsage)
			}
		}
	})

	t.Run("a failed live poll exits 3 with reachability guidance", func(t *testing.T) {
		sock := shortSocket(t)
		startPsDaemon(t, sock, psFunc(func(context.Context, bool, bool) (api.PsPayload, error) {
			return psFixture(), nil
		}))

		var out, errb bytes.Buffer
		a := newApp(&out, &errb)
		a.isTTY = func() bool { return true }
		a.stdinIsTTY = func() bool { return true }
		a.psLive = func(*cobra.Command, *psDaemonClient, psSnapshot, string) (bool, error) {
			return true, errPsEngineGone // the poller lost the daemon mid-view
		}
		code := a.run([]string{"--socket", sock, "ps"})
		if code != exitNoDaemon {
			t.Fatalf("engine-gone ps exit = %d, want %d", code, exitNoDaemon)
		}
		// The teardown line is the docker-ps-shaped connect message naming the
		// exact target it lost.
		if s := errb.String(); !strings.Contains(s, "Cannot connect to the iris engine at unix://"+sock) ||
			!strings.Contains(s, "Is the engine running?") || !strings.Contains(s, "iris engine start") {
			t.Errorf("teardown line missing the docker-shaped reachability guidance: %s", s)
		}
	})

	t.Run("no daemon reachable exits 3 with the docker-shaped message", func(t *testing.T) {
		sock := shortSocket(t) // nothing listening
		var out, errb bytes.Buffer
		code := newApp(&out, &errb).run([]string{"--socket", sock, "ps"})
		if code != exitNoDaemon {
			t.Fatalf("no-daemon ps exit = %d, want %d\nstderr: %s", code, exitNoDaemon, errb.String())
		}
		if s := errb.String(); !strings.Contains(s, "Cannot connect to the iris engine at unix://"+sock) ||
			!strings.Contains(s, "Is the engine running?") {
			t.Errorf("no-daemon line missing the docker-shaped connect message: %s", s)
		}
	})

	t.Run("a reached daemon refusing the read exits 4 with its message", func(t *testing.T) {
		sock := shortSocket(t)
		startPsDaemon(t, sock, psFunc(func(context.Context, bool, bool) (api.PsPayload, error) {
			return api.PsPayload{}, api.ErrPsUnavailable // the route 500s "api: ps not available"
		}))

		var out, errb bytes.Buffer
		code := newApp(&out, &errb).run([]string{"--socket", sock, "ps"})
		if code != exitOpFailed {
			t.Fatalf("refused ps exit = %d, want %d (never no-daemon: the daemon answered)\nstderr: %s", code, exitOpFailed, errb.String())
		}
		if s := errb.String(); !strings.Contains(s, "ps not available") || strings.Contains(s, "iris engine start") {
			t.Errorf("refusal must carry the daemon's message, never start guidance: %s", s)
		}
	})

	t.Run("a TTY stdout with a piped stdin resolves to JSON up front", func(t *testing.T) {
		sock := shortSocket(t)
		var fetches atomic.Int32
		startPsDaemon(t, sock, psFunc(func(context.Context, bool, bool) (api.PsPayload, error) {
			fetches.Add(1)
			return psFixture(), nil
		}))

		var out, errb bytes.Buffer
		a := newApp(&out, &errb)
		a.isTTY = func() bool { return true }
		a.stdinIsTTY = func() bool { return false }
		a.psLive = func(*cobra.Command, *psDaemonClient, psSnapshot, string) (bool, error) {
			t.Error("a key-less stdin must never enter the live view")
			return true, nil
		}
		if code := a.run([]string{"--socket", sock, "ps"}); code != exitOK {
			t.Fatalf("piped-stdin ps exit = %d, want %d\nstderr: %s", code, exitOK, errb.String())
		}
		if !strings.Contains(out.String(), `"engine"`) {
			t.Errorf("piped-stdin ps did not emit the envelope: %s", out.String())
		}
		if fetches.Load() != 1 {
			t.Errorf("mode resolved late: %d /ps fetches, want exactly 1", fetches.Load())
		}
	})
}
