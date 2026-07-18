package api_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/MateusAMP2119/iris-lakehouse/internal/api"
)

// fixedCatalog is a CatalogHandler answering a canned result, recording the request.
type fixedCatalog struct {
	res  api.CatalogInstallResult
	err  error
	last *api.CatalogInstallRequest
}

func (f *fixedCatalog) InstallPack(_ context.Context, req api.CatalogInstallRequest) (api.CatalogInstallResult, error) {
	if f.last != nil {
		*f.last = req
	}
	return f.res, f.err
}

// postCatalog drives POST /catalog/install with the given body and returns the recorder.
func postCatalog(h http.Handler, body string) *httptest.ResponseRecorder {
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/catalog/install", strings.NewReader(body))
	h.ServeHTTP(rec, req)
	return rec
}

// TestCatalogInstallRoute proves the /catalog/install contract: envelope on success,
// 422 on an empty pack or a failed install, 500 unwired, 421 on a standby.
func TestCatalogInstallRoute(t *testing.T) {
	t.Run("success renders the data envelope and forwards the flags", func(t *testing.T) {
		var got api.CatalogInstallRequest
		h := &fixedCatalog{res: api.CatalogInstallResult{Pack: "quake-monitor", Files: []string{"a"}, ApplyOrder: []string{"a"}, Applied: true}, last: &got}
		rec := postCatalog(leaderMux(api.WithCatalog(h)), `{"pack":"quake-monitor","apply":true,"force":true}`)
		if rec.Code != http.StatusOK {
			t.Fatalf("status = %d, want 200 (%s)", rec.Code, rec.Body.String())
		}
		if !got.Apply || !got.Force || got.Pack != "quake-monitor" {
			t.Errorf("handler saw %+v, want pack+apply+force forwarded", got)
		}
		var env struct {
			Data api.CatalogInstallResult `json:"data"`
		}
		if err := json.Unmarshal(rec.Body.Bytes(), &env); err != nil || env.Data.Pack != "quake-monitor" || !env.Data.Applied {
			t.Errorf("body = %s, want the result in the data envelope (err %v)", rec.Body.String(), err)
		}
	})

	t.Run("an empty pack name is operation_failed", func(t *testing.T) {
		rec := postCatalog(leaderMux(api.WithCatalog(&fixedCatalog{})), `{}`)
		if rec.Code != http.StatusUnprocessableEntity {
			t.Errorf("status = %d, want 422", rec.Code)
		}
	})

	t.Run("a malformed body is bad_request", func(t *testing.T) {
		rec := postCatalog(leaderMux(api.WithCatalog(&fixedCatalog{})), `{"pack":`)
		if rec.Code != http.StatusBadRequest {
			t.Errorf("status = %d, want 400", rec.Code)
		}
	})

	t.Run("an unwired handler is an internal fault", func(t *testing.T) {
		rec := postCatalog(leaderMux(), `{"pack":"x"}`)
		if rec.Code != http.StatusInternalServerError {
			t.Errorf("status = %d, want 500", rec.Code)
		}
	})

	t.Run("a standby answers not_leader", func(t *testing.T) {
		rec := postCatalog(api.NewMux(api.WithCatalog(&fixedCatalog{})), `{"pack":"x"}`)
		if rec.Code != api.StatusNotLeader {
			t.Errorf("status = %d, want %d (not_leader)", rec.Code, api.StatusNotLeader)
		}
	})

	t.Run("GET is method_not_allowed", func(t *testing.T) {
		rec := httptest.NewRecorder()
		leaderMux(api.WithCatalog(&fixedCatalog{})).ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/catalog/install", nil))
		if rec.Code != http.StatusMethodNotAllowed {
			t.Errorf("status = %d, want 405", rec.Code)
		}
	})
}

// fixedCatalogList is a CatalogListHandler answering a canned listing.
type fixedCatalogList struct {
	res api.CatalogListResult
	err error
}

func (f *fixedCatalogList) ListPacks(context.Context) (api.CatalogListResult, error) {
	return f.res, f.err
}

// TestCatalogListRoute proves GET /catalog (#219): the listing envelope on any
// role, 500 unwired, params refused, POST refused.
func TestCatalogListRoute(t *testing.T) {
	t.Run("the listing renders in the data envelope on a standby too", func(t *testing.T) {
		h := &fixedCatalogList{res: api.CatalogListResult{Packs: []api.CatalogPack{{Name: "quake-monitor", Source: "embedded"}}, Warnings: []string{"w"}}}
		rec := httptest.NewRecorder()
		api.NewMux(api.WithCatalogList(h)).ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/catalog", nil))
		if rec.Code != http.StatusOK {
			t.Fatalf("status = %d, want 200 (%s)", rec.Code, rec.Body.String())
		}
		var env struct {
			Data api.CatalogListResult `json:"data"`
		}
		if err := json.Unmarshal(rec.Body.Bytes(), &env); err != nil || len(env.Data.Packs) != 1 || env.Data.Packs[0].Name != "quake-monitor" {
			t.Errorf("body = %s, want the listing envelope (err %v)", rec.Body.String(), err)
		}
	})

	t.Run("an unwired reader is an internal fault", func(t *testing.T) {
		rec := httptest.NewRecorder()
		leaderMux().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/catalog", nil))
		if rec.Code != http.StatusInternalServerError {
			t.Errorf("status = %d, want 500", rec.Code)
		}
	})

	t.Run("query params are refused", func(t *testing.T) {
		rec := httptest.NewRecorder()
		leaderMux(api.WithCatalogList(&fixedCatalogList{})).ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/catalog?x=1", nil))
		if rec.Code != http.StatusBadRequest {
			t.Errorf("status = %d, want 400", rec.Code)
		}
	})
}
