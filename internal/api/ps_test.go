package api_test

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/MateusAMP2119/iris-lakehouse/internal/api"
)

// leaderMux builds a mux whose role reporter answers leader, the shape every
// wired-route test drives (reads work anywhere, but the leader mux exercises
// the full matrix).
func leaderMux(opts ...api.MuxOption) http.Handler {
	role := api.NewRoleState()
	role.SetLeader()
	return api.NewMux(append([]api.MuxOption{api.WithRole(role)}, opts...)...)
}

// fixedPs is a PsHandler serving a canned payload, recording the all and
// history flags it was asked for.
type fixedPs struct {
	payload     api.PsPayload
	err         error
	lastAll     *bool
	lastHistory *bool
}

func (f *fixedPs) Ps(_ context.Context, all, history bool) (api.PsPayload, error) {
	if f.lastAll != nil {
		*f.lastAll = all
	}
	if f.lastHistory != nil {
		*f.lastHistory = history
	}
	return f.payload, f.err
}

// psEnvelope decodes a /ps response envelope.
type psEnvelope struct {
	Data  *api.PsPayload `json:"data"`
	Error *struct {
		Code    string `json:"code"`
		Message string `json:"message"`
	} `json:"error"`
}

// getPs drives one request against the mux and decodes the envelope.
func getPs(t *testing.T, h http.Handler, method, target string) (*http.Response, psEnvelope) {
	t.Helper()
	req := httptest.NewRequest(method, target, nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	resp := rec.Result()
	defer resp.Body.Close()
	var env psEnvelope
	if err := json.NewDecoder(resp.Body).Decode(&env); err != nil {
		t.Fatalf("decode %s %s: %v", method, target, err)
	}
	return resp, env
}

// TestPsRoute proves GET /ps serves the wired handler's payload in the data
// envelope, resolves the ?all parameter, rejects unknown parameters and
// non-GET methods with the closed envelope, and answers the internal-fault
// envelope while unwired or when the handler fails.
func TestPsRoute(t *testing.T) {
	payload := api.PsPayload{
		Engine: api.PsEngine{Version: "dev", Role: "leader", PID: 42, Uptime: "1s",
			QueuedRuns: 1, RunningRuns: 1, Load: &api.PsLoad{CPUPercent: 2.5, RSSBytes: 1 << 20}},
		Runs: []api.PsRun{{ID: "7", Pipeline: "extract", Lane: "ingest", State: "running",
			Load: &api.PsLoad{CPUPercent: 50, RSSBytes: 2 << 20}}},
	}

	t.Run("serves the wired payload, default all=false", func(t *testing.T) {
		var all bool
		h := leaderMux(api.WithPs(&fixedPs{payload: payload, lastAll: &all}))
		resp, env := getPs(t, h, http.MethodGet, "/ps")
		if resp.StatusCode != http.StatusOK || env.Data == nil {
			t.Fatalf("GET /ps = %d (data %v), want 200 with a data envelope", resp.StatusCode, env.Data != nil)
		}
		if all {
			t.Error("bare GET /ps asked the handler for all=true, want false")
		}
		if env.Data.Engine.Version != "dev" || len(env.Data.Runs) != 1 || env.Data.Runs[0].ID != "7" {
			t.Errorf("payload did not round-trip: %+v", env.Data)
		}
		if env.Data.Engine.Load == nil || env.Data.Engine.Load.CPUPercent != 2.5 {
			t.Errorf("engine load did not round-trip: %+v", env.Data.Engine.Load)
		}
	})

	t.Run("?all=true widens the read", func(t *testing.T) {
		var all bool
		h := leaderMux(api.WithPs(&fixedPs{payload: payload, lastAll: &all}))
		if resp, _ := getPs(t, h, http.MethodGet, "/ps?all=true"); resp.StatusCode != http.StatusOK {
			t.Fatalf("GET /ps?all=true = %d, want 200", resp.StatusCode)
		}
		if !all {
			t.Error("GET /ps?all=true asked the handler for all=false, want true")
		}
	})

	t.Run("?history=1 asks the handler for history and round-trips it", func(t *testing.T) {
		var history bool
		withHistory := payload
		withHistory.SampleTick = 7
		withHistory.History = &api.PsHistory{
			FineIntervalSeconds: 2, CoarseIntervalSeconds: 60,
			Series: []api.PsSeries{{Key: "engine", CPU: []float64{api.PsHistoryNoSample, 2.5},
				RSS: []int64{0, 1 << 20}, CoarseCPU: []float64{2.5}, CoarseRSS: []int64{1 << 20}}},
		}
		h := leaderMux(api.WithPs(&fixedPs{payload: withHistory, lastHistory: &history}))
		resp, env := getPs(t, h, http.MethodGet, "/ps?history=1")
		if resp.StatusCode != http.StatusOK || env.Data == nil {
			t.Fatalf("GET /ps?history=1 = %d, want 200 with a data envelope", resp.StatusCode)
		}
		if !history {
			t.Error("GET /ps?history=1 asked the handler for history=false, want true")
		}
		if env.Data.SampleTick != 7 {
			t.Errorf("sample tick = %d, want 7", env.Data.SampleTick)
		}
		got := env.Data.History
		if got == nil || len(got.Series) != 1 || got.Series[0].Key != "engine" {
			t.Fatalf("history did not round-trip: %+v", got)
		}
		if got.Series[0].CPU[0] != api.PsHistoryNoSample || got.Series[0].CoarseCPU[0] != 2.5 {
			t.Errorf("series values did not round-trip: %+v", got.Series[0])
		}
	})

	t.Run("without ?history the handler is asked for none", func(t *testing.T) {
		var history bool
		h := leaderMux(api.WithPs(&fixedPs{payload: payload, lastHistory: &history}))
		if resp, _ := getPs(t, h, http.MethodGet, "/ps?all=true"); resp.StatusCode != http.StatusOK {
			t.Fatalf("GET /ps?all=true = %d, want 200", resp.StatusCode)
		}
		if history {
			t.Error("a history-less GET asked the handler for history=true, want false")
		}
	})

	t.Run("unknown parameter is bad_param", func(t *testing.T) {
		h := leaderMux(api.WithPs(&fixedPs{payload: payload}))
		resp, env := getPs(t, h, http.MethodGet, "/ps?bogus=1")
		if resp.StatusCode != http.StatusBadRequest || env.Error == nil || env.Error.Code != "bad_param" {
			t.Fatalf("GET /ps?bogus=1 = %d %+v, want 400 bad_param", resp.StatusCode, env.Error)
		}
	})

	t.Run("non-GET is method_not_allowed", func(t *testing.T) {
		h := leaderMux(api.WithPs(&fixedPs{payload: payload}))
		resp, env := getPs(t, h, http.MethodPost, "/ps")
		if resp.StatusCode != http.StatusMethodNotAllowed || env.Error == nil || env.Error.Code != "method_not_allowed" {
			t.Fatalf("POST /ps = %d %+v, want 405 method_not_allowed", resp.StatusCode, env.Error)
		}
	})

	t.Run("unwired handler is an internal fault, never an empty payload", func(t *testing.T) {
		resp, env := getPs(t, leaderMux(), http.MethodGet, "/ps")
		if resp.StatusCode != http.StatusInternalServerError || env.Error == nil || env.Error.Code != "internal" {
			t.Fatalf("unwired GET /ps = %d %+v, want 500 internal", resp.StatusCode, env.Error)
		}
	})

	t.Run("handler fault is 500 internal", func(t *testing.T) {
		h := leaderMux(api.WithPs(&fixedPs{err: errors.New("meta down")}))
		resp, env := getPs(t, h, http.MethodGet, "/ps")
		if resp.StatusCode != http.StatusInternalServerError || env.Error == nil || env.Error.Code != "internal" {
			t.Fatalf("faulting GET /ps = %d %+v, want 500 internal", resp.StatusCode, env.Error)
		}
	})
}

// TestNoMetricsEndpoint proves core exposes no /metrics: the daemon mux answers
// a /metrics request with the closed not_found error envelope, never a metrics
// document -- with and without the ps handler wired (a monitor consumes GET
// /ps; /metrics is deliberately left out).
func TestNoMetricsEndpoint(t *testing.T) {
	t.Run("no-metrics-endpoint", func(t *testing.T) {
		muxes := map[string]http.Handler{
			"bare mux":        api.NewMux(),
			"ps-wired mux":    api.NewMux(api.WithPs(&fixedPs{})),
			"role-leader mux": leaderMux(),
		}
		for name, h := range muxes {
			resp, env := getPs(t, h, http.MethodGet, "/metrics")
			if resp.StatusCode != http.StatusNotFound || env.Error == nil || env.Error.Code != "not_found" {
				t.Errorf("%s: GET /metrics = %d %+v, want 404 not_found (no metrics endpoint in core)", name, resp.StatusCode, env.Error)
			}
		}
	})
}
