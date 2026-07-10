package api

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
)

// This file is the control-plane endpoint-apply surface of the daemon
// (specification section 7): POST /endpoint/apply, the route `iris endpoint apply`
// drives. Publishing an endpoint is a control-plane mutation -- it prepare-verifies
// the derived SQL against the data database, persists the compiled shapes to meta,
// and swaps them into the live serving registry -- so the mux's existing leader
// gate rejects it on a standby with not_leader guidance, and its scope is control.
// On the leader it runs the injected EndpointControlHandler, which the daemon wires
// to the workspace endpoint discovery and dispatch's endpoint applier.
//
// api stays a leaf: it defines the seam and the plain request/result shapes but
// reaches nothing up the stack. The daemon supplies the handler.

// EndpointApplyRequest is the body of a POST /endpoint/apply: an optional endpoint
// name to publish just that one, or empty to publish every endpoint declared under
// the leader's workspace endpoints/ tree (specification section 7). The CLI sends
// the name; the leader discovers and compiles from its own workspace.
type EndpointApplyRequest struct {
	// Name is the single endpoint to publish, or empty to publish all declared ones.
	Name string `json:"name,omitempty"`
}

// EndpointApplyResult is the success payload of an endpoint apply: the names of the
// endpoints published, in a deterministic order.
type EndpointApplyResult struct {
	// Applied are the names of the endpoints published by this apply.
	Applied []string `json:"applied"`
}

// EndpointControlHandler runs the leader-side endpoint apply. The daemon implements
// it over the workspace endpoint discovery, the endpoint compiler, and dispatch's
// endpoint applier; the mux depends only on this interface, so api never imports
// dispatch/declare here. An operation-failed error (a bad endpoint file, a
// prepare-verify refusal, a persistence failure) is rendered as the
// operation-failed envelope.
type EndpointControlHandler interface {
	// ApplyEndpoints publishes the endpoints named by req (all when Name is empty).
	ApplyEndpoints(ctx context.Context, req EndpointApplyRequest) (EndpointApplyResult, error)
}

// ErrEndpointControlUnavailable is returned by the default (unwired) endpoint
// handler: a mutation reached the mux but no handler is installed. The leader
// installs the handler before it reports the leader role and the mux gates
// mutations to the leader, so this is an internal fault, not an operation failure.
var ErrEndpointControlUnavailable = errors.New("api: endpoint control plane not available")

// WithEndpointControl wires the leader-side endpoint apply handler the mux routes
// POST /endpoint/apply to. A nil handler is ignored, keeping the safe default (the
// mutation faults internally until a real handler is installed).
func WithEndpointControl(h EndpointControlHandler) MuxOption {
	return func(m *mux) {
		if h != nil {
			m.endpointCtl = h
		}
	}
}

// noEndpointControl is the default handler before one is wired: every apply is an
// internal fault, never a silent success.
type noEndpointControl struct{}

func (noEndpointControl) ApplyEndpoints(context.Context, EndpointApplyRequest) (EndpointApplyResult, error) {
	return EndpointApplyResult{}, ErrEndpointControlUnavailable
}

// serveEndpointApply handles POST /endpoint/apply: decode the request, run the
// leader's endpoint apply, and render the section-7 envelope. The leader gate ran
// already in ServeHTTP; a malformed body is 400, an operation failure 422, an
// internal fault 500.
func (m *mux) serveEndpointApply(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		WriteError(w, http.StatusMethodNotAllowed, string(CodeMethodNotAllowed), "POST "+r.URL.Path+" only")
		return
	}
	var req EndpointApplyRequest
	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()
	if err := dec.Decode(&req); err != nil {
		WriteError(w, http.StatusBadRequest, CodeBadRequest, "malformed endpoint apply request body: "+err.Error())
		return
	}
	res, err := m.endpointCtl.ApplyEndpoints(r.Context(), req)
	if err != nil {
		if errors.Is(err, ErrEndpointControlUnavailable) {
			WriteError(w, http.StatusInternalServerError, "internal", err.Error())
			return
		}
		WriteError(w, http.StatusUnprocessableEntity, CodeOpFailed, err.Error())
		return
	}
	WriteData(w, http.StatusOK, res)
}
