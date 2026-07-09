package api

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
)

const (
	// StatusClientClosedRequest is the non-standard 499 status the build route
	// returns when the caller's request context is cancelled or times out before the
	// build finishes: an aborted request, told apart from an engine-side failure.
	StatusClientClosedRequest = 499
	// CodeCanceled is the machine code for a build aborted by request-context
	// cancellation or deadline, distinct from operation_failed so a client or proxy
	// never mistakes an aborted request for a genuine build error.
	CodeCanceled = "canceled"
)

// This file is the daemon's build surface for the explicit `iris pipeline build`
// control mutation (specification sections 1, 8, and 9). POST /pipeline/build is a
// mutation, so the mux's leader gate already rejects it on a standby with
// not_leader guidance (exit 6). Like the control plane, api stays a leaf: it
// defines the BuildHandler seam and the plain request/result shapes but reaches
// nothing up the stack -- the daemon supplies the handler that composes the
// dispatcher's build op, the meta run-target read, the object store, and exec.
//
// Building is never implicit (apply never builds); this route is the one wire
// entry point that compiles anything. A successful build returns 200 carrying the
// recorded content hash; a build failure (unsupported runtime, failing toolchain,
// unregistered pipeline) is an operation failure (422), never a silent success.

// PipelineBuildRequest is the body of POST /pipeline/build: the pipeline to build.
type PipelineBuildRequest struct {
	// Pipeline is the pipeline name to build.
	Pipeline string `json:"pipeline"`
}

// PipelineBuildResult is the success payload of POST /pipeline/build: the built
// pipeline and its new artifact's identity -- the content hash the executed bytes
// are always identifiable by, and their size.
type PipelineBuildResult struct {
	// Pipeline is the pipeline that was built.
	Pipeline string `json:"pipeline"`
	// Hash is the produced binary's content hash (the new artifacts row's key and
	// the object-store key its bytes live under).
	Hash string `json:"hash"`
	// SizeBytes is the produced binary's size in bytes.
	SizeBytes int64 `json:"size_bytes"`
}

// BuildHandler performs the leader-side explicit pipeline build. The daemon
// implements it over the dispatcher's build op, meta reads, the object store, and
// exec; the mux depends only on this interface, so api never imports
// dispatch/store. It is leader-only (the mux gates the mutation).
type BuildHandler interface {
	// BuildPipeline builds req.Pipeline and returns the recorded artifact identity.
	BuildPipeline(ctx context.Context, req PipelineBuildRequest) (PipelineBuildResult, error)
}

// WithBuild wires the build handler the mux routes /pipeline/build to. A nil
// handler is ignored, keeping the safe default (the route faults with an internal
// error until a real handler is installed).
func WithBuild(h BuildHandler) MuxOption {
	return func(m *mux) {
		if h != nil {
			m.build = h
		}
	}
}

// noBuild is the default BuildHandler before one is wired: every call is an
// internal fault, never a silent success.
type noBuild struct{}

func (noBuild) BuildPipeline(context.Context, PipelineBuildRequest) (PipelineBuildResult, error) {
	return PipelineBuildResult{}, ErrControlUnavailable
}

// servePipelineBuild handles POST /pipeline/build: decode the request, run the
// build op, and render the section-7 envelope. The leader gate ran already in
// ServeHTTP. A malformed body is 400 bad_request; a build failure is 422
// operation_failed; an internal fault (no handler) is 500 internal. A successful
// build is 200 carrying the recorded content hash.
func (m *mux) servePipelineBuild(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		WriteError(w, http.StatusMethodNotAllowed, "method_not_allowed", "POST "+r.URL.Path+" only")
		return
	}
	var req PipelineBuildRequest
	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()
	if err := dec.Decode(&req); err != nil {
		WriteError(w, http.StatusBadRequest, CodeBadRequest, "malformed pipeline build request body: "+err.Error())
		return
	}
	res, err := m.build.BuildPipeline(r.Context(), req)
	if err != nil {
		switch {
		case errors.Is(err, ErrControlUnavailable):
			WriteError(w, http.StatusInternalServerError, "internal", err.Error())
		case errors.Is(err, context.Canceled), errors.Is(err, context.DeadlineExceeded):
			// The caller's request context ended before the build finished: report the
			// cancellation distinctly (499 + canceled), so an aborted request is never
			// mistaken for an engine-side build failure (422 operation_failed).
			WriteError(w, StatusClientClosedRequest, CodeCanceled, "pipeline build canceled: "+err.Error())
		default:
			WriteError(w, http.StatusUnprocessableEntity, CodeOpFailed, err.Error())
		}
		return
	}
	WriteData(w, http.StatusOK, res)
}
