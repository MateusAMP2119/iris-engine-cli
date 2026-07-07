package api

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"

	"github.com/MateusAMP2119/iris-engine-cli/internal/declare"
)

// This file is the daemon's promote surface for the `iris pipeline promote`
// control mutation (specification sections 1, 5, and 8). POST /pipeline/promote
// is a mutation, so the mux's leader gate already rejects it on a standby with
// not_leader guidance (exit 6), and its scope is control. Like the build route,
// api stays a leaf: it defines the PromoteHandler seam and the plain
// request/result shapes but reaches nothing up the stack -- the daemon supplies
// the handler that composes the dispatcher's promote op over meta and the data
// journal.
//
// Promotion is gated on built: a promote refused because the pipeline is not in
// built state (or not registered at all) is an operation failure (422), never a
// silent success. A successful promote returns 200 carrying the pipeline's
// permanent data mode plus any repeated cross-mode read warnings -- warnings
// ride the envelope, they never turn a success into a failure.

// PipelinePromoteRequest is the body of POST /pipeline/promote: the pipeline
// whose data to mark permanent.
type PipelinePromoteRequest struct {
	// Pipeline is the pipeline name to promote.
	Pipeline string `json:"pipeline"`
}

// PipelinePromoteResult is the success payload of POST /pipeline/promote: the
// promoted pipeline, its data mode after the flip (permanent), and the repeated
// cross-mode read warnings, if any upstream read dependency is still disposable.
type PipelinePromoteResult struct {
	// Pipeline is the pipeline that was promoted.
	Pipeline string `json:"pipeline"`
	// DataMode is the pipeline's per-pipeline data mode after the promote:
	// "permanent".
	DataMode string `json:"data_mode"`
	// Warnings are the advisory cross-mode read warnings the promote repeats
	// while an upstream stays disposable (specification section 5); omitted when
	// there are none.
	Warnings []declare.Warning `json:"warnings,omitempty"`
}

// PromoteHandler performs the leader-side pipeline promote. The daemon
// implements it over the dispatcher's promote op; the mux depends only on this
// interface, so api never imports dispatch/store. It is leader-only (the mux
// gates the mutation).
type PromoteHandler interface {
	// PromotePipeline promotes req.Pipeline and returns the flipped data mode
	// plus any repeated cross-mode read warnings.
	PromotePipeline(ctx context.Context, req PipelinePromoteRequest) (PipelinePromoteResult, error)
}

// WithPromote wires the promote handler the mux routes /pipeline/promote to. A
// nil handler is ignored, keeping the safe default (the route faults with an
// internal error until a real handler is installed).
func WithPromote(h PromoteHandler) MuxOption {
	return func(m *mux) {
		if h != nil {
			m.promote = h
		}
	}
}

// noPromote is the default PromoteHandler before one is wired: every call is an
// internal fault, never a silent success.
type noPromote struct{}

func (noPromote) PromotePipeline(context.Context, PipelinePromoteRequest) (PipelinePromoteResult, error) {
	return PipelinePromoteResult{}, ErrControlUnavailable
}

// servePipelinePromote handles POST /pipeline/promote: decode the request, run
// the promote op, and render the section-7 envelope. The leader gate ran already
// in ServeHTTP. A malformed body is 400 bad_request; a refused promote (un-built
// or unregistered pipeline) is 422 operation_failed carrying the gate's reason;
// an internal fault (no handler) is 500 internal. A successful promote is 200
// carrying the permanent data mode and any repeated cross-mode read warnings.
func (m *mux) servePipelinePromote(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		WriteError(w, http.StatusMethodNotAllowed, "method_not_allowed", "POST "+r.URL.Path+" only")
		return
	}
	var req PipelinePromoteRequest
	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()
	if err := dec.Decode(&req); err != nil {
		WriteError(w, http.StatusBadRequest, CodeBadRequest, "malformed pipeline promote request body: "+err.Error())
		return
	}
	res, err := m.promote.PromotePipeline(r.Context(), req)
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
