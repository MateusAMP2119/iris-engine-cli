package api

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
)

// This file is the dead-letter mutation plane's HTTP surface: POST
// /deadletter/replay and the real POST /deadletter/drain, plus the wire types
// the CLI's `iris deadletter replay`/`drain` marshal and decode. Both are
// leader-only mutations -- ServeHTTP gates them to the leader before routing,
// and the scope check (authority.go) demands the control scope over TCP -- so
// the handlers here run only on the leader. GET /dead_letters/{run}/impact (the
// blast readout `iris deadletter show` renders) lives in readroutes.go; this
// file owns the two operator dispositions.

// ReplayRequest is the scope of a replay: exactly one of a single run, one
// pipeline's outstanding entries, or every outstanding entry (bare invocation is a
// usage error the CLI refuses before it ever reaches here).
type ReplayRequest struct {
	// Run is the single dead-lettered run to replay, resolved to its root cause.
	Run string `json:"run,omitempty"`
	// Pipeline scopes to one pipeline's outstanding entries (--pipeline).
	Pipeline string `json:"pipeline,omitempty"`
	// All scopes to every outstanding entry (--all).
	All bool `json:"all,omitempty"`
}

// ReplayedRun pairs a replaced dead-lettered run with the fresh replacement minted
// on current data, plus the replacement's replay lineage (replayed_from, the replaced
// run).
type ReplayedRun struct {
	// ReplacedRun is the dead-lettered run this replay replaced (its worklist entry
	// was removed when the replacement minted).
	ReplacedRun string `json:"replaced_run"`
	// ReplacementRun is the fresh run minted on current data (cause replay).
	ReplacementRun string `json:"replacement_run"`
	// ReplayedFrom is the replacement's runs.replayed_from: the replaced run.
	ReplayedFrom string `json:"replayed_from,omitempty"`
}

// ReplayResult is the leader's reply to a replay: the replacements it minted and,
// separately, any whose fresh run itself dead-lettered again (the CLI maps a non-empty
// DeadLettered to exit 5).
type ReplayResult struct {
	// Replayed are the root causes replayed, each with its replacement run.
	Replayed []ReplayedRun `json:"replayed"`
	// DeadLettered are the replays whose replacement itself dead-lettered again.
	DeadLettered []ReplayedRun `json:"dead_lettered"`
}

// DrainRequest is the scope of a drain: exactly one of a single run, one pipeline's
// entries, or every entry, plus the explicit confirm the destructive op demands over
// the API.
type DrainRequest struct {
	// Run is the single dead-lettered run to drain.
	Run string `json:"run,omitempty"`
	// Pipeline scopes to one pipeline's outstanding entries.
	Pipeline string `json:"pipeline,omitempty"`
	// All scopes to every outstanding entry.
	All bool `json:"all,omitempty"`
	// Force requests that soft-blocks be overridden (--force): in-flight runs on
	// the drain's scope are cancelled instead of refusing. Without it (--yes or an
	// interactive confirmation) every soft-block is honored.
	Force bool `json:"force,omitempty"`
}

// DrainResult is the leader's reply to a drain: the runs whose worklist entries were
// discarded (drained runs can never be replayed -- the entry was the replay ticket).
type DrainResult struct {
	// Drained are the run ids whose worklist entries the drain discarded.
	Drained []string `json:"drained"`
}

// DeadImpactPayload is the body of GET /dead_letters/{run}/impact: the blast
// radius `iris deadletter show` renders. It names the root cause the entry
// walks to and classifies every pipeline in the root's blast neighborhood from
// the closed class set (poisoned_now / pending / shielded), with composer-only
// lane neighbors marked untouched (order is not dependency).
type DeadImpactPayload struct {
	// Run is the dead-lettered run the readout was requested for.
	Run string `json:"run"`
	// Reason is the entry's own reason (failed / stopped / upstream_dead_lettered).
	Reason string `json:"reason"`
	// RootCause is the run and pipeline the entry walks failed_upstream to.
	RootCause DeadImpactRoot `json:"root_cause"`
	// Impacts classifies each pipeline in the blast neighborhood.
	Impacts []DeadImpactItem `json:"impacts"`
}

// DeadImpactRoot is the root cause a dead-letter entry walks to.
type DeadImpactRoot struct {
	// Run is the root-cause run's id.
	Run string `json:"run"`
	// Pipeline is the root-cause run's pipeline.
	Pipeline string `json:"pipeline"`
}

// DeadImpactItem is one pipeline's blast classification.
type DeadImpactItem struct {
	// Pipeline is the classified pipeline's name.
	Pipeline string `json:"pipeline"`
	// Class is the blast class: poisoned_now, pending, shielded, or untouched.
	Class string `json:"class"`
}

// ReplayHandler is the leader-side replay seam: it resolves the scope to root
// causes, mints each a replacement on current data, and discards the propagated
// entries that walked to a replayed root as superseded.
type ReplayHandler interface {
	Replay(ctx context.Context, req ReplayRequest) (ReplayResult, error)
}

// DrainHandler is the leader-side drain seam: it resolves the scope to the
// exact outstanding entries and discards their worklist rows.
type DrainHandler interface {
	Drain(ctx context.Context, req DrainRequest) (DrainResult, error)
}

// ErrReplayUnavailable and ErrDrainUnavailable are returned by the default (unwired)
// handlers: a mutation reached the mux but no handler is installed. It is unreachable
// in practice -- the leader installs the handlers before it reports the leader role,
// and the mux gates mutations to the leader -- so it is an internal fault.
var (
	ErrReplayUnavailable = errors.New("api: dead-letter replay plane not available")
	ErrDrainUnavailable  = errors.New("api: dead-letter drain plane not available")
)

// noReplay is the default ReplayHandler before one is wired.
type noReplay struct{}

func (noReplay) Replay(context.Context, ReplayRequest) (ReplayResult, error) {
	return ReplayResult{}, ErrReplayUnavailable
}

// noDrain is the default DrainHandler before one is wired.
type noDrain struct{}

func (noDrain) Drain(context.Context, DrainRequest) (DrainResult, error) {
	return DrainResult{}, ErrDrainUnavailable
}

// WithReplay wires the leader-side replay handler for POST /deadletter/replay. A nil
// handler is ignored.
func WithReplay(h ReplayHandler) MuxOption {
	return func(m *mux) {
		if h != nil {
			m.replay = h
		}
	}
}

// WithDrain wires the leader-side drain handler for POST /deadletter/drain. A nil
// handler is ignored.
func WithDrain(h DrainHandler) MuxOption {
	return func(m *mux) {
		if h != nil {
			m.drain = h
		}
	}
}

// serveDeadletterReplay handles POST /deadletter/replay: it decodes the scope, resolves
// and replays through the leader-side handler, and writes the replay result. The scope
// is validated (exactly one of run/pipeline/all); a bare scope is a bad request (the CLI
// refuses it before POSTing, but the server never trusts that). A handler error is an
// operation failure (exit 4), never an internal fault: the resolution can legitimately
// fail (a run absent from the worklist, a broken failed_upstream chain).
func (m *mux) serveDeadletterReplay(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		WriteError(w, http.StatusMethodNotAllowed, "method_not_allowed", "POST "+r.URL.Path+" only")
		return
	}
	var req ReplayRequest
	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()
	if err := dec.Decode(&req); err != nil {
		WriteError(w, http.StatusBadRequest, CodeBadRequest, "malformed replay request body: "+err.Error())
		return
	}
	if req.Run == "" && req.Pipeline == "" && !req.All {
		WriteError(w, http.StatusUnprocessableEntity, CodeOpFailed, "replay requires a scope: <run>, --pipeline, or --all")
		return
	}
	res, err := m.replay.Replay(r.Context(), req)
	if err != nil {
		WriteError(w, http.StatusUnprocessableEntity, CodeOpFailed, err.Error())
		return
	}
	WriteData(w, http.StatusOK, res)
}
