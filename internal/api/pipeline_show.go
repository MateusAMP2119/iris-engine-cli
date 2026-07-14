package api

import (
	"context"
	"errors"
	"net/http"
)

// This file is the daemon's pipeline-show surface: GET /pipeline/show, the
// single-pipeline readout `iris pipeline show <name>` prints. It reports the
// pipeline's resolved declaration, its Postgres role and field-level grants,
// its recent runs, and the gate ledger -- the per-edge depends_on verdict from
// the closed set (open, up_to_date, pending, poisoned). It is a read, served on
// any role (reads work anywhere), and mutates nothing.
//
// Like the other read surfaces, api stays a leaf: it defines the seam and the wire
// shapes but reaches nothing up the stack. The gate verdict rides the wire as a
// plain string (the closed-set token), so api never imports dispatch; the daemon
// maps its dispatch.Verdict onto the token.

// PipelineShowResult is the GET /pipeline/show document: a pipeline's resolved
// declaration, its role and grants, its recent runs, and the gate ledger. It
// carries no clock readout -- no recorded_at, no last-seen, no timestamp --
// since connection state is the only liveness signal.
type PipelineShowResult struct {
	// Name is the pipeline's name.
	Name string `json:"name"`
	// Folder is the pipeline's workspace-relative folder (the declaration's folder).
	Folder string `json:"folder"`
	// Run is the declared run argv (the resolved declaration's command vector).
	Run []string `json:"run"`
	// Artifact is the pipeline's artifact mode (source or built).
	Artifact string `json:"artifact"`
	// DataMode is the pipeline's data mode (disposable or permanent).
	DataMode string `json:"data_mode"`
	// DependsOn are the pipeline's declared upstream dependencies, in edge order.
	DependsOn []string `json:"depends_on"`
	// Role is the pipeline's engine-managed Postgres role name.
	Role string `json:"role"`
	// Grants are the role's field-level access grants, in stable order. Always
	// present, possibly empty.
	Grants []GrantView `json:"grants"`
	// RecentRuns are the pipeline's runs, newest last (ordering identity, never a
	// clock). Always present, possibly empty.
	RecentRuns []RunView `json:"recent_runs"`
	// GateLedger is the per-edge depends_on verdict, in edge order: the gate's
	// read surface. Always present, possibly empty (a pipeline with no depends_on
	// edges has an empty ledger).
	GateLedger []EdgeVerdictView `json:"gate_ledger"`
}

// GrantView is one field-level grant in the pipeline-show readout: the schema,
// table, field, and access kind (read or write) the pipeline's role holds.
type GrantView struct {
	// Schema is the grant's schema.
	Schema string `json:"schema"`
	// Table is the grant's table.
	Table string `json:"table"`
	// Field is the single column the grant covers.
	Field string `json:"field"`
	// Access is the grant's access kind (read or write).
	Access string `json:"access"`
}

// RunView is one run in the pipeline-show recent-runs list: its id, lifecycle
// state, and exit code when it carries one. It carries no clock field -- ordering
// is by the run's identity, never a timestamp.
type RunView struct {
	// ID is the run's meta id.
	ID string `json:"id"`
	// State is the run's lifecycle state.
	State string `json:"state"`
	// ExitCode is the run's exit code when it carries one; null otherwise.
	ExitCode *int `json:"exit_code"`
}

// EdgeVerdictView is one row of the gate ledger: the upstream, the resolved
// verdict (a closed-set token: open, up_to_date, pending, poisoned), and the id of
// the upstream's most recent run the verdict was computed against.
type EdgeVerdictView struct {
	// Upstream is the upstream pipeline's name.
	Upstream string `json:"upstream"`
	// Verdict is the closed-set gate verdict token.
	Verdict string `json:"verdict"`
	// LatestRunID is the upstream's most recent run id the verdict resolved
	// against, or "" when the upstream has produced no run.
	LatestRunID string `json:"latest_run_id"`
}

// PipelineShowHandler serves the single-pipeline readout. The daemon implements it
// over the meta reads (declaration, grants, runs) and the depends_on gate; the mux
// depends only on this interface, so api never imports store/dispatch. It is a
// read, served on any node.
type PipelineShowHandler interface {
	// ShowPipeline returns the readout for the named pipeline. It returns an error
	// when the pipeline is not registered.
	ShowPipeline(ctx context.Context, name string) (PipelineShowResult, error)
}

// ErrPipelineShowUnavailable is returned by the default (unwired) pipeline-show
// handler: a show read reached the mux but no handler is installed. The daemon
// wires the handler at construction, so it is an internal fault.
var ErrPipelineShowUnavailable = errors.New("api: pipeline show not available")

// WithPipelineShow wires the pipeline-show handler the mux routes GET
// /pipeline/show to. A nil handler is ignored, keeping the safe default (the route
// faults with an internal error until a real handler is installed).
func WithPipelineShow(h PipelineShowHandler) MuxOption {
	return func(m *mux) {
		if h != nil {
			m.pipelineShow = h
		}
	}
}

// noPipelineShow is the default PipelineShowHandler before one is wired: every
// read is an internal fault, never a silent empty readout.
type noPipelineShow struct{}

func (noPipelineShow) ShowPipeline(context.Context, string) (PipelineShowResult, error) {
	return PipelineShowResult{}, ErrPipelineShowUnavailable
}

// servePipelineShow handles GET /pipeline/show?name=<name>: run the wired
// handler and render the data envelope. It is a read, served on any node. A
// missing name is 400 bad_param; an unwired handler is 500 internal; an
// unregistered pipeline (or any read error) is 422 operation_failed.
func (m *mux) servePipelineShow(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		WriteError(w, http.StatusMethodNotAllowed, "method_not_allowed", "GET "+r.URL.Path+" only")
		return
	}
	name := r.URL.Query().Get("name")
	if name == "" {
		WriteError(w, http.StatusBadRequest, string(CodeBadParam), "pipeline show requires a name")
		return
	}
	res, err := m.pipelineShow.ShowPipeline(r.Context(), name)
	if err != nil {
		if errors.Is(err, ErrPipelineShowUnavailable) {
			WriteError(w, http.StatusInternalServerError, "internal", err.Error())
			return
		}
		WriteError(w, http.StatusUnprocessableEntity, CodeOpFailed, err.Error())
		return
	}
	WriteData(w, http.StatusOK, res)
}
