package api_test

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/MateusAMP2119/iris-engine-cli/internal/api"
	"github.com/MateusAMP2119/iris-engine-cli/internal/pat"
)

// This file proves the per-route scope checks at the mux level: the deliberate
// surface split -- data-only PATs see no engine internals, read-only PATs see
// no table data -- and that every mounted route, control plane included, is
// scope-checked. The transport half (RequirePAT resolving a bearer token into
// the request's authority over TCP, ambient authorization over the socket) is
// proven in the daemon's listener tests.

// getAs performs a request as the given authority and returns the status code
// and the error envelope's code ("" for a success envelope).
func getAs(t *testing.T, h http.Handler, method, path string, a api.Authority) (int, string) {
	t.Helper()
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(method, path, nil)
	req = req.WithContext(api.WithAuthority(req.Context(), a))
	h.ServeHTTP(rec, req)
	var env jsonEnvelope
	if err := json.Unmarshal(rec.Body.Bytes(), &env); err != nil {
		t.Fatalf("%s %s: body is not a JSON envelope: %v (%q)", method, path, err, rec.Body.String())
	}
	if env.Error != nil {
		return rec.Code, env.Error.Code
	}
	return rec.Code, ""
}

// TestScopeSplit403 proves the 403 split between the two read surfaces and that
// every route is scope-checked: a data-only PAT gets 403 forbidden on every
// engine-state route, a read-only PAT gets 403 on /data and /q, the control
// routes demand the control scope, and ambient (socket) authority passes every
// scope check.
func TestScopeSplit403(t *testing.T) {
	t.Run("scope-split-403", func(t *testing.T) {
		mux := leaderMux()
		dataOnly := api.Authority{PATID: "d1", Scopes: []pat.Scope{pat.ScopeData}}
		readOnly := api.Authority{PATID: "r1", Scopes: []pat.Scope{pat.ScopeRead}}
		controlOnly := api.Authority{PATID: "c1", Scopes: []pat.Scope{pat.ScopeControl}}
		ambient := api.Authority{Ambient: true}

		t.Run("a data-only PAT is 403 on every engine-state route", func(t *testing.T) {
			for _, path := range engineStateRoutes {
				code, errCode := getAs(t, mux, http.MethodGet, path, dataOnly)
				if code != http.StatusForbidden || errCode != "forbidden" {
					t.Errorf("data-only GET %s = (%d, %q), want (403, forbidden)", path, code, errCode)
				}
			}
		})

		t.Run("a read-only PAT is 403 on the data surface", func(t *testing.T) {
			for _, path := range dataRoutes {
				code, errCode := getAs(t, mux, http.MethodGet, path, readOnly)
				if code != http.StatusForbidden || errCode != "forbidden" {
					t.Errorf("read-only GET %s = (%d, %q), want (403, forbidden)", path, code, errCode)
				}
			}
		})

		t.Run("the right scope passes its own surface", func(t *testing.T) {
			for _, path := range engineStateRoutes {
				if code, errCode := getAs(t, mux, http.MethodGet, path, readOnly); code == http.StatusForbidden || code == http.StatusUnauthorized {
					t.Errorf("read GET %s = (%d, %q); the read scope covers the engine-state surface", path, code, errCode)
				}
			}
			for _, path := range dataRoutes {
				if code, errCode := getAs(t, mux, http.MethodGet, path, dataOnly); code == http.StatusForbidden || code == http.StatusUnauthorized {
					t.Errorf("data GET %s = (%d, %q); the data scope covers the data surface", path, code, errCode)
				}
			}
		})

		t.Run("control routes demand the control scope", func(t *testing.T) {
			for _, path := range []string{"/apply", "/destroy", "/pipeline/run", "/pipeline/build", "/deadletter/drain", "/workload/wipe"} {
				code, errCode := getAs(t, mux, http.MethodPost, path, readOnly)
				if code != http.StatusForbidden || errCode != "forbidden" {
					t.Errorf("read-only POST %s = (%d, %q), want (403, forbidden)", path, code, errCode)
				}
				if code, errCode := getAs(t, mux, http.MethodPost, path, controlOnly); code == http.StatusForbidden {
					t.Errorf("control POST %s = (%d, %q); the control scope covers the control plane", path, code, errCode)
				}
			}
			// The pipeline listing is an engine-state read: read scope, not control.
			if code, errCode := getAs(t, mux, http.MethodGet, "/pipeline/list", dataOnly); code != http.StatusForbidden || errCode != "forbidden" {
				t.Errorf("data-only GET /pipeline/list = (%d, %q), want (403, forbidden)", code, errCode)
			}
			if code, _ := getAs(t, mux, http.MethodGet, "/pipeline/list", readOnly); code == http.StatusForbidden {
				t.Errorf("read GET /pipeline/list = %d; the read scope covers the pipeline listing", code)
			}
		})

		t.Run("ambient authority passes every scope check", func(t *testing.T) {
			for _, path := range append(append([]string{}, engineStateRoutes...), dataRoutes...) {
				if code, errCode := getAs(t, mux, http.MethodGet, path, ambient); code == http.StatusForbidden || code == http.StatusUnauthorized {
					t.Errorf("ambient GET %s = (%d, %q); socket requests are ambiently authorized", path, code, errCode)
				}
			}
		})

		t.Run("an absent authority is ambient (the socket serves the mux bare)", func(t *testing.T) {
			rec := httptest.NewRecorder()
			mux.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/healthz", nil))
			if rec.Code != http.StatusOK {
				t.Errorf("bare GET /healthz = %d, want 200 (ambient default)", rec.Code)
			}
		})
	})
}
