package api

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
)

// This file is the daemon's run-control surface for `iris run cancel <run>`
// (only an operator cancel frees a hung run). POST /run/cancel is a mutation,
// so the mux's leader gate rejects it on a standby with not_leader guidance
// (exit 6); its scope is control. api stays a leaf: it defines the
// RunCancelHandler seam and the plain request/result shapes but reaches nothing
// up the stack. The daemon supplies the handler that kills the run's process
// group and dead-letters it as stopped through the single meta writer.

// RunCancelRequest is the body of POST /run/cancel: exactly one of the run id or the pipeline whose latest run to stop.
type RunCancelRequest struct {
	// Run is the running run to cancel (kills its process group, dead-letters it as
	// stopped).
	Run string `json:"run,omitempty"`
	// Pipeline parks a pipeline by name: the leader resolves its latest run and stops it atomically (#202).
	Pipeline string `json:"pipeline,omitempty"`
}

// RunCancelResult is the success payload of POST /run/cancel: the cancelled run and
// its resulting terminal state (dead_lettered).
type RunCancelResult struct {
	// Run is the cancelled run id.
	Run string `json:"run"`
	// State is the run's resulting terminal state (dead_lettered).
	State string `json:"state"`
}

// RunCancelHandler performs the leader-side run cancel. The daemon implements it over
// the lane loop's in-flight table and the single writer; the mux depends only on this
// interface.
type RunCancelHandler interface {
	// CancelRun kills the run's process group and dead-letters it as stopped. An
	// unknown or already-terminal run is returned as an error (mapped to 422).
	CancelRun(ctx context.Context, run string) error
	// CancelPipeline resolves the pipeline's latest run and stops it atomically, returning the parked run id; nothing stoppable is an error (mapped to 422).
	CancelPipeline(ctx context.Context, pipeline string) (string, error)
}

// WithRunCancel wires the run-cancel handler the mux routes /run/cancel to. A nil
// handler is ignored, keeping the safe default (faults until installed).
func WithRunCancel(h RunCancelHandler) MuxOption {
	return func(m *mux) {
		if h != nil {
			m.runCancel = h
		}
	}
}

// noRunCancel is the default RunCancelHandler before one is wired: every call is an
// internal fault, never a silent success.
type noRunCancel struct{}

func (noRunCancel) CancelRun(context.Context, string) error { return ErrControlUnavailable }

func (noRunCancel) CancelPipeline(context.Context, string) (string, error) {
	return "", ErrControlUnavailable
}

// serveRunCancel handles POST /run/cancel: decode, cancel the run, render. A malformed
// body is 400; a missing run id is 400; an operation failure (unknown or already-
// terminal run) is 422; an unwired handler is 500; success is 200 with the run and its
// terminal state.
func (m *mux) serveRunCancel(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		WriteError(w, http.StatusMethodNotAllowed, "method_not_allowed", "POST "+r.URL.Path+" only")
		return
	}
	var req RunCancelRequest
	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()
	if err := dec.Decode(&req); err != nil {
		WriteError(w, http.StatusBadRequest, CodeBadRequest, "malformed run cancel request body: "+err.Error())
		return
	}
	if (req.Run == "") == (req.Pipeline == "") {
		WriteError(w, http.StatusBadRequest, CodeBadRequest, "run cancel requires exactly one of a run id or a pipeline")
		return
	}
	run := req.Run
	var err error
	if req.Pipeline != "" {
		run, err = m.runCancel.CancelPipeline(r.Context(), req.Pipeline)
	} else {
		err = m.runCancel.CancelRun(r.Context(), req.Run)
	}
	if err != nil {
		if errors.Is(err, ErrControlUnavailable) {
			WriteError(w, http.StatusInternalServerError, "internal", err.Error())
			return
		}
		WriteError(w, http.StatusUnprocessableEntity, CodeOpFailed, err.Error())
		return
	}
	WriteData(w, http.StatusOK, RunCancelResult{Run: run, State: "dead_lettered"})
}
