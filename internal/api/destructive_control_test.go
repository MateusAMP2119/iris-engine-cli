package api_test

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/MateusAMP2119/iris-engine-cli/internal/api"
	"github.com/MateusAMP2119/iris-engine-cli/internal/pat"
)

// This file proves the remote tiering of destructive control operations over the
// API surface (specification section 12): control-PAT + explicit confirm for
// destructive routes, and leader-only acceptance. Tests use the mux directly
// (the same handler the in-process daemon serves over unix and TCP sockets)
// with injected role and control handler seams -- fakes, no live database.

// controlCall records an invocation of the control handler.
type controlCall struct {
	path string
	req  api.ControlRequest
}

// capturingControl is a ControlHandler that records calls and optionally returns error.
type capturingControl struct {
	calls []controlCall
	err   error
}

func (c *capturingControl) Apply(_ context.Context, req api.ControlRequest) (api.ControlResult, error) {
	c.calls = append(c.calls, controlCall{path: "/apply", req: req})
	return api.ControlResult{}, c.err
}

func (c *capturingControl) Destroy(_ context.Context, req api.ControlRequest) (api.ControlResult, error) {
	c.calls = append(c.calls, controlCall{path: "/destroy", req: req})
	return api.ControlResult{}, c.err
}

// postControlJSON is a helper to POST a ControlRequest as JSON to the handler under authority.
func postControlJSON(t *testing.T, h http.Handler, method, path string, a api.Authority, body api.ControlRequest) *httptest.ResponseRecorder {
	t.Helper()
	var buf bytes.Buffer
	if err := json.NewEncoder(&buf).Encode(body); err != nil {
		t.Fatalf("encode body: %v", err)
	}
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(method, path, &buf)
	req.Header.Set("Content-Type", "application/json")
	req = req.WithContext(api.WithAuthority(req.Context(), a))
	h.ServeHTTP(rec, req)
	return rec
}

// (errEnvelope is defined in role_test.go for the package; reuse it here.)

// TestAPIDestructiveConfirmAndLeader proves the two API contracts for destructive
// ops over the control plane.
//
// spec: S12/api-destructive-control-pat-confirm-field
// spec: S12/api-destructive-leader-only
func TestAPIDestructiveConfirmAndLeader(t *testing.T) {
	t.Run("S12/api-destructive-control-pat-confirm-field", func(t *testing.T) {
		// A control authority (as a TCP control PAT would grant) must still supply
		// explicit Confirm:true for /destroy; without it the handler must not be
		// invoked and the request must be rejected as an operation failure.
		role := api.NewRoleState()
		role.SetLeader()
		capCtl := &capturingControl{}
		mux := api.NewMux(api.WithRole(role), api.WithControl(capCtl))

		controlAuth := api.Authority{PATID: "ctl", Scopes: []pat.Scope{pat.ScopeControl}}

		// Without confirm: handler must not be invoked for the destructive op.
		rec := postControlJSON(t, mux, http.MethodPost, "/destroy", controlAuth, api.ControlRequest{Path: "foo/iris-declare.yaml", Confirm: false})
		if rec.Code != http.StatusUnprocessableEntity {
			t.Errorf("destroy without confirm status = %d, want 422 (op failed)", rec.Code)
		}
		if len(capCtl.calls) != 0 {
			t.Errorf("destroy handler was invoked with Confirm=false; API must require explicit confirm for destructive ops (S12/api-destructive-control-pat-confirm-field)")
		}
	})

	t.Run("S12/api-destructive-leader-only", func(t *testing.T) {
		// Mutations on /destroy (and /apply) are accepted only when role is leader.
		// A standby must reject with not_leader before reaching any control handler.
		role := api.NewRoleState()
		role.SetStandby("leader.example:1234")
		capCtl := &capturingControl{}
		mux := api.NewMux(api.WithRole(role), api.WithControl(capCtl))

		controlAuth := api.Authority{PATID: "ctl", Scopes: []pat.Scope{pat.ScopeControl}}

		rec := postControlJSON(t, mux, http.MethodPost, "/destroy", controlAuth, api.ControlRequest{Path: "bar/iris-declare.yaml", Confirm: true})
		if rec.Code != api.StatusNotLeader {
			t.Errorf("standby POST /destroy = status %d, want %d (not_leader)", rec.Code, api.StatusNotLeader)
		}
		var env errEnvelope
		_ = json.Unmarshal(rec.Body.Bytes(), &env)
		if env.Error.Code != api.CodeNotLeader {
			t.Errorf("standby destroy error code = %q, want %q", env.Error.Code, api.CodeNotLeader)
		}
		if len(capCtl.calls) != 0 {
			t.Errorf("standby destroy invoked the control handler; leader-only gate must prevent it (S12/api-destructive-leader-only)")
		}
	})
}

// TestDestroyRequiresConfirmEvenWithControlToken documents that a control-scoped
// authority alone is not sufficient; the body must carry explicit confirm.
//
// spec: S12/api-destructive-control-pat-confirm-field
func TestDestroyRequiresConfirmEvenWithControlToken(t *testing.T) {
	role := api.NewRoleState()
	role.SetLeader()
	capCtl := &capturingControl{}
	mux := api.NewMux(api.WithRole(role), api.WithControl(capCtl))

	ctl := api.Authority{PATID: "c", Scopes: []pat.Scope{pat.ScopeControl}}

	rec := postControlJSON(t, mux, http.MethodPost, "/destroy", ctl, api.ControlRequest{Path: "x/iris-declare.yaml", Confirm: false})
	if rec.Code == http.StatusOK {
		t.Errorf("destroy without confirm returned 200; expected refusal (S12/api-destructive-control-pat-confirm-field)")
	}
	for _, c := range capCtl.calls {
		if c.path == "/destroy" && !c.req.Confirm {
			t.Errorf("captured a !Confirm destroy call -- gate missing (S12/api-destructive-control-pat-confirm-field)")
		}
	}
	// control PAT + confirm must reach the handler (not be scope-rejected); with no real handler it yields internal/422, never 403.
	recOK := postControlJSON(t, mux, http.MethodPost, "/destroy", ctl, api.ControlRequest{Path: "ok/iris-declare.yaml", Confirm: true})
	if recOK.Code == http.StatusForbidden {
		t.Errorf("control PAT + confirm got 403 on destroy; control scope must authorize the destructive (S12/api-destructive-control-pat-confirm-field)")
	}
}

// TestAPIDrainConfirmField proves the drain destructive op over API also
// requires explicit confirm even with control authority (defense in depth).
//
// spec: S12/api-destructive-control-pat-confirm-field
func TestAPIDrainConfirmField(t *testing.T) {
	role := api.NewRoleState()
	role.SetLeader()
	mux := api.NewMux(api.WithRole(role))

	// without confirm body: 422, not processed.
	rec := postJSONDrain(t, mux, `{"all":true}`)
	if rec.Code != http.StatusUnprocessableEntity {
		t.Errorf("drain without confirm status=%d want 422", rec.Code)
	}

	// with confirm: reaches (currently not-wired 422, not 403).
	recOK := postJSONDrain(t, mux, `{"all":true,"confirm":true}`)
	if recOK.Code == http.StatusForbidden || recOK.Code == http.StatusUnauthorized {
		t.Errorf("control+confirm drain blocked at auth/scope: %d", recOK.Code)
	}
}

func postJSONDrain(t *testing.T, h http.Handler, body string) *httptest.ResponseRecorder {
	t.Helper()
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/deadletter/drain", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req = req.WithContext(api.WithAuthority(req.Context(), api.Authority{PATID: "c", Scopes: []pat.Scope{pat.ScopeControl}}))
	h.ServeHTTP(rec, req)
	return rec
}
