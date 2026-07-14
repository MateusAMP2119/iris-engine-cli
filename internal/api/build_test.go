package api_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/MateusAMP2119/iris-lakehouse/internal/api"
)

// errBuildHandler is a canned api.BuildHandler that always fails with a fixed
// error, so a test can pin how the build route classifies each failure kind.
type errBuildHandler struct{ err error }

func (h errBuildHandler) BuildPipeline(context.Context, api.PipelineBuildRequest) (api.PipelineBuildResult, error) {
	return api.PipelineBuildResult{}, h.err
}

// TestBuildRouteDistinguishesCancellation proves POST /pipeline/build tells a
// request-context cancellation apart from a genuine build failure: a real
// failure is operation_failed (422), while a caller that cancels or times out
// mid-build gets a distinct status and error code, so a client or proxy never
// mistakes an aborted request for an engine-side build error.
func TestBuildRouteDistinguishesCancellation(t *testing.T) {
	do := func(h api.BuildHandler) (int, api.ErrorBody) {
		t.Helper()
		role := api.NewRoleState()
		role.SetLeader()
		mux := api.NewMux(api.WithRole(role), api.WithBuild(h))
		body, err := json.Marshal(api.PipelineBuildRequest{Pipeline: "etl"})
		if err != nil {
			t.Fatalf("marshal request: %v", err)
		}
		req := httptest.NewRequest(http.MethodPost, "/pipeline/build", bytes.NewReader(body))
		rec := httptest.NewRecorder()
		mux.ServeHTTP(rec, req)
		var env api.ErrorEnvelope
		if err := json.NewDecoder(rec.Body).Decode(&env); err != nil {
			t.Fatalf("decode error envelope: %v", err)
		}
		return rec.Code, env.Error
	}

	// A genuine build failure is operation_failed (422).
	failStatus, failBody := do(errBuildHandler{err: errors.New(`unsupported runtime "ruby": no pinned build recipe`)})
	if failStatus != http.StatusUnprocessableEntity {
		t.Errorf("build-failure status = %d, want %d (operation failed)", failStatus, http.StatusUnprocessableEntity)
	}
	if failBody.Code != api.CodeOpFailed {
		t.Errorf("build-failure code = %q, want %q", failBody.Code, api.CodeOpFailed)
	}

	// A cancellation and a deadline are each a distinct outcome, never operation_failed.
	for _, tc := range []struct {
		name string
		err  error
	}{
		{"canceled", context.Canceled},
		{"deadline", context.DeadlineExceeded},
	} {
		t.Run(tc.name, func(t *testing.T) {
			status, body := do(errBuildHandler{err: tc.err})
			if body.Code == failBody.Code {
				t.Errorf("%s code %q is indistinguishable from a build failure", tc.name, body.Code)
			}
			if status == http.StatusUnprocessableEntity {
				t.Errorf("%s status %d is indistinguishable from a build failure (422)", tc.name, status)
			}
			if body.Code != api.CodeCanceled {
				t.Errorf("%s code = %q, want %q", tc.name, body.Code, api.CodeCanceled)
			}
		})
	}
}
