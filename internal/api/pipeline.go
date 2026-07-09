package api

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
)

// This file is the daemon's pipeline surface for the manual `iris pipeline run` control
// mutation and the `iris pipeline list` read (specification section 8). POST
// /pipeline/run is a mutation, so the mux's leader gate already rejects it on a standby
// with not_leader guidance (exit 6); GET /pipeline/list is a read and works on any node.
// Like the control plane, api stays a leaf: it defines the PipelineHandler seam and the
// plain request/result shapes but reaches nothing up the stack -- the daemon supplies
// the handler that composes the dispatcher, meta reads, and exec.
//
// A manual run's business outcome (queued, succeeded, dead-lettered, or ineligible) is
// NOT an HTTP error: the route returns 200 and carries the outcome in the body's state
// field, and the CLI maps state -> exit code (0 for queued/succeeded, 4 for ineligible,
// 5 for dead-lettered). Only a genuine operation failure (an unregistered pipeline, a
// meta or exec fault) is a non-200.

// PipelineRunRequest is the body of POST /pipeline/run: the pipeline to run.
type PipelineRunRequest struct {
	// Pipeline is the pipeline name to run.
	Pipeline string `json:"pipeline"`
}

// The manual-run outcome state tokens carried in PipelineRunResult.State (the closed set
// the CLI maps to exit codes).
const (
	// PipelineRunQueued is a lane-member run enqueued as its lane's next run (exit 0).
	PipelineRunQueued = "queued"
	// PipelineRunSucceeded is an own-lane run that ran and succeeded (exit 0).
	PipelineRunSucceeded = "succeeded"
	// PipelineRunDeadLettered is a run that dead-lettered (exit 5).
	PipelineRunDeadLettered = "dead_lettered"
	// PipelineRunIneligible is a run whose depends_on gate did not open (exit 4).
	PipelineRunIneligible = "ineligible"
)

// PipelineRunResult is the success payload of POST /pipeline/run: the pipeline, the
// business outcome state (a PipelineRun* token), and an ineligibility reason when the
// gate did not open.
type PipelineRunResult struct {
	// Pipeline is the pipeline that was run.
	Pipeline string `json:"pipeline"`
	// State is the manual-run outcome (queued, succeeded, dead_lettered, ineligible).
	State string `json:"state"`
	// Reason explains an ineligible outcome; empty otherwise.
	Reason string `json:"reason,omitempty"`
}

// PipelineListItem is one row of iris pipeline list: a registered pipeline and whether
// it has a queued or running run.
type PipelineListItem struct {
	// Name is the registered pipeline's name.
	Name string `json:"name"`
	// Active reports whether the pipeline has a queued or running run.
	Active bool `json:"active"`
}

// PipelineListResult is the payload of GET /pipeline/list: the listed pipelines.
type PipelineListResult struct {
	// Pipelines are the listed pipelines (active-only by default, all with ?all=1).
	Pipelines []PipelineListItem `json:"pipelines"`
}

// PipelineHandler runs the leader-side manual run and serves the pipeline listing. The
// daemon implements it over the dispatcher, meta reads, and exec; the mux depends only
// on this interface, so api never imports dispatch/store. RunPipeline is leader-only (the
// mux gates the mutation); ListPipelines is a read served on any node.
type PipelineHandler interface {
	// RunPipeline performs one manual run of req.Pipeline and returns its outcome.
	RunPipeline(ctx context.Context, req PipelineRunRequest) (PipelineRunResult, error)
	// ListPipelines returns the pipeline listing: active-only by default, or every
	// registered pipeline when all is true.
	ListPipelines(ctx context.Context, all bool) (PipelineListResult, error)
}

// WithPipelines wires the pipeline handler the mux routes /pipeline/run and
// /pipeline/list to. A nil handler is ignored, keeping the safe default (the routes
// fault with an internal error until a real handler is installed).
func WithPipelines(h PipelineHandler) MuxOption {
	return func(m *mux) {
		if h != nil {
			m.pipelines = h
		}
	}
}

// noPipelines is the default PipelineHandler before one is wired: every call is an
// internal fault, never a silent success.
type noPipelines struct{}

func (noPipelines) RunPipeline(context.Context, PipelineRunRequest) (PipelineRunResult, error) {
	return PipelineRunResult{}, ErrControlUnavailable
}

func (noPipelines) ListPipelines(context.Context, bool) (PipelineListResult, error) {
	return PipelineListResult{}, ErrControlUnavailable
}

// servePipelineRun handles POST /pipeline/run: decode the request, run the manual op,
// and render the section-7 envelope. The leader gate ran already in ServeHTTP. A
// malformed body is 400 bad_request; an operation failure is 422 operation_failed; an
// internal fault (no handler) is 500 internal. A successful call is 200 -- the run's
// business outcome rides the result's state field, not the HTTP status.
func (m *mux) servePipelineRun(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		WriteError(w, http.StatusMethodNotAllowed, "method_not_allowed", "POST "+r.URL.Path+" only")
		return
	}
	var req PipelineRunRequest
	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()
	if err := dec.Decode(&req); err != nil {
		WriteError(w, http.StatusBadRequest, CodeBadRequest, "malformed pipeline run request body: "+err.Error())
		return
	}
	res, err := m.pipelines.RunPipeline(r.Context(), req)
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

// servePipelineList handles GET /pipeline/list: the active-only default, or every
// registered pipeline with ?all=1 (or ?all=true). It is a read, so it works on any node.
func (m *mux) servePipelineList(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		WriteError(w, http.StatusMethodNotAllowed, "method_not_allowed", "GET "+r.URL.Path+" only")
		return
	}
	all := r.URL.Query().Get("all")
	res, err := m.pipelines.ListPipelines(r.Context(), all == "1" || all == "true")
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
