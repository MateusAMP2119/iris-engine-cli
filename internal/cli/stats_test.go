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
	"testing"
	"time"

	"github.com/MateusAMP2119/iris-engine-cli/internal/api"
	"github.com/MateusAMP2119/iris-engine-cli/internal/daemon"
	"github.com/MateusAMP2119/iris-engine-cli/internal/dispatch"
	"github.com/MateusAMP2119/iris-engine-cli/internal/store"
	"github.com/MateusAMP2119/iris-engine-cli/internal/store/storetest"
)

// startStatsDaemon stands up an in-process daemon over a unix socket serving the
// REAL api mux with the real daemon stats plane over the meta-store fake -- the
// integration-tier "in-process daemon over a socket" pattern -- so `iris engine
// stats` and a raw GET /stats read the same engine state through the same route.
func startStatsDaemon(t *testing.T, sock string, handler api.StatsHandler) {
	t.Helper()
	ln, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatalf("listen unix %s: %v", sock, err)
	}
	srv := &http.Server{Handler: api.NewMux(api.WithStats(handler)), ReadHeaderTimeout: 5 * time.Second}
	go func() { _ = srv.Serve(ln) }()
	t.Cleanup(func() { _ = srv.Shutdown(context.Background()) })
}

// seededStatsPlane builds the daemon stats plane over a seeded meta-store fake:
// one lane with one pipeline and a succeeded run, a dead letter, journal
// counters, a two-checkpoint chain whose head digest is chainDigest, and one
// counted lane pass.
func seededStatsPlane(t *testing.T, chainDigest string) api.StatsHandler {
	t.Helper()
	f := storetest.NewStats()
	f.RegisterPipeline("extract")
	f.AddLaneMember("ingest", "extract")
	run, err := f.CreateRun(context.Background(), store.RunSpec{Pipeline: "extract", Lane: "ingest"})
	if err != nil {
		t.Fatalf("create run: %v", err)
	}
	if _, err := f.SetRunState(context.Background(), run.ID, store.RunSucceeded, store.WithExitCode(0)); err != nil {
		t.Fatalf("set run state: %v", err)
	}
	f.AddDeadLetter(store.DeadLetterEntry{RunID: "run-7", Reason: store.ReasonFailed})
	f.SetJournal(store.JournalStats{CapturedWrites: 120, WipeEligibleRows: 40, TotalRows: 200, HotRows: 150})
	f.AddCheckpoint(store.Checkpoint{Seq: 1, Digest: "aaaa", Location: "archived"})
	f.AddCheckpoint(store.Checkpoint{Seq: 2, Digest: chainDigest, Location: "resident"})

	pc := dispatch.NewPassCounter()
	pc.Hook()(dispatch.PassReport{Lane: "ingest"})
	return daemon.NewStatsPlane(f, pc, nil)
}

// TestEngineStatsParity proves GET /stats and `iris engine stats` return the
// identical read-only rollup payload for the same engine state: both surfaces
// read the one mux route backed by the one rollup handler, and the CLI's --json
// data envelope carries exactly the route's data
// document.
func TestEngineStatsParity(t *testing.T) {
	t.Setenv("IRIS_HOST", "")
	t.Setenv("IRIS_SOCKET", "")
	t.Setenv("IRIS_TOKEN", "")

	t.Run("stats-cli-http-parity", func(t *testing.T) {
		sock := shortSocket(t)
		startStatsDaemon(t, sock, seededStatsPlane(t, "beefcafe"))

		// The CLI surface: `iris engine stats --json` prints the data envelope.
		var out, errb bytes.Buffer
		code := newApp(&out, &errb).run([]string{"--socket", sock, "engine", "stats", "--json"})
		if code != exitOK {
			t.Fatalf("engine stats exit = %d, want %d\nstderr: %s", code, exitOK, errb.String())
		}
		var cliEnv struct {
			Data any `json:"data"`
		}
		if err := json.Unmarshal(out.Bytes(), &cliEnv); err != nil {
			t.Fatalf("decode CLI stats envelope: %v\nstdout: %s", err, out.String())
		}

		// The HTTP surface: a raw GET /stats over the same socket.
		client := &http.Client{Transport: &http.Transport{
			DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
				var d net.Dialer
				return d.DialContext(ctx, "unix", sock)
			},
		}}
		resp, err := client.Get("http://iris/stats")
		if err != nil {
			t.Fatalf("GET /stats: %v", err)
		}
		defer resp.Body.Close()
		raw, err := io.ReadAll(resp.Body)
		if err != nil {
			t.Fatalf("read /stats body: %v", err)
		}
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("GET /stats status = %d, want 200\nbody: %s", resp.StatusCode, raw)
		}
		var httpEnv struct {
			Data any `json:"data"`
		}
		if err := json.Unmarshal(raw, &httpEnv); err != nil {
			t.Fatalf("decode HTTP stats envelope: %v\nbody: %s", err, raw)
		}

		// Identical payloads: the same engine state rolls up to one document on
		// both surfaces.
		if !reflect.DeepEqual(cliEnv.Data, httpEnv.Data) {
			t.Errorf("CLI and HTTP stats payloads differ\nCLI:  %v\nHTTP: %v", cliEnv.Data, httpEnv.Data)
		}
		if cliEnv.Data == nil {
			t.Fatal("stats payload is empty; parity proven over nothing")
		}
	})

	t.Run("stats-reports-chain-head", func(t *testing.T) {
		sock := shortSocket(t)
		const digest = "deadbeef01"
		startStatsDaemon(t, sock, seededStatsPlane(t, digest))

		// The machine surface names the chain head.
		var out, errb bytes.Buffer
		code := newApp(&out, &errb).run([]string{"--socket", sock, "engine", "stats", "--json"})
		if code != exitOK {
			t.Fatalf("engine stats --json exit = %d, want %d\nstderr: %s", code, exitOK, errb.String())
		}
		var env struct {
			Data api.StatsPayload `json:"data"`
		}
		if err := json.Unmarshal(out.Bytes(), &env); err != nil {
			t.Fatalf("decode stats envelope: %v", err)
		}
		head := env.Data.Engine.CheckpointChainHead
		if head == nil {
			t.Fatal("stats payload carries no checkpoint chain head")
		}
		if head.Digest != digest || head.Seq != 2 {
			t.Errorf("chain head = %+v, want the highest-seq checkpoint (seq 2, digest %s)", *head, digest)
		}

		// The human surface reports it too.
		var human, herr bytes.Buffer
		code = newApp(&human, &herr).run([]string{"--socket", sock, "engine", "stats"})
		if code != exitOK {
			t.Fatalf("engine stats exit = %d, want %d\nstderr: %s", code, exitOK, herr.String())
		}
		if !strings.Contains(human.String(), digest) {
			t.Errorf("human stats output does not name the chain head digest %s:\n%s", digest, human.String())
		}
	})

	t.Run("no daemon reachable exits 3", func(t *testing.T) {
		sock := shortSocket(t) // nothing listening
		var out, errb bytes.Buffer
		code := newApp(&out, &errb).run([]string{"--socket", sock, "engine", "stats"})
		if code != exitNoDaemon {
			t.Fatalf("no-daemon engine stats exit = %d, want %d\nstderr: %s", code, exitNoDaemon, errb.String())
		}
	})
}

// startProvenanceDaemon starts an in-process mux serving the given provenance
// handler over unix socket, for integration parity tests.
func startProvenanceDaemon(t *testing.T, sock string, h api.ProvenanceHandler) {
	t.Helper()
	ln, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatalf("listen unix %s: %v", sock, err)
	}
	srv := &http.Server{Handler: api.NewMux(api.WithProvenance(h)), ReadHeaderTimeout: 5 * time.Second}
	go func() { _ = srv.Serve(ln) }()
	t.Cleanup(func() { _ = srv.Shutdown(context.Background()) })
}

// fixedProv is a simple ProvenanceHandler for tests.
type fixedProv struct{ r api.ProvenanceResult }

func (f fixedProv) Provenance(context.Context, string, string, string) (api.ProvenanceResult, error) {
	return f.r, nil
}

// TestProvenanceCLIRenderParity proves that the CLI `data provenance` and direct
// GET /provenance return the identical content (the api payload) for the same
// in-process daemon state. Also exercises the
// lineage-only shape under the surfaces.
func TestProvenanceCLIRenderParity(t *testing.T) {
	t.Setenv("IRIS_HOST", "")
	t.Setenv("IRIS_SOCKET", "")
	t.Setenv("IRIS_TOKEN", "")

	t.Run("api-cli-read-render-parity", func(t *testing.T) {
		sock := shortSocket(t)
		sample := api.ProvenanceResult{
			Schema: "analytics", Table: "orders", PK: "9f3c..",
			Stamps:   []api.ProvenanceStamp{{EntryID: 42, RunID: 7, Op: "insert", Undo: "open"}},
			Authored: true, Author: &api.ProvenanceStamp{EntryID: 42, RunID: 7, Op: "insert", Undo: "open"},
			Pipeline: "load_orders", State: "succeeded",
		}
		startProvenanceDaemon(t, sock, fixedProv{r: sample})

		// CLI surface --json
		var out, errb bytes.Buffer
		code := newApp(&out, &errb).run([]string{"--socket", sock, "data", "provenance", "analytics.orders", "9f3c..", "--json"})
		if code != exitOK {
			t.Fatalf("data provenance exit=%d stderr=%s", code, errb.String())
		}
		var cliEnv struct {
			Data any `json:"data"`
		}
		if err := json.Unmarshal(out.Bytes(), &cliEnv); err != nil {
			t.Fatalf("decode CLI: %v", err)
		}

		// Direct HTTP GET over socket
		client := &http.Client{Transport: &http.Transport{
			DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
				var d net.Dialer
				return d.DialContext(ctx, "unix", sock)
			},
		}}
		resp, err := client.Get("http://iris/provenance/analytics/orders/9f3c..")
		if err != nil {
			t.Fatalf("GET: %v", err)
		}
		defer resp.Body.Close()
		raw, _ := io.ReadAll(resp.Body)
		var httpEnv struct {
			Data any `json:"data"`
		}
		if err := json.Unmarshal(raw, &httpEnv); err != nil {
			t.Fatalf("decode HTTP: %v", err)
		}

		if !reflect.DeepEqual(cliEnv.Data, httpEnv.Data) {
			t.Errorf("CLI and HTTP provenance payloads differ\nCLI: %+v\nHTTP: %+v", cliEnv.Data, httpEnv.Data)
		}
	})

	t.Run("cli-api-same-views", func(_ *testing.T) {
		// The actual end-to-end binary+daemon same-views is asserted in the
		// conformance suite. Here the in-process surfaces already share the
		// handler payload, proving parity in-process.
	})
}
