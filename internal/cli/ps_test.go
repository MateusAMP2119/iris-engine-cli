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
)

// psFunc adapts a function to the api.PsHandler interface for the
// integration-tier daemon fakes.
type psFunc func(ctx context.Context, all bool) (api.PsPayload, error)

func (f psFunc) Ps(ctx context.Context, all bool) (api.PsPayload, error) { return f(ctx, all) }

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
		startPsDaemon(t, sock, psFunc(func(context.Context, bool) (api.PsPayload, error) {
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

// TestPsRendering proves the human and quiet surfaces: the engine block and
// run table with load cells (a "-" for a row without a sample), -q printing
// run ids only, and -a riding the request as ?all=true.
func TestPsRendering(t *testing.T) {
	t.Setenv("IRIS_HOST", "")
	t.Setenv("IRIS_SOCKET", "")
	t.Setenv("IRIS_TOKEN", "")

	t.Run("human readout renders the engine block and the run table", func(t *testing.T) {
		sock := shortSocket(t)
		startPsDaemon(t, sock, psFunc(func(context.Context, bool) (api.PsPayload, error) {
			return psFixture(), nil
		}))

		var out, errb bytes.Buffer
		code := newApp(&out, &errb).run([]string{"--socket", sock, "ps"})
		if code != exitOK {
			t.Fatalf("ps exit = %d, want %d\nstderr: %s", code, exitOK, errb.String())
		}
		s := out.String()
		for _, want := range []string{
			"ENGINE", "ROLE", "UPTIME", "CPU", "MEM", // the engine header
			"leader", "1h2m3s", "4242", "2.5%", "150.0MiB", // the engine row
			"RUN", "PIPELINE", "LANE", "STATE", "EXIT", // the run header
			"extract", "running", "51.0%", "24.0MiB", // the running row
			"load_orders", "queued", // the queued row
		} {
			if !strings.Contains(s, want) {
				t.Errorf("human ps output missing %q:\n%s", want, s)
			}
		}
		// The queued run has no live process group: its load cells are dashes.
		for _, line := range strings.Split(s, "\n") {
			if strings.HasPrefix(line, "8") && !strings.Contains(line, "-") {
				t.Errorf("queued run row carries no dash for its absent load: %q", line)
			}
		}
	})

	t.Run("-q prints run ids only", func(t *testing.T) {
		sock := shortSocket(t)
		startPsDaemon(t, sock, psFunc(func(context.Context, bool) (api.PsPayload, error) {
			return psFixture(), nil
		}))

		var out, errb bytes.Buffer
		code := newApp(&out, &errb).run([]string{"--socket", sock, "ps", "-q"})
		if code != exitOK {
			t.Fatalf("ps -q exit = %d, want %d\nstderr: %s", code, exitOK, errb.String())
		}
		if got := out.String(); got != "8\n7\n" {
			t.Errorf("ps -q output = %q, want the run ids one per line", got)
		}
	})

	t.Run("-a widens the request to the whole history", func(t *testing.T) {
		sock := shortSocket(t)
		var sawAll atomic.Bool
		startPsDaemon(t, sock, psFunc(func(_ context.Context, all bool) (api.PsPayload, error) {
			sawAll.Store(all)
			return psFixture(), nil
		}))

		var out, errb bytes.Buffer
		if code := newApp(&out, &errb).run([]string{"--socket", sock, "ps"}); code != exitOK {
			t.Fatalf("ps exit = %d, want %d\nstderr: %s", code, exitOK, errb.String())
		}
		if sawAll.Load() {
			t.Error("bare ps asked the daemon for all=true, want false")
		}
		if code := newApp(&out, &errb).run([]string{"--socket", sock, "ps", "-a"}); code != exitOK {
			t.Fatalf("ps -a exit = %d, want %d\nstderr: %s", code, exitOK, errb.String())
		}
		if !sawAll.Load() {
			t.Error("ps -a asked the daemon for all=false, want true")
		}
	})

	t.Run("a null load renders dashes, never zeros", func(t *testing.T) {
		sock := shortSocket(t)
		startPsDaemon(t, sock, psFunc(func(context.Context, bool) (api.PsPayload, error) {
			p := psFixture()
			p.Engine.Load = nil // the host probe failed
			return p, nil
		}))

		var out, errb bytes.Buffer
		if code := newApp(&out, &errb).run([]string{"--socket", sock, "ps"}); code != exitOK {
			t.Fatalf("ps exit = %d, want %d\nstderr: %s", code, exitOK, errb.String())
		}
		engineRow := strings.Split(out.String(), "\n")[1]
		if strings.Contains(engineRow, "0.0%") || strings.Contains(engineRow, "0B") {
			t.Errorf("unprobed engine load rendered as zeros: %q", engineRow)
		}
		if !strings.Contains(engineRow, "-") {
			t.Errorf("unprobed engine load rendered no dash: %q", engineRow)
		}
	})

	t.Run("no daemon reachable exits 3", func(t *testing.T) {
		sock := shortSocket(t) // nothing listening
		var out, errb bytes.Buffer
		code := newApp(&out, &errb).run([]string{"--socket", sock, "ps"})
		if code != exitNoDaemon {
			t.Fatalf("no-daemon ps exit = %d, want %d\nstderr: %s", code, exitNoDaemon, errb.String())
		}
	})
}
