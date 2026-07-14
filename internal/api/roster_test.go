package api_test

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/MateusAMP2119/iris-engine-cli/internal/api"
)

// This file proves the E09.5 route-mux half of the read API: the fixed
// engine-state route roster (the meta-roster routes with their item sub-routes
// plus the E14 graph and triage routes), all GET; the role report on GET
// /healthz and GET /leader; and the per-route scope checks with the 403 split
// between the engine-state and data surfaces.

// engineStateRoutes is the fixed engine-state roster, each pattern instantiated
// with sample path params: the meta-roster collection and item routes plus the
// graph and triage routes owned by E14. Every one is GET-only and lives on the
// read (engine-state) scope.
var engineStateRoutes = []string{
	"/pipelines",
	"/pipelines/load_orders",
	"/pipelines/load_orders/gate",
	"/runs",
	"/runs/42",
	"/runs/42/trace",
	"/dead_letters",
	"/dead_letters/42",
	"/dead_letters/42/impact",
	"/lanes",
	"/dependencies",
	"/workload",
	"/leader",
	"/stats",
	"/healthz",
	"/provenance/analytics/orders/123",
}

// dataRoutes is the data surface: raw table reads and declared endpoints,
// GET-only, on the data scope.
var dataRoutes = []string{
	"/data/analytics/orders",
	"/q/orders_by_customer",
}

// jsonEnvelope is the decoded read-API document: a data envelope, an error
// envelope, or both halves absent (which is a wire-contract violation).
type jsonEnvelope struct {
	Data  json.RawMessage `json:"data"`
	Error *struct {
		Code    string `json:"code"`
		Message string `json:"message"`
	} `json:"error"`
}

// get performs a request against h and decodes the JSON envelope.
func get(t *testing.T, h http.Handler, method, path string) (int, jsonEnvelope, *http.Response) {
	t.Helper()
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(method, path, nil)
	h.ServeHTTP(rec, req)
	var env jsonEnvelope
	res := rec.Result()
	if err := json.NewDecoder(res.Body).Decode(&env); err != nil {
		t.Fatalf("%s %s: body is not a JSON envelope: %v", method, path, err)
	}
	return rec.Code, env, res
}

// TestEngineStateRouteRoster proves the engine-state surface serves exactly the
// meta-roster routes with their item sub-routes, all GET, plus the E14 graph
// and triage routes: every roster route is mounted (never 404, never 405 on
// GET), every non-GET method on a roster route is 405 method_not_allowed, and
// everything outside the roster is 404 not_found.
func TestEngineStateRouteRoster(t *testing.T) {
	t.Run("engine-state-route-roster", func(t *testing.T) {
		mux := leaderMux()

		t.Run("every roster route is mounted and answers GET", func(t *testing.T) {
			for _, path := range append(append([]string{}, engineStateRoutes...), dataRoutes...) {
				code, env, res := get(t, mux, http.MethodGet, path)
				if code == http.StatusNotFound {
					t.Errorf("GET %s = 404; the route is part of the fixed roster and must be mounted", path)
					continue
				}
				if code == http.StatusMethodNotAllowed {
					t.Errorf("GET %s = 405; every roster route is a GET", path)
					continue
				}
				if ct := res.Header.Get("Content-Type"); ct != "application/json" {
					t.Errorf("GET %s Content-Type = %q, want application/json", path, ct)
				}
				if env.Data == nil && env.Error == nil {
					t.Errorf("GET %s: response is neither a data nor an error envelope", path)
				}
			}
		})

		t.Run("non-GET on a roster route is 405 method_not_allowed", func(t *testing.T) {
			for _, path := range append(append([]string{}, engineStateRoutes...), dataRoutes...) {
				for _, method := range []string{http.MethodPost, http.MethodPut, http.MethodDelete} {
					code, env, _ := get(t, mux, method, path)
					if code != http.StatusMethodNotAllowed {
						t.Errorf("%s %s = %d, want 405", method, path, code)
						continue
					}
					if env.Error == nil || env.Error.Code != "method_not_allowed" {
						t.Errorf("%s %s error envelope = %+v, want code method_not_allowed", method, path, env.Error)
					}
				}
			}
		})

		t.Run("routes outside the roster are 404 not_found", func(t *testing.T) {
			for _, path := range []string{
				"/",
				"/metrics", // deliberately unrouted: a monitor consumes GET /stats instead.
				"/nope",
				"/pipelines/load_orders/nope",
				"/pipelines/load_orders/gate/extra",
				"/runs/42/nope",
				"/dead_letters/42/nope",
				"/provenance/analytics/orders", // provenance needs schema, table, and pk.
				"/provenance/analytics/orders/123/extra",
				"/data",
				"/data/analytics",
				"/data/analytics/orders/extra",
				"/q",
				"/q/orders_by_customer/extra",
				"/healthz/extra",
			} {
				code, env, _ := get(t, mux, http.MethodGet, path)
				if code != http.StatusNotFound {
					t.Errorf("GET %s = %d, want 404 (outside the fixed roster)", path, code)
					continue
				}
				if env.Error == nil || env.Error.Code != "not_found" {
					t.Errorf("GET %s error envelope = %+v, want code not_found", path, env.Error)
				}
			}
		})

		t.Run("an empty path parameter never matches", func(t *testing.T) {
			for _, path := range []string{"/pipelines//gate", "/runs//trace", "/q//"} {
				code, _, _ := get(t, mux, http.MethodGet, path)
				if code != http.StatusNotFound {
					t.Errorf("GET %s = %d, want 404 (empty path param)", path, code)
				}
			}
		})
	})
}

// TestHealthzLeaderReportRole proves GET /healthz and GET /leader report the
// node's current leadership role on both a leader and a standby ("GET /healthz
// / GET /leader report role on both").
func TestHealthzLeaderReportRole(t *testing.T) {
	t.Run("healthz-leader-report-role", func(t *testing.T) {
		for _, tc := range []struct {
			name       string
			set        func(*api.RoleState)
			wantRole   string
			wantLeader string
		}{
			{"leader", func(s *api.RoleState) { s.SetLeader() }, "leader", ""},
			{"standby", func(s *api.RoleState) { s.SetStandby("10.0.0.7:9000") }, "standby", "10.0.0.7:9000"},
		} {
			t.Run(tc.name, func(t *testing.T) {
				role := api.NewRoleState()
				tc.set(role)
				mux := api.NewMux(api.WithRole(role))

				code, env, _ := get(t, mux, http.MethodGet, "/healthz")
				if code != http.StatusOK {
					t.Fatalf("GET /healthz on %s = %d, want 200", tc.name, code)
				}
				var health struct {
					Status string `json:"status"`
					Role   string `json:"role"`
				}
				if err := json.Unmarshal(env.Data, &health); err != nil {
					t.Fatalf("decode healthz data: %v", err)
				}
				if health.Role != tc.wantRole {
					t.Errorf("healthz role on %s = %q, want %q", tc.name, health.Role, tc.wantRole)
				}

				code, env, _ = get(t, mux, http.MethodGet, "/leader")
				if code != http.StatusOK {
					t.Fatalf("GET /leader on %s = %d, want 200", tc.name, code)
				}
				var leader struct {
					Role   string `json:"role"`
					Leader string `json:"leader"`
				}
				if err := json.Unmarshal(env.Data, &leader); err != nil {
					t.Fatalf("decode leader data: %v", err)
				}
				if leader.Role != tc.wantRole {
					t.Errorf("leader role on %s = %q, want %q", tc.name, leader.Role, tc.wantRole)
				}
				if leader.Leader != tc.wantLeader {
					t.Errorf("leader hint on %s = %q, want %q", tc.name, leader.Leader, tc.wantLeader)
				}
			})
		}
	})
}
