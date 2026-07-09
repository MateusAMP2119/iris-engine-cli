package api

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// fakeWorkload implements WorkloadShowHandler for the S07/workload-route integration test.
type fakeWorkload struct {
	pipeline string
}

func (f *fakeWorkload) ShowWorkload(ctx context.Context, pipeline string) (WorkloadShowResult, error) {
	f.pipeline = pipeline
	// Return a minimal wiring payload matching the contract description:
	// lanes, composer order, pipelines with modes and run tips (latest run), depends_on edges with gate state.
	return WorkloadShowResult{
		Lanes: []LaneWiring{
			{
				Name: "ingest",
				Pipelines: []PipelineWiring{
					{Name: "extract_orders", Folder: "pipelines/ingest/extract_orders", Artifact: "source", DataMode: "disposable", RunTip: "succeeded 41", Gate: nil},
					{Name: "load_orders", Folder: "pipelines/ingest/load_orders", Artifact: "built", DataMode: "permanent", RunTip: "succeeded 42", Gate: []EdgeVerdictView{{Upstream: "extract_orders", Verdict: "up_to_date", LatestRunID: "41"}}},
				},
			},
		},
	}, nil
}

// fakeRuns implements RunsHandler for tests claiming S07/runs-include-inputs.
type fakeRuns struct {
	include bool
}

func (f *fakeRuns) ListRuns(ctx context.Context, includeInputs bool) (any, error) {
	f.include = includeInputs
	if includeInputs {
		rf := "10"
		return []map[string]any{{"id": "42", "pipeline": "load", "state": "succeeded", "replayed_from": &rf, "inputs": []string{"39"}}}, nil
	}
	return []map[string]any{{"id": "42", "pipeline": "load", "state": "succeeded"}}, nil
}

func (f *fakeRuns) GetRun(ctx context.Context, id string, includeInputs bool) (any, error) {
	return map[string]any{"id": id}, nil
}

// fakeTrace implements RunTraceHandler.
type fakeTrace struct{}

func (fakeTrace) Trace(ctx context.Context, id, direction string) (any, error) {
	return map[string]any{"run": id, "direction": direction, "ancestry": []any{}}, nil
}

// fakeGate implements PipelineGateHandler.
type fakeGate struct{}

func (fakeGate) Gate(ctx context.Context, name string) (any, error) {
	return map[string]any{"pipeline": name, "gate": []any{}}, nil
}

// fakeImpact implements DeadImpactHandler.
type fakeImpact struct{}

func (fakeImpact) Impact(ctx context.Context, runID string) (any, error) {
	return map[string]any{"run": runID, "impact": []any{}}, nil
}

// TestS07ReadRoutes claims the S07 integration contracts for the read routes:
// workload (already wired), runs with inputs, trace/gate/impact.
func TestS07ReadRoutes(t *testing.T) {
	// spec: S07/runs-include-inputs
	t.Run("S07/runs-include-inputs", func(t *testing.T) {
		fr := &fakeRuns{}
		mux := NewMux(WithRuns(fr))
		req := httptest.NewRequest(http.MethodGet, "/runs?include=inputs", nil)
		rec := httptest.NewRecorder()
		mux.ServeHTTP(rec, req)
		if rec.Code != http.StatusOK {
			t.Fatalf("GET /runs?include=inputs status = %d, want 200; body=%s", rec.Code, rec.Body.String())
		}
		if !fr.include {
			t.Errorf("include=inputs was not passed to handler")
		}
		// Response should embed consumed upstream ids and replayed_from as plain row attributes (parents-per-row, never a separate edge array), per contract.
		body := rec.Body.String()
		if !strings.Contains(body, `"inputs"`) || !strings.Contains(body, `"replayed_from"`) {
			t.Errorf("runs response missing inputs/replayed_from attrs: %s", body)
		}
		if strings.Contains(body, `"edges"`) || strings.Contains(body, `"parents"`) {
			t.Errorf("runs must not use separate edge array, parents-per-row only: %s", body)
		}
		// Also streams as NDJSON like any collection route when requested.
		reqND := httptest.NewRequest(http.MethodGet, "/runs?include=inputs", nil)
		reqND.Header.Set("Accept", "application/x-ndjson")
		recND := httptest.NewRecorder()
		mux.ServeHTTP(recND, reqND)
		if recND.Code != http.StatusOK {
			t.Errorf("ndjson /runs status=%d", recND.Code)
		}
		if ct := recND.Header().Get("Content-Type"); ct != "application/x-ndjson" {
			t.Errorf("ndjson content-type=%q want application/x-ndjson", ct)
		}
		ndbody := recND.Body.String()
		if !strings.Contains(ndbody, `"inputs"`) {
			t.Errorf("ndjson runs missing inputs attr")
		}
		if strings.Contains(ndbody, "{") && strings.Contains(ndbody, `"data"`) {
			t.Errorf("ndjson must have no envelope")
		}
	})

	// spec: S07/trace-gate-impact-routes
	t.Run("S07/trace-gate-impact-routes", func(t *testing.T) {
		mux := NewMux(WithRunTrace(fakeTrace{}), WithPipelineGate(fakeGate{}), WithDeadImpact(fakeImpact{}))

		for _, path := range []string{"/runs/99/trace?direction=up", "/pipelines/load/gate", "/dead_letters/99/impact"} {
			req := httptest.NewRequest(http.MethodGet, path, nil)
			rec := httptest.NewRecorder()
			mux.ServeHTTP(rec, req)
			if rec.Code != http.StatusOK {
				t.Errorf("%s status = %d, want 200; body=%s", path, rec.Code, rec.Body.String())
			}
		}
	})

	// spec: S07/workload-route
	t.Run("S07/workload-route", func(t *testing.T) {
		fw := &fakeWorkload{}
		mux := NewMux(WithWorkloadShow(fw))

		// full wiring
		req := httptest.NewRequest(http.MethodGet, "/workload", nil)
		rec := httptest.NewRecorder()
		mux.ServeHTTP(rec, req)
		if rec.Code != http.StatusOK {
			t.Fatalf("GET /workload status=%d want 200; body=%s", rec.Code, rec.Body.String())
		}
		body := rec.Body.String()
		if !strings.Contains(body, `"lanes"`) || !strings.Contains(body, `"ingest"`) {
			t.Errorf("workload payload missing lanes structure: %s", body)
		}
		// payload must have lanes + pipelines with modes + run tips + gate edges per contract
		if !strings.Contains(body, "extract_orders") || !strings.Contains(body, "source") || !strings.Contains(body, "disposable") || !strings.Contains(body, "up_to_date") {
			t.Errorf("workload payload missing pipelines/modes/run_tip/gate state: %s", body)
		}
		if fw.pipeline != "" {
			t.Errorf("full /workload passed non-empty pipeline zoom %q", fw.pipeline)
		}

		// ?pipeline= zooms
		fw2 := &fakeWorkload{}
		mux2 := NewMux(WithWorkloadShow(fw2))
		req2 := httptest.NewRequest(http.MethodGet, "/workload?pipeline=load_orders", nil)
		rec2 := httptest.NewRecorder()
		mux2.ServeHTTP(rec2, req2)
		if rec2.Code != http.StatusOK {
			t.Fatalf("?pipeline zoom status=%d want 200", rec2.Code)
		}
		if fw2.pipeline != "load_orders" {
			t.Errorf("zoom did not pass pipeline name")
		}
	})
}
