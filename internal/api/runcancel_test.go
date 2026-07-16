package api_test

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/MateusAMP2119/iris-lakehouse/internal/api"
)

// recordCancel is a RunCancelHandler recording what it was asked to stop.
type recordCancel struct {
	run, pipeline string
	err           error
}

func (r *recordCancel) CancelRun(_ context.Context, run string) error {
	r.run = run
	return r.err
}

func (r *recordCancel) CancelPipeline(_ context.Context, pipeline string) (string, error) {
	r.pipeline = pipeline
	if r.err != nil {
		return "", r.err
	}
	return "77", nil
}

// postCancel POSTs one body to /run/cancel on a leader mux over h.
func postCancel(t *testing.T, h *recordCancel, body string) *httptest.ResponseRecorder {
	t.Helper()
	mux := leaderMux(api.WithRunCancel(h))
	req := httptest.NewRequest(http.MethodPost, "/run/cancel", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)
	return w
}

// TestRunCancelRoute proves POST /run/cancel accepts exactly one of run or pipeline: a run id cancels that run, a pipeline parks its latest run and returns the resolved id (#202), both-or-neither is 400, and a handler failure is 422.
func TestRunCancelRoute(t *testing.T) {
	t.Run("run-cancel-route", func(t *testing.T) {
		t.Run("run id cancels that run", func(t *testing.T) {
			h := &recordCancel{}
			w := postCancel(t, h, `{"run":"12"}`)
			if w.Code != http.StatusOK || h.run != "12" || h.pipeline != "" {
				t.Fatalf("status %d, handler saw run=%q pipeline=%q", w.Code, h.run, h.pipeline)
			}
		})

		t.Run("pipeline parks its latest run and returns the id", func(t *testing.T) {
			h := &recordCancel{}
			w := postCancel(t, h, `{"pipeline":"hello_iris"}`)
			if w.Code != http.StatusOK || h.pipeline != "hello_iris" || h.run != "" {
				t.Fatalf("status %d, handler saw run=%q pipeline=%q", w.Code, h.run, h.pipeline)
			}
			var env struct {
				Data api.RunCancelResult `json:"data"`
			}
			if err := json.NewDecoder(w.Body).Decode(&env); err != nil {
				t.Fatalf("decode: %v", err)
			}
			if env.Data.Run != "77" || env.Data.State != "dead_lettered" {
				t.Errorf("result = %+v, want the resolved run 77 dead_lettered", env.Data)
			}
		})

		t.Run("neither and both are 400", func(t *testing.T) {
			for _, body := range []string{`{}`, `{"run":"1","pipeline":"p"}`} {
				h := &recordCancel{}
				if w := postCancel(t, h, body); w.Code != http.StatusBadRequest {
					t.Errorf("body %s: status = %d, want 400", body, w.Code)
				}
				if h.run != "" || h.pipeline != "" {
					t.Errorf("body %s reached the handler", body)
				}
			}
		})

		t.Run("handler failure is 422 with the message", func(t *testing.T) {
			h := &recordCancel{err: errors.New("nothing to stop")}
			w := postCancel(t, h, `{"pipeline":"idle"}`)
			if w.Code != http.StatusUnprocessableEntity || !strings.Contains(w.Body.String(), "nothing to stop") {
				t.Fatalf("status %d body %s, want 422 carrying the handler error", w.Code, w.Body.String())
			}
		})
	})
}
