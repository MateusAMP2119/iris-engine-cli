package api_test

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/MateusAMP2119/iris-lakehouse/internal/api"
	"github.com/MateusAMP2119/iris-lakehouse/internal/declare"
)

// cannedPromoteHandler is a canned api.PromoteHandler: the daemon-side promote
// outcome the route under test renders.
type cannedPromoteHandler struct {
	res api.PipelinePromoteResult
	err error
}

func (h cannedPromoteHandler) PromotePipeline(context.Context, api.PipelinePromoteRequest) (api.PipelinePromoteResult, error) {
	return h.res, h.err
}

// postPromote drives POST /pipeline/promote on a leader mux wired to h.
func postPromote(t *testing.T, h api.PromoteHandler, body []byte) *httptest.ResponseRecorder {
	t.Helper()
	role := api.NewRoleState()
	role.SetLeader()
	mux := api.NewMux(api.WithRole(role), api.WithPromote(h))
	req := httptest.NewRequest(http.MethodPost, "/pipeline/promote", bytes.NewReader(body))
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	return rec
}

// TestPromoteRouteRefusesUnbuilt proves POST /pipeline/promote renders the
// built gate's refusal as an operation failure: a promote refused because the
// pipeline is not in built state is 422 operation_failed carrying the daemon's
// reason, never a silent success.
func TestPromoteRouteRefusesUnbuilt(t *testing.T) {
	body, err := json.Marshal(api.PipelinePromoteRequest{Pipeline: "etl"})
	if err != nil {
		t.Fatalf("marshal request: %v", err)
	}
	refusal := `dispatch: promote "etl" refused: pipeline is not in built state`
	rec := postPromote(t, cannedPromoteHandler{err: errSentinel(refusal)}, body)

	if rec.Code != http.StatusUnprocessableEntity {
		t.Errorf("refusal status = %d, want %d (operation failed)", rec.Code, http.StatusUnprocessableEntity)
	}
	var env api.ErrorEnvelope
	if err := json.NewDecoder(rec.Body).Decode(&env); err != nil {
		t.Fatalf("decode error envelope: %v", err)
	}
	if env.Error.Code != api.CodeOpFailed {
		t.Errorf("refusal code = %q, want %q", env.Error.Code, api.CodeOpFailed)
	}
	if !strings.Contains(env.Error.Message, "built") {
		t.Errorf("refusal message does not name the built requirement: %q", env.Error.Message)
	}
}

// TestPromoteRouteReportsFlipAndWarnings proves a successful POST
// /pipeline/promote reports the flipped data mode and carries any repeated
// cross-mode read warnings in the data envelope (the warning rides the JSON
// surface, promote repeats it while the upstream stays disposable).
func TestPromoteRouteReportsFlipAndWarnings(t *testing.T) {
	body, err := json.Marshal(api.PipelinePromoteRequest{Pipeline: "etl"})
	if err != nil {
		t.Fatalf("marshal request: %v", err)
	}
	rec := postPromote(t, cannedPromoteHandler{res: api.PipelinePromoteResult{
		Pipeline: "etl",
		DataMode: "permanent",
		Warnings: []declare.Warning{{
			Kind:    declare.WarnCrossModeRead,
			Table:   "raw_orders",
			Message: "permanent-data pipeline reads disposable-mode upstream raw_orders",
		}},
	}}, body)

	if rec.Code != http.StatusOK {
		t.Fatalf("promote status = %d, want %d\nbody: %s", rec.Code, http.StatusOK, rec.Body.String())
	}
	var env struct {
		Data api.PipelinePromoteResult `json:"data"`
	}
	if err := json.NewDecoder(rec.Body).Decode(&env); err != nil {
		t.Fatalf("decode data envelope: %v", err)
	}
	if env.Data.Pipeline != "etl" || env.Data.DataMode != "permanent" {
		t.Errorf("promote result = %+v, want pipeline etl with data mode permanent", env.Data)
	}
	if len(env.Data.Warnings) != 1 || env.Data.Warnings[0].Kind != declare.WarnCrossModeRead {
		t.Errorf("promote result does not carry the cross-mode read warning: %+v", env.Data.Warnings)
	}
}

// errSentinel is a plain error with a fixed message.
type errSentinel string

func (e errSentinel) Error() string { return string(e) }
