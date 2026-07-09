package api

import (
	"context"
	"errors"
	"net/http"
)

// This file is the daemon's workload-show surface: GET /workload and
// `iris workload show [<pipeline>]` (specification sections 8 and the wiring
// panel contract). It renders the standing wiring as a panel (lanes with
// composer walk, artifact and data mode, run tips, per-edge live gate state),
// never a commit graph. Optional ?pipeline= zooms to that pipeline's
// neighborhood. It is a read, served on any role, and mutates nothing.
//
// The panel draws from the same walks the dispatcher uses (dispatch.BuildWalk,
// dispatch.Gate) and the existing gate ledger surface; no new meta state is
// written or stored.

// WorkloadShowResult is the GET /workload payload: the wiring panel.
type WorkloadShowResult struct {
	// Lanes is the list of lanes (sorted by name), each carrying its composer
	// walk as ordered pipelines with their wiring info.
	Lanes []LaneWiring `json:"lanes"`
}

// LaneWiring is one lane in the wiring panel: its name and the ordered list of
// its member pipelines' standing wiring.
type LaneWiring struct {
	Name      string           `json:"name"`
	Pipelines []PipelineWiring `json:"pipelines"`
}

// PipelineWiring is one pipeline's entry in the wiring panel: modes, a run tip,
// and its per-edge gate ledger (live state).
type PipelineWiring struct {
	Name     string `json:"name"`
	Folder   string `json:"folder"`
	Artifact string `json:"artifact"`
	DataMode string `json:"data_mode"`
	// RunTip is a short descriptor of the latest run (e.g. "succeeded 42" or
	// "none"); present for triage visibility.
	RunTip string `json:"run_tip,omitempty"`
	// Gate is the per-depends_on edge live gate state, in edge order (reuses
	// the EdgeVerdictView shape from pipeline show).
	Gate []EdgeVerdictView `json:"gate"`
}

// WorkloadShowHandler serves the wiring panel. The daemon implements it by
// composing the store reads with dispatch's BuildWalk and Gate over the same
// consumed ledger the runner uses. api depends only on the interface.
type WorkloadShowHandler interface {
	// ShowWorkload returns the panel. When pipeline is non-empty it zooms to
	// that pipeline's neighborhood (its lane and directly connected pipelines
	// via depends_on); empty returns the full wiring.
	ShowWorkload(ctx context.Context, pipeline string) (WorkloadShowResult, error)
}

// ErrWorkloadShowUnavailable is returned by the default (unwired) handler.
var ErrWorkloadShowUnavailable = errors.New("api: workload show not available")

// WithWorkloadShow wires the workload panel handler for GET /workload.
func WithWorkloadShow(h WorkloadShowHandler) MuxOption {
	return func(m *mux) {
		if h != nil {
			m.workloadShow = h
		}
	}
}

// noWorkloadShow is the default before wiring.
type noWorkloadShow struct{}

func (noWorkloadShow) ShowWorkload(context.Context, string) (WorkloadShowResult, error) {
	return WorkloadShowResult{}, ErrWorkloadShowUnavailable
}

// serveWorkload handles GET /workload[?pipeline=NAME]. It is a read, served
// anywhere. An unwired handler is 500; other errors are 422 operation_failed.
func (m *mux) serveWorkload(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		WriteError(w, http.StatusMethodNotAllowed, "method_not_allowed", "GET "+r.URL.Path+" only")
		return
	}
	// Accept optional pipeline for zoom; unknown params are rejected.
	q := r.URL.Query()
	pipeline := q.Get("pipeline")
	allowed := map[string]struct{}{}
	if pipeline != "" || q.Has("pipeline") {
		allowed["pipeline"] = struct{}{}
	}
	if err := checkKnownSingle(q, allowed); err != nil {
		WriteError(w, http.StatusBadRequest, string(CodeBadParam), err.Error())
		return
	}
	res, err := m.workloadShow.ShowWorkload(r.Context(), pipeline)
	if err != nil {
		if errors.Is(err, ErrWorkloadShowUnavailable) {
			WriteError(w, http.StatusInternalServerError, "internal", err.Error())
			return
		}
		WriteError(w, http.StatusUnprocessableEntity, CodeOpFailed, err.Error())
		return
	}
	WriteData(w, http.StatusOK, res)
}
