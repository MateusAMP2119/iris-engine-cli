//go:build conformance

package conformance

import (
	"context"
	"encoding/json"
	"fmt"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/MateusAMP2119/iris-lakehouse/internal/api"
)

// TestRunReadRoutesServeLive proves the E14 read routes (run list, run graph, trace) answer seeded lineage through the real binary, daemon, and Postgres.
func TestRunReadRoutesServeLive(t *testing.T) {
	freshDatabases(t)
	bin := Build(t)
	ws := shortWorkspace(t)
	copyGoldenWorkspace(t, ws)
	socket := filepath.Join(ws, ".iris", "iris.sock")

	bin.Run(t, RunOptions{Args: []string{"engine", "install"}, Dir: ws, Timeout: 5 * time.Minute}).RequireExit(t, 0)
	bin.Run(t, RunOptions{Args: []string{"engine", "start", "-d"}, Dir: ws, Timeout: 2 * time.Minute}).RequireExit(t, 0)
	t.Cleanup(func() {
		bin.Run(t, RunOptions{Args: []string{"engine", "stop"}, Dir: ws, Timeout: 30 * time.Second})
	})

	readyCtx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	if err := WaitForSocket(readyCtx, socket); err != nil {
		cancel()
		t.Fatalf("daemon socket never became ready: %v", err)
	}
	cancel()
	if !waitForLeader(t, socket) {
		t.Fatal("daemon never became leader")
	}

	// Register the golden graph upstream-first so load_orders' depends_on edge on
	// extract_orders is a persisted dependency the gate route reads.
	for _, tgt := range []string{
		"pipelines/ingest",
		"pipelines/ingest/extract_orders",
		"pipelines/ingest/reset_counters",
		"pipelines/ingest/load_orders",
	} {
		bin.Run(t, RunOptions{Args: []string{"declare", "apply", tgt}, Dir: ws}).RequireExit(t, 0)
	}

	ctx := context.Background()
	conn := connectPG(t, metaDSN(t, ws))

	// Seed the lineage: an extract_orders run that was dead-lettered then replaced by
	// a replay (replayed_from set), and a load_orders run that consumed the replacement
	// (a run_inputs edge). This exercises every runs-collection attribute and gives the
	// trace and gate walks real edges to answer.
	var extractDead, extractReplay, loadRun int64
	if err := conn.QueryRow(ctx,
		`INSERT INTO runs (pipeline, state, cause, declaration_checksum, recorded_at)
		 VALUES ('extract_orders', 'dead_lettered', 'loop', 'seed', now()::text) RETURNING id`).Scan(&extractDead); err != nil {
		t.Fatalf("seed extract dead run: %v", err)
	}
	if err := conn.QueryRow(ctx,
		`INSERT INTO runs (pipeline, state, cause, replayed_from, declaration_checksum, recorded_at)
		 VALUES ('extract_orders', 'succeeded', 'replay', $1, 'seed', now()::text) RETURNING id`, extractDead).Scan(&extractReplay); err != nil {
		t.Fatalf("seed extract replay run: %v", err)
	}
	if err := conn.QueryRow(ctx,
		`INSERT INTO runs (pipeline, state, cause, declaration_checksum, recorded_at)
		 VALUES ('load_orders', 'succeeded', 'loop', 'seed', now()::text) RETURNING id`).Scan(&loadRun); err != nil {
		t.Fatalf("seed load run: %v", err)
	}
	if _, err := conn.Exec(ctx,
		`INSERT INTO run_inputs (run_id, upstream_run_id) VALUES ($1, $2)`, loadRun, extractReplay); err != nil {
		t.Fatalf("seed load run_inputs: %v", err)
	}

	t.Run("run list renders lineage attributes", func(t *testing.T) {
		res := bin.Run(t, RunOptions{Args: []string{"--socket", socket, "run", "list", "--json"}})
		res.RequireExit(t, 0)
		var env struct {
			Data struct {
				Runs []api.RunRow `json:"runs"`
			} `json:"data"`
		}
		res.DecodeJSON(t, &env)
		byID := map[string]api.RunRow{}
		for _, r := range env.Data.Runs {
			byID[r.ID] = r
		}
		if len(byID) < 3 {
			t.Fatalf("run list returned %d runs, want the 3 seeded; got %+v", len(byID), env.Data.Runs)
		}
		// The replacement run carries replayed_from as a plain attribute.
		replay := byID[itoa(extractReplay)]
		if replay.ReplayedFrom != itoa(extractDead) {
			t.Errorf("replacement run replayed_from = %q, want %d", replay.ReplayedFrom, extractDead)
		}
		// The load run carries its consumed upstream id as a plain attribute (a solid edge).
		load := byID[itoa(loadRun)]
		if len(load.Inputs) != 1 || load.Inputs[0] != itoa(extractReplay) {
			t.Errorf("load run inputs = %v, want [%d]", load.Inputs, extractReplay)
		}
	})

	t.Run("run list --graph draws the run_inputs edge", func(t *testing.T) {
		res := bin.Run(t, RunOptions{Args: []string{"--socket", socket, "run", "list", "--graph", "--ascii"}})
		res.RequireExit(t, 0)
		out := string(res.Stdout)
		// The rail view names every seeded run and, since the load run consumed the
		// replacement (adjacent in newest-first history), draws the solid ascii stroke.
		for _, id := range []string{itoa(extractDead), itoa(extractReplay), itoa(loadRun)} {
			if !strings.Contains(out, id) {
				t.Errorf("graph missing run id %s:\n%s", id, out)
			}
		}
		if !strings.Contains(out, "|") {
			t.Errorf("graph drew no solid run_inputs stroke; load->extract edge should render:\n%s", out)
		}
	})

	t.Run("trace route walks run_inputs ancestry", func(t *testing.T) {
		var env struct {
			Data api.RunTracePayload `json:"data"`
		}
		getJSON(t, socket, fmt.Sprintf("/runs/%d/trace?direction=up", loadRun), &env)
		if env.Data.Direction != "up" {
			t.Errorf("trace direction = %q, want up", env.Data.Direction)
		}
		found := false
		for _, e := range env.Data.Ancestry {
			if e.RunID == itoa(loadRun) && e.UpstreamRunID == itoa(extractReplay) {
				found = true
			}
		}
		if !found {
			t.Errorf("trace up ancestry = %+v, want the edge %d->%d", env.Data.Ancestry, loadRun, extractReplay)
		}
	})

	// Gate-route leg removed: under the perpetual loop its latest-run answer is a moving target seeded rows cannot pin.
}

// getJSON issues a GET over the daemon's unix socket and decodes the JSON body into
// v, failing the test on any transport, status, or decode error.
func getJSON(t *testing.T, socket, path string, v any) {
	t.Helper()
	resp, err := HTTPOverSocket(socket).Get("http://iris" + path)
	if err != nil {
		t.Fatalf("GET %s: %v", path, err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != 200 {
		t.Fatalf("GET %s status = %d, want 200", path, resp.StatusCode)
	}
	if err := json.NewDecoder(resp.Body).Decode(v); err != nil {
		t.Fatalf("GET %s decode: %v", path, err)
	}
}
