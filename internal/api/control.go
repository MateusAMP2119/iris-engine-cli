package api

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
)

// This file is the control-plane mutation surface of the daemon (specification
// sections 2, 8, and 12): POST /apply and POST /destroy, the two routes iris declare
// apply and iris declare destroy drive. They are mutations, so the mux's existing
// leader gate already rejects them on a standby with not_leader guidance (exit 6);
// on the leader they run the injected ControlHandler, which the daemon wires to the
// registry apply, schema provisioning, and the scoped teardown.
//
// api stays a leaf: it defines the ControlHandler seam and the plain request/result
// shapes but reaches nothing up the stack. The daemon supplies the handler that
// composes dispatch, pg, and store; the mux only routes to it and renders its
// outcome as the section-7 envelope.

// ControlRequest is the body of a POST /apply or POST /destroy: the workspace-relative
// declaration target the leader resolves from its own workspace tree, plus the
// destructive-op flags. The CLI reads local files only to validate, then sends the
// path (workspace is per-host: the leader dispatches from and resolves against its own
// tree, aligned with the E11 candidate-requires-workspace rule).
type ControlRequest struct {
	// Path is the workspace-relative (or absolute) declaration target: a file named
	// iris-declare.yaml or a folder resolving to one.
	Path string `json:"path"`
	// DryRun previews the change without writing (specification section 12).
	DryRun bool `json:"dry_run,omitempty"`
	// Confirm is the explicit confirmation a destructive op requires over the API
	// (specification section 12: control PAT plus an explicit confirm field).
	Confirm bool `json:"confirm,omitempty"`
	// Force requests that soft-blocks be overridden (in-flight runs on scope are
	// cancelled and dead-lettered stopped).
	Force bool `json:"force,omitempty"`
}

// ControlResult is the success payload of a control mutation: what the leader did,
// carried in the section-7 data envelope. Warnings are advisory (cross-mode reads and
// the like); they accompany the outcome, never replace it.
type ControlResult struct {
	// Kind is the declaration kind acted on: "pipeline" or "composer".
	Kind string `json:"kind"`
	// Target is the pipeline name (pipeline) or lane (composer) acted on.
	Target string `json:"target"`
	// DryRun reports whether the change was a preview (nothing written).
	DryRun bool `json:"dry_run,omitempty"`
	// Warnings are advisory messages that rode the outcome.
	Warnings []string `json:"warnings,omitempty"`
}

// ControlHandler runs the leader-side control mutations. The daemon implements it over
// the registry applier, the schema provisioner, and the scoped destroyer; the mux
// depends only on this interface, so api never imports dispatch/pg/store. An
// operation-failed error (validation, interlock, blocker, provisioning) is rendered
// as the operation-failed envelope; the daemon owns the wrapping.
type ControlHandler interface {
	// Apply registers and provisions the one declaration named by req, idempotently.
	Apply(ctx context.Context, req ControlRequest) (ControlResult, error)
	// Destroy tears down the one declaration named by req.
	Destroy(ctx context.Context, req ControlRequest) (ControlResult, error)
}

// The control-plane error codes and statuses. Like not_leader they are distinct from
// the read-API closed set, so a client can tell a control failure apart from a read
// error and map it to the operation-failed exit category (specification section 8).
const (
	// CodeOpFailed is the machine code for a control mutation that failed (a
	// validation, interlock, blocker, or provisioning failure). The CLI maps it to
	// exit 4 (operation failed).
	CodeOpFailed = "operation_failed"
	// CodeBadRequest is the machine code for a malformed control request body.
	CodeBadRequest = "bad_request"
)

// ErrControlUnavailable is returned by the default (unwired) control handler: a
// mutation reached the mux but no control handler is installed. It should be
// unreachable in practice -- the leader installs the handler before it reports the
// leader role, and the mux gates mutations to the leader -- so it is an internal
// fault, not an operation failure.
var ErrControlUnavailable = errors.New("api: control plane not available")

// WithControl wires the leader-side control handler the mux routes POST /apply and
// POST /destroy to. A nil handler is ignored, keeping the safe default (mutations
// fail with an internal fault until a real handler is installed).
func WithControl(h ControlHandler) MuxOption {
	return func(m *mux) {
		if h != nil {
			m.control = h
		}
	}
}

// noControl is the default ControlHandler before one is wired: every mutation is an
// internal fault, never a silent success.
type noControl struct{}

func (noControl) Apply(context.Context, ControlRequest) (ControlResult, error) {
	return ControlResult{}, ErrControlUnavailable
}

func (noControl) Destroy(context.Context, ControlRequest) (ControlResult, error) {
	return ControlResult{}, ErrControlUnavailable
}

// serveApply handles POST /apply: decode the request, run the leader's apply, and
// render the section-7 envelope. Method enforcement mirrors the read routes (only the
// declared verb is allowed); the leader gate ran already in ServeHTTP.
func (m *mux) serveApply(w http.ResponseWriter, r *http.Request) {
	m.serveControl(w, r, m.control.Apply)
}

// serveDestroy handles POST /destroy, symmetric to serveApply.
func (m *mux) serveDestroy(w http.ResponseWriter, r *http.Request) {
	m.serveControl(w, r, m.control.Destroy)
}

// serveControl is the shared body of the two control routes: enforce POST, decode the
// request, run op, and render success (200 data envelope) or failure. A malformed
// body is 400 bad_request; an operation failure is 422 operation_failed; an internal
// control fault is 500 internal.
func (m *mux) serveControl(w http.ResponseWriter, r *http.Request, op func(context.Context, ControlRequest) (ControlResult, error)) {
	if r.Method != http.MethodPost {
		WriteError(w, http.StatusMethodNotAllowed, "method_not_allowed", "POST "+r.URL.Path+" only")
		return
	}
	var req ControlRequest
	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()
	if err := dec.Decode(&req); err != nil {
		WriteError(w, http.StatusBadRequest, CodeBadRequest, "malformed control request body: "+err.Error())
		return
	}
	// Destructive ops over the API (here: /destroy) require a control PAT
	// (enforced by scope) plus an explicit confirm body field (specification
	// section 12). The confirm gate runs after decode but before the handler,
	// so a !Confirm destroy never reaches the control plane and is rejected
	// as an operation failure.
	if r.URL.Path == "/destroy" {
		if !req.Confirm {
			WriteError(w, http.StatusUnprocessableEntity, CodeOpFailed, "confirm required for destructive operation")
			return
		}
		// debug marker: if we reach here with Confirm, we will proceed
		_ = req.Confirm
	}
	res, err := op(r.Context(), req)
	if err != nil {
		if errors.Is(err, ErrControlUnavailable) {
			WriteError(w, http.StatusInternalServerError, "internal", err.Error())
			return
		}
		WriteError(w, http.StatusUnprocessableEntity, CodeOpFailed, err.Error())
		return
	}
	WriteData(w, http.StatusOK, res)
}
