package cli

import (
	"bytes"
	"context"
	"net"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/MateusAMP2119/iris-engine-cli/internal/api"
	"github.com/MateusAMP2119/iris-engine-cli/internal/daemon"
	"github.com/MateusAMP2119/iris-engine-cli/internal/store"
	"github.com/MateusAMP2119/iris-engine-cli/internal/store/storetest"
)

// startWorkloadDaemon stands up an in-process daemon over unix socket serving
// the REAL api mux with the real daemon workload plane over the meta-store fake,
// so `iris workload show` reads through GET /workload with no live Postgres.
func startWorkloadDaemon(t *testing.T, sock string, reader store.ShowReader) {
	t.Helper()
	ln, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatalf("listen unix %s: %v", sock, err)
	}
	srv := &http.Server{
		Handler:           api.NewMux(api.WithWorkloadShow(daemon.NewWorkloadPlane(reader, nil))),
		ReadHeaderTimeout: 5 * time.Second,
	}
	go func() { _ = srv.Serve(ln) }()
	t.Cleanup(func() { _ = srv.Shutdown(context.Background()) })
}

// seededWorkloadFake seeds a small wiring: one lane "ingest" with order
// extract, reset, load; load depends_on extract; modes and some runs for tips
// and gates. Used for panel structure and zoom tests.
func seededWorkloadFake(t *testing.T) *storetest.ShowFake {
	t.Helper()
	f := storetest.NewShow()

	// Register details with modes.
	f.SetDetail("extract_orders", store.PipelineDetail{
		Folder:   "pipelines/ingest/extract_orders",
		Run:      []string{"python", "main.py"},
		Artifact: store.ArtifactSource,
		DataMode: store.DataDisposable,
	})
	f.SetDetail("reset_counters", store.PipelineDetail{
		Folder:   "pipelines/ingest/reset_counters",
		Run:      []string{"python", "reset.py"},
		Artifact: store.ArtifactSource,
		DataMode: store.DataDisposable,
	})
	f.SetDetail("load_orders", store.PipelineDetail{
		Folder:   "pipelines/ingest/load_orders",
		Run:      []string{"python", "load.py"},
		Artifact: store.ArtifactBuilt,
		DataMode: store.DataPermanent,
	})

	// Lane rows for composer walk.
	f.SeedLaneRows(
		store.LaneEntry{Lane: "ingest", Pipeline: "extract_orders", Pos: 0},
		store.LaneEntry{Lane: "ingest", Pipeline: "reset_counters", Pos: 1},
		store.LaneEntry{Lane: "ingest", Pipeline: "load_orders", Pos: 2},
	)
	f.SeedRegistered("extract_orders", "reset_counters", "load_orders")

	// Depends_on edge.
	f.AddEdge("load_orders", "extract_orders")

	// Latest runs and consumed for gate.
	f.SetLatestRun("extract_orders", store.LatestRunInfo{ID: 41, State: store.RunSucceeded})
	f.SetLatestRun("reset_counters", store.LatestRunInfo{ID: 99, State: store.RunSucceeded})
	f.SetLatestRun("load_orders", store.LatestRunInfo{ID: 7, State: store.RunSucceeded})
	f.SetConsumed("load_orders", 41)

	return f
}

// TestWorkloadShowWiringPanel claims the integration contract for the wiring
// panel: renders lanes+composer walk+modes+run tips+per-edge gate state as
// panel (not commit graph); optional pipeline zooms neighborhood. Uses
// in-proc daemon + store fake (no live PG).
func TestWorkloadShowWiringPanel(t *testing.T) {
	t.Setenv("IRIS_HOST", "")
	t.Setenv("IRIS_SOCKET", "")
	t.Setenv("IRIS_TOKEN", "")

	// spec: S08/workload-show-wiring-panel
	t.Run("S08/workload-show-wiring-panel", func(t *testing.T) {
		sock := shortSocket(t)
		f := seededWorkloadFake(t)
		startWorkloadDaemon(t, sock, f)

		// --json full panel structure.
		var out, errb bytes.Buffer
		code := newApp(&out, &errb).run([]string{"--socket", sock, "workload", "show", "--json"})
		if code != exitOK {
			t.Fatalf("workload show exit = %d, want %d\nstderr: %s", code, exitOK, errb.String())
		}
		var doc struct {
			Data api.WorkloadShowResult `json:"data"`
		}
		decodeSingleJSON(t, out.Bytes(), &doc)
		d := doc.Data

		if len(d.Lanes) != 1 {
			t.Fatalf("panel lanes = %d, want 1", len(d.Lanes))
		}
		lane := d.Lanes[0]
		if lane.Name != "ingest" {
			t.Errorf("lane name = %q, want ingest", lane.Name)
		}
		if len(lane.Pipelines) != 3 {
			t.Fatalf("lane pipelines = %d, want composer walk of 3", len(lane.Pipelines))
		}
		// Check order from composer.
		if lane.Pipelines[0].Name != "extract_orders" || lane.Pipelines[2].Name != "load_orders" {
			t.Errorf("composer walk = %v, want extract,reset,load order", names(lane.Pipelines))
		}
		// Modes present.
		p0 := lane.Pipelines[0]
		if p0.Artifact != string(store.ArtifactSource) || p0.DataMode != string(store.DataDisposable) {
			t.Errorf("extract modes = %s/%s, want source/disposable", p0.Artifact, p0.DataMode)
		}
		p2 := lane.Pipelines[2]
		if p2.Artifact != string(store.ArtifactBuilt) || p2.DataMode != string(store.DataPermanent) {
			t.Errorf("load modes = %s/%s, want built/permanent", p2.Artifact, p2.DataMode)
		}
		// Run tips present (non empty or "none").
		if lane.Pipelines[0].RunTip == "" {
			t.Errorf("extract run_tip missing")
		}
		// Per-edge gate state for load.
		if len(p2.Gate) != 1 || p2.Gate[0].Upstream != "extract_orders" {
			t.Fatalf("load gate = %+v, want one edge to extract", p2.Gate)
		}
		if p2.Gate[0].Verdict != "up_to_date" {
			t.Errorf("gate verdict = %q, want up_to_date (consumed)", p2.Gate[0].Verdict)
		}

		// Human panel carries structure tokens.
		var human, herr bytes.Buffer
		code = newApp(&human, &herr).run([]string{"--socket", sock, "workload", "show"})
		if code != exitOK {
			t.Fatalf("human workload show exit = %d, want %d\nstderr: %s", code, exitOK, herr.String())
		}
		h := human.String()
		for _, tok := range []string{"lane: ingest", "extract_orders", "source/disposable", "built/permanent", "gate:", "up_to_date"} {
			if !strings.Contains(h, tok) {
				t.Errorf("human panel missing %q:\n%s", tok, h)
			}
		}
		// Never looks like commit graph (no rail chars in basic check).
		if strings.Contains(h, "│") || strings.Contains(h, "─") {
			t.Errorf("human panel appears to contain rail graph chars; must be lanes panel not lineage")
		}

		// Zoom to named pipeline returns neighborhood (its lane here).
		var zout, zerr bytes.Buffer
		code = newApp(&zout, &zerr).run([]string{"--socket", sock, "workload", "show", "load_orders", "--json"})
		if code != exitOK {
			t.Fatalf("zoomed workload show exit = %d, want %d\nstderr: %s", code, exitOK, zerr.String())
		}
		var zdoc struct {
			Data api.WorkloadShowResult `json:"data"`
		}
		decodeSingleJSON(t, zout.Bytes(), &zdoc)
		zd := zdoc.Data
		if len(zd.Lanes) != 1 || zd.Lanes[0].Name != "ingest" {
			t.Errorf("zoom to load_orders lanes = %+v, want the neighborhood lane", zd.Lanes)
		}
		found := false
		for _, p := range zd.Lanes[0].Pipelines {
			if p.Name == "load_orders" {
				found = true
			}
		}
		if !found {
			t.Errorf("zoomed panel misses the named pipeline")
		}
	})
}

func names(ps []api.PipelineWiring) []string {
	var n []string
	for _, p := range ps {
		n = append(n, p.Name)
	}
	return n
}
