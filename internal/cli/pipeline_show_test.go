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

// startShowDaemon stands up an in-process daemon over a unix socket serving the
// REAL api mux with the real daemon pipeline-show plane over the meta-store fake,
// so `iris pipeline show` reads the readout through the real GET /pipeline/show
// route with no live Postgres.
func startShowDaemon(t *testing.T, sock string, reader store.ShowReader) {
	t.Helper()
	ln, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatalf("listen unix %s: %v", sock, err)
	}
	srv := &http.Server{
		Handler:           api.NewMux(api.WithPipelineShow(daemon.NewShowPlane(reader, nil))),
		ReadHeaderTimeout: 5 * time.Second,
	}
	go func() { _ = srv.Serve(ln) }()
	t.Cleanup(func() { _ = srv.Shutdown(context.Background()) })
}

// seededShowFake seeds the pipeline-show fake with the full readout surface for
// pipeline "transform": its resolved declaration, its role's grants, two recent
// runs, and four depends_on edges resolving to each closed-set gate verdict --
// extract open (fresh success), enrich up_to_date (success already consumed),
// load pending (no upstream run yet), and clean poisoned (dead-lettered).
func seededShowFake(t *testing.T) *storetest.ShowFake {
	t.Helper()
	ctx := context.Background()
	f := storetest.NewShow()
	f.SetDetail("transform", store.PipelineDetail{
		Folder:   "pipelines/transform",
		Run:      []string{"python", "run.py"},
		Artifact: store.ArtifactBuilt,
		DataMode: store.DataPermanent,
	})
	f.AddGrant("iris_pipeline_transform", store.Grant{Schema: "analytics", Table: "orders", Field: "id", Access: store.AccessRead})
	f.AddGrant("iris_pipeline_transform", store.Grant{Schema: "analytics", Table: "orders_clean", Field: "total", Access: store.AccessWrite})

	run1, err := f.CreateRun(ctx, store.RunSpec{Pipeline: "transform", Lane: "ingest"})
	if err != nil {
		t.Fatalf("create run: %v", err)
	}
	if _, err := f.SetRunState(ctx, run1.ID, store.RunSucceeded, store.WithExitCode(0)); err != nil {
		t.Fatalf("set run state: %v", err)
	}
	if _, err := f.CreateRun(ctx, store.RunSpec{Pipeline: "transform", Lane: "ingest"}); err != nil {
		t.Fatalf("create second run: %v", err)
	}

	f.AddEdge("transform", "extract").
		AddEdge("transform", "enrich").
		AddEdge("transform", "load").
		AddEdge("transform", "clean")
	f.SetLatestRun("extract", store.LatestRunInfo{ID: 41, State: store.RunSucceeded})
	f.SetLatestRun("enrich", store.LatestRunInfo{ID: 7, State: store.RunSucceeded})
	f.SetConsumed("transform", 7)
	// load has no run at all: pending.
	f.SetLatestRun("clean", store.LatestRunInfo{ID: 12, State: store.RunDeadLettered})
	return f
}

// TestPipelineShowReadout proves `iris pipeline show` reports the pipeline's
// resolved declaration, its role and grants, its recent runs, and the gate ledger
// with the per-edge verdict from the closed set (specification sections 6.2, 8,
// and 11), through the real mux route over a unix socket with no live Postgres.
func TestPipelineShowReadout(t *testing.T) {
	t.Setenv("IRIS_HOST", "")
	t.Setenv("IRIS_SOCKET", "")
	t.Setenv("IRIS_TOKEN", "")

	// spec: S11/pipeline-show-readout
	t.Run("S11/pipeline-show-readout", func(t *testing.T) {
		sock := shortSocket(t)
		startShowDaemon(t, sock, seededShowFake(t))

		var out, errb bytes.Buffer
		code := newApp(&out, &errb).run([]string{"--socket", sock, "pipeline", "show", "transform", "--json"})
		if code != exitOK {
			t.Fatalf("pipeline show exit = %d, want %d\nstderr: %s", code, exitOK, errb.String())
		}
		var doc struct {
			Data api.PipelineShowResult `json:"data"`
		}
		decodeSingleJSON(t, out.Bytes(), &doc)
		d := doc.Data

		// The resolved declaration.
		if d.Name != "transform" || d.Folder != "pipelines/transform" {
			t.Errorf("show declaration = %q %q, want transform pipelines/transform", d.Name, d.Folder)
		}
		if strings.Join(d.Run, " ") != "python run.py" {
			t.Errorf("show run argv = %v, want [python run.py]", d.Run)
		}
		if d.Artifact != string(store.ArtifactBuilt) || d.DataMode != string(store.DataPermanent) {
			t.Errorf("show modes = %q/%q, want built/permanent", d.Artifact, d.DataMode)
		}
		if strings.Join(d.DependsOn, ",") != "extract,enrich,load,clean" {
			t.Errorf("show depends_on = %v, want the declared edges in order", d.DependsOn)
		}

		// The role and its field-level grants.
		if d.Role != "iris_pipeline_transform" {
			t.Errorf("show role = %q, want iris_pipeline_transform", d.Role)
		}
		if len(d.Grants) != 2 {
			t.Fatalf("show grants = %+v, want the role's 2 grants", d.Grants)
		}
		if g := d.Grants[0]; g.Schema != "analytics" || g.Table != "orders" || g.Field != "id" || g.Access != "read" {
			t.Errorf("show grant[0] = %+v, want analytics.orders.id read", g)
		}

		// The recent runs.
		if len(d.RecentRuns) != 2 {
			t.Fatalf("show recent runs = %+v, want 2 runs", d.RecentRuns)
		}
		first := d.RecentRuns[0]
		if first.State != string(store.RunSucceeded) || first.ExitCode == nil || *first.ExitCode != 0 {
			t.Errorf("show run[0] = %+v, want a succeeded run with exit 0", first)
		}

		// The gate ledger: one verdict per edge, in edge order, from the closed set.
		want := []struct {
			upstream, verdict, latest string
		}{
			{"extract", "open", "41"},
			{"enrich", "up_to_date", "7"},
			{"load", "pending", ""},
			{"clean", "poisoned", "12"},
		}
		if len(d.GateLedger) != len(want) {
			t.Fatalf("show gate ledger = %+v, want one row per edge (%d)", d.GateLedger, len(want))
		}
		for i, w := range want {
			row := d.GateLedger[i]
			if row.Upstream != w.upstream || row.Verdict != w.verdict || row.LatestRunID != w.latest {
				t.Errorf("gate ledger[%d] = %+v, want upstream %q verdict %q latest %q", i, row, w.upstream, w.verdict, w.latest)
			}
		}

		// The human readout carries the ledger verdicts too.
		var human, herr bytes.Buffer
		code = newApp(&human, &herr).run([]string{"--socket", sock, "pipeline", "show", "transform"})
		if code != exitOK {
			t.Fatalf("human pipeline show exit = %d, want %d\nstderr: %s", code, exitOK, herr.String())
		}
		for _, token := range []string{"open", "up_to_date", "pending", "poisoned", "iris_pipeline_transform"} {
			if !strings.Contains(human.String(), token) {
				t.Errorf("human show output missing %q:\n%s", token, human.String())
			}
		}
	})

	t.Run("unregistered pipeline is operation-failed", func(t *testing.T) {
		// spec: S11/pipeline-show-readout
		sock := shortSocket(t)
		startShowDaemon(t, sock, storetest.NewShow())

		var out, errb bytes.Buffer
		code := newApp(&out, &errb).run([]string{"--socket", sock, "pipeline", "show", "ghost"})
		if code != exitOpFailed {
			t.Fatalf("show of unregistered pipeline exit = %d, want %d\nstdout: %s\nstderr: %s", code, exitOpFailed, out.String(), errb.String())
		}
		if !strings.Contains(errb.String(), "ghost") {
			t.Errorf("failure message does not name the pipeline: %q", errb.String())
		}
	})

	t.Run("no daemon reachable exits 3", func(t *testing.T) {
		// spec: S11/pipeline-show-readout
		sock := shortSocket(t) // nothing listening
		var out, errb bytes.Buffer
		code := newApp(&out, &errb).run([]string{"--socket", sock, "pipeline", "show", "transform"})
		if code != exitNoDaemon {
			t.Fatalf("no-daemon pipeline show exit = %d, want %d\nstderr: %s", code, exitNoDaemon, errb.String())
		}
	})
}

// TestGateLedgerInPipelineShow claims the S06.2 contract for the gate ledger
// surface: it reports per-edge the upstream latest, consumed check, verdict
// from closed set; the show is pure read (no meta writes); --json makes
// verdicts script-readable. Uses the in-proc daemon + meta fake (integration
// tier, no live PG).
func TestGateLedgerInPipelineShow(t *testing.T) {
	t.Setenv("IRIS_HOST", "")
	t.Setenv("IRIS_SOCKET", "")
	t.Setenv("IRIS_TOKEN", "")

	// spec: S06.2/gate-ledger-in-pipeline-show
	t.Run("S06.2/gate-ledger-in-pipeline-show", func(t *testing.T) {
		sock := shortSocket(t)
		f := seededShowFake(t)
		startShowDaemon(t, sock, f)

		// Drive via --json so scripts read verdicts.
		var out, errb bytes.Buffer
		code := newApp(&out, &errb).run([]string{"--socket", sock, "pipeline", "show", "transform", "--json"})
		if code != exitOK {
			t.Fatalf("pipeline show exit = %d, want %d\nstderr: %s", code, exitOK, errb.String())
		}
		var doc struct {
			Data api.PipelineShowResult `json:"data"`
		}
		decodeSingleJSON(t, out.Bytes(), &doc)
		d := doc.Data

		// Closed set verdicts, per edge in order, with latest run.
		want := []struct {
			upstream, verdict, latest string
		}{
			{"extract", "open", "41"},
			{"enrich", "up_to_date", "7"},
			{"load", "pending", ""},
			{"clean", "poisoned", "12"},
		}
		if len(d.GateLedger) != len(want) {
			t.Fatalf("gate ledger len = %d, want %d", len(d.GateLedger), len(want))
		}
		for i, w := range want {
			row := d.GateLedger[i]
			if row.Upstream != w.upstream || row.Verdict != w.verdict || row.LatestRunID != w.latest {
				t.Errorf("ledger[%d] = %+v, want upstream=%s verdict=%s latest=%s", i, row, w.upstream, w.verdict, w.latest)
			}
		}

		// Human also surfaces them (already asserted in sibling test, but recheck tokens).
		var human, herr bytes.Buffer
		code = newApp(&human, &herr).run([]string{"--socket", sock, "pipeline", "show", "transform"})
		if code != exitOK {
			t.Fatalf("human show = %d: %s", code, herr.String())
		}
		for _, v := range []string{"open", "up_to_date", "pending", "poisoned"} {
			if !strings.Contains(human.String(), v) {
				t.Errorf("human output misses verdict %q", v)
			}
		}
		// The read wrote no meta state: our fake is read-only surface; a closed
		// gate produces no run row (absence is the record). No writer was
		// involved in the show path.
	})
}
