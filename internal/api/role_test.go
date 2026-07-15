package api_test

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/MateusAMP2119/iris-lakehouse/internal/api"
)

// decodeErr decodes an error envelope, including the leader hint the not_leader
// shape carries.
type errEnvelope struct {
	Error struct {
		Code    string `json:"code"`
		Message string `json:"message"`
		Leader  string `json:"leader"`
	} `json:"error"`
}

func doReq(t *testing.T, h http.Handler, method, path string) (int, errEnvelope) {
	t.Helper()
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(method, path, nil))
	var env errEnvelope
	_ = json.Unmarshal(rec.Body.Bytes(), &env)
	return rec.Code, env
}

// TestStandbyRejectsMutations proves a standby daemon rejects mutation requests
// with guidance pointing at the leader ("standbys reject mutations with leader
// guidance"). A mutating (non-safe) request on a standby -- or on a daemon
// whose role is not yet the confirmed leader -- gets the not_leader error
// envelope carrying the leader hint; the leader accepts mutations (they fall
// through to normal routing), and reads work on any role.
func TestStandbyRejectsMutations(t *testing.T) {
	t.Run("standby-rejects-mutations", func(t *testing.T) {
		t.Run("a standby rejects a mutation with leader guidance", func(t *testing.T) {
			role := api.NewRoleState()
			role.SetStandby("10.0.0.7:9000")
			mux := api.NewMux(api.WithRole(role))

			code, env := doReq(t, mux, http.MethodPost, "/pipelines")
			if code != api.StatusNotLeader {
				t.Errorf("standby mutation status = %d, want %d (not_leader)", code, api.StatusNotLeader)
			}
			if env.Error.Code != api.CodeNotLeader {
				t.Errorf("error code = %q, want %q", env.Error.Code, api.CodeNotLeader)
			}
			if env.Error.Leader != "10.0.0.7:9000" {
				t.Errorf("leader guidance = %q, want the leader address", env.Error.Leader)
			}
			if env.Error.Message == "" {
				t.Error("not_leader envelope carries no human guidance message")
			}
		})

		t.Run("an unknown-role daemon rejects mutations too (only a confirmed leader writes)", func(t *testing.T) {
			mux := api.NewMux() // default role reporter: unknown
			code, env := doReq(t, mux, http.MethodDelete, "/pipelines/x")
			if code != api.StatusNotLeader || env.Error.Code != api.CodeNotLeader {
				t.Errorf("unknown-role mutation = (%d, %q), want (%d, %q)", code, env.Error.Code, api.StatusNotLeader, api.CodeNotLeader)
			}
			if env.Error.Leader != "unknown" {
				t.Errorf("leader guidance with no known leader = %q, want %q", env.Error.Leader, "unknown")
			}
		})

		t.Run("the leader accepts mutations (they route normally, no not_leader)", func(t *testing.T) {
			role := api.NewRoleState()
			role.SetLeader()
			mux := api.NewMux(api.WithRole(role))
			// No mutating route exists yet, so a POST falls through to the normal
			// not-found path -- crucially NOT the standby not_leader rejection.
			code, env := doReq(t, mux, http.MethodPost, "/pipelines")
			if code == api.StatusNotLeader || env.Error.Code == api.CodeNotLeader {
				t.Errorf("leader rejected a mutation as not_leader (%d, %q); a leader accepts mutations", code, env.Error.Code)
			}
		})

		t.Run("reads work on any role; healthz reports the role", func(t *testing.T) {
			for _, tc := range []struct {
				name string
				set  func(*api.RoleState)
				want string
			}{
				{"leader", func(s *api.RoleState) { s.SetLeader() }, "leader"},
				{"standby", func(s *api.RoleState) { s.SetStandby("") }, "standby"},
			} {
				t.Run(tc.name, func(t *testing.T) {
					role := api.NewRoleState()
					tc.set(role)
					mux := api.NewMux(api.WithRole(role))
					rec := httptest.NewRecorder()
					mux.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/healthz", nil))
					if rec.Code != http.StatusOK {
						t.Fatalf("GET /healthz on %s: status = %d, want 200", tc.name, rec.Code)
					}
					var env struct {
						Data struct {
							Status string `json:"status"`
							Role   string `json:"role"`
						} `json:"data"`
					}
					if err := json.Unmarshal(rec.Body.Bytes(), &env); err != nil {
						t.Fatalf("decode healthz: %v", err)
					}
					if env.Data.Status != "ok" {
						t.Errorf("healthz status = %q, want ok", env.Data.Status)
					}
					if env.Data.Role != tc.want {
						t.Errorf("healthz role = %q, want %q", env.Data.Role, tc.want)
					}
				})
			}
		})
	})
}
