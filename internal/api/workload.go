package api

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
)

// This file is the daemon's workload surface for `iris workload wipe
// [<pipeline>]`. POST /workload/wipe is a mutation, so the mux's leader gate
// already rejects it on a standby with not_leader guidance (exit 6); its scope
// is control. api stays a leaf: it defines the WipeHandler seam and the plain
// request/result shapes but reaches nothing up the stack. The daemon supplies
// the handler that reads attribution, plans scope, and executes the revert over
// the data database via pg.ExecuteWipe.
//
// Confirmation: the request carries Confirm (from --yes/--force or interactive
// prompt) for the dev-loop gate; the handler itself performs the wipe when
// admitted.

// WorkloadWipeRequest is the body of POST /workload/wipe: optional pipeline to
// scope the wipe to one pipeline's writes (bare = all wipe-eligible), plus the
// explicit confirm the destructive gate requires.
type WorkloadWipeRequest struct {
	// Pipeline, when non-empty, narrows the wipe to that pipeline's journal
	// entries (through their runs).
	Pipeline string `json:"pipeline,omitempty"`
	// Confirm is the explicit confirmation.
	Confirm bool `json:"confirm,omitempty"`
	// Force requests that soft-blocks be overridden (--force): in-flight runs on
	// the wipe's scope are cancelled instead of refusing. Without it (--yes or an
	// interactive confirmation) every soft-block is honored.
	Force bool `json:"force,omitempty"`
}

// WorkloadWipeResult is the success payload of POST /workload/wipe: the counts
// of entries reverted (wiped) and conflict-skipped. Conflicts (when any) may
// be rendered by CLI from other reads or future enrichment; the counts are the
// primary outcome.
type WorkloadWipeResult struct {
	// Wiped is the number of journal entries whose writes were reverted.
	Wiped int `json:"wiped"`
	// Skipped is the number of open entries conflict-skipped (later write
	// still in the row's value).
	Skipped int `json:"skipped"`
}

// WipeHandler performs the leader-side workload wipe. The daemon implements it
// over dispatch (gate), a run->pipeline attribution reader, and pg.ExecuteWipe;
// the mux depends only on this interface.
type WipeHandler interface {
	// Wipe executes the wipe per req (scoped or bare) and returns the summary
	// counts. Refusals (e.g. un-admitted confirmation or in-flight) are
	// returned as errors (mapped to 422 by serve).
	Wipe(ctx context.Context, req WorkloadWipeRequest) (WorkloadWipeResult, error)
}

// WithWipe wires the wipe handler the mux routes /workload/wipe to. A nil
// handler is ignored, keeping the safe default (faults until installed).
func WithWipe(h WipeHandler) MuxOption {
	return func(m *mux) {
		if h != nil {
			m.wipe = h
		}
	}
}

// noWipe is the default WipeHandler before one is wired: every call is an
// internal fault, never a silent success.
type noWipe struct{}

func (noWipe) Wipe(context.Context, WorkloadWipeRequest) (WorkloadWipeResult, error) {
	return WorkloadWipeResult{}, ErrControlUnavailable
}

// serveWorkloadWipe handles POST /workload/wipe: decode, run the wipe, render.
// A malformed body is 400; an operation failure (gate, no scope etc) is 422;
// internal is 500; success is 200 with the counts.
func (m *mux) serveWorkloadWipe(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		WriteError(w, http.StatusMethodNotAllowed, "method_not_allowed", "POST "+r.URL.Path+" only")
		return
	}
	var req WorkloadWipeRequest
	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()
	if err := dec.Decode(&req); err != nil {
		WriteError(w, http.StatusBadRequest, CodeBadRequest, "malformed workload wipe request body: "+err.Error())
		return
	}
	res, err := m.wipe.Wipe(r.Context(), req)
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
