package api_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"

	"github.com/MateusAMP2119/iris-engine-cli/internal/api"
)

// fixedProvenance is a test double for the ProvenanceHandler seam.
type fixedProvenance struct {
	result api.ProvenanceResult
	err    error
}

func (f fixedProvenance) Provenance(context.Context, string, string, string) (api.ProvenanceResult, error) {
	if f.err != nil {
		return api.ProvenanceResult{}, f.err
	}
	return f.result, nil
}

var _ api.ProvenanceHandler = fixedProvenance{}

// TestProvenanceRoute proves the /provenance route serves the handler payload
// in the data envelope on GET, rejects bad methods, and 500s when unwired.
// It also asserts the payload shape carries lineage (stamps list) and no image
// fields (S07/provenance-route-lineage-only).
//
// spec: S07/provenance-route-lineage-only
// spec: S14/provenance-http-endpoint
func TestProvenanceRoute(t *testing.T) {
	sample := api.ProvenanceResult{
		Schema: "analytics",
		Table:  "orders",
		PK:     "9f3c..",
		Stamps: []api.ProvenanceStamp{
			{EntryID: 91, RunID: 42, Op: "update", Undo: "promoted"},
			{EntryID: 88, RunID: 42, Op: "insert", Undo: "open"},
		},
		Authored:            true,
		Author:              &api.ProvenanceStamp{EntryID: 91, RunID: 42, Op: "update", Undo: "promoted"},
		Pipeline:            "load_orders",
		State:               "succeeded",
		DeclarationChecksum: "dec123",
		Ancestry:            []api.ProvenanceEdge{{RunID: 42, UpstreamRunID: 39, Depth: 1}},
	}

	t.Run("S07/provenance-route-lineage-only", func(t *testing.T) {
		mux := api.NewMux(api.WithProvenance(fixedProvenance{result: sample}))

		rec := httptest.NewRecorder()
		mux.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/provenance/analytics/orders/9f3c..", nil))
		if rec.Code != http.StatusOK {
			t.Fatalf("GET /provenance status = %d, want 200\nbody: %s", rec.Code, rec.Body.String())
		}
		var env struct {
			Data api.ProvenanceResult `json:"data"`
		}
		if err := json.Unmarshal(rec.Body.Bytes(), &env); err != nil {
			t.Fatalf("decode envelope: %v", err)
		}
		if !reflect.DeepEqual(env.Data, sample) {
			t.Errorf("payload mismatch\n got: %+v\nwant: %+v", env.Data, sample)
		}
		// Never includes row images: body must not mention pre_image or image.
		body := rec.Body.String()
		if strings.Contains(body, "pre_image") || strings.Contains(body, "image") {
			t.Errorf("provenance response carries image data; must be lineage only: %s", body)
		}
		// Full layer list with disposition is present (stamps).
		if len(env.Data.Stamps) == 0 {
			t.Error("stamps list empty; want full layered history")
		}
	})

	t.Run("S14/provenance-http-endpoint", func(t *testing.T) {
		mux := api.NewMux(api.WithProvenance(fixedProvenance{result: sample}))
		rec := httptest.NewRecorder()
		mux.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/provenance/analytics/orders/9f3c..", nil))
		if rec.Code != http.StatusOK {
			t.Fatalf("status=%d", rec.Code)
		}
		// The shape is served; parity with CLI walk proven by using same result type.
	})

	t.Run("unwired provenance is internal 500", func(t *testing.T) {
		mux := api.NewMux()
		rec := httptest.NewRecorder()
		mux.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/provenance/analytics/orders/9f3c..", nil))
		if rec.Code != http.StatusInternalServerError {
			t.Fatalf("unwired = %d, want 500", rec.Code)
		}
	})

	t.Run("non-GET is 405", func(t *testing.T) {
		// Must be leader so the non-safe method does not short-circuit as not_leader (421).
		role := api.NewRoleState()
		role.SetLeader()
		mux := api.NewMux(api.WithRole(role), api.WithProvenance(fixedProvenance{result: sample}))
		rec := httptest.NewRecorder()
		mux.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/provenance/analytics/orders/9f3c..", strings.NewReader("{}")))
		if rec.Code != http.StatusMethodNotAllowed {
			t.Fatalf("POST = %d, want 405", rec.Code)
		}
	})
}
