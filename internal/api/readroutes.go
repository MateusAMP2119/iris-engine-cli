package api

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
)

// This file is the E14 read-route surface: the runs collection with its
// ?include=inputs lineage attributes and the trace, gate, and impact triage
// walks an IDE-style renderer consumes. Each route is GET-only and served on
// any role (reads work anywhere). Like the other read surfaces api stays a
// leaf: it defines the seam and the wire shape but reaches nothing up the stack
// -- the daemon composes the real walk (the same one the CLI prints) behind
// each handler, and the mux renders exactly what it returns.
//
// GET /runs[?include=inputs] serves the run history. With include=inputs each row
// carries its consumed upstream ids and replayed_from as plain attributes
// (parents-per-row, never a separate edge array): one solid edge per input, the
// replay an annotation, never an edge. Like any collection route it streams as
// NDJSON when the client asks (Accept: application/x-ndjson), one row per line,
// no envelope. GET /runs/{id}/trace (direction=up|down), GET
// /pipelines/{name}/gate, and GET /dead_letters/{run_id}/impact serve the same
// walks the CLI prints, with the verdict and status enums closed by the daemon
// before they reach the wire.

// RunsHandler serves the runs collection (GET /runs and GET /runs/{id}).
// includeInputs requests embedding each run's consumed upstream ids and
// replayed_from as plain row attributes. The daemon implements it over the meta
// run reads and the run_inputs consumption ledger; the mux depends only on this
// interface.
type RunsHandler interface {
	// ListRuns returns the run history as a collection payload. When includeInputs
	// is set each row carries its consumed upstream ids and replayed_from.
	ListRuns(ctx context.Context, includeInputs bool) (any, error)
	// GetRun returns a single run by id, with the same optional lineage attributes.
	GetRun(ctx context.Context, id string, includeInputs bool) (any, error)
}

// RunTraceHandler serves GET /runs/{id}/trace?direction=up|down: the run's
// ancestry walk over run_inputs (up), or who consumed it (down). It is the same
// walk `iris run show <run> --trace [--down]` prints.
type RunTraceHandler interface {
	Trace(ctx context.Context, id, direction string) (any, error)
}

// PipelineGateHandler serves GET /pipelines/{name}/gate: the pipeline's per-edge
// depends_on gate ledger (the closed verdict set: open, up_to_date, pending,
// poisoned), the same ledger `iris pipeline show` prints.
type PipelineGateHandler interface {
	Gate(ctx context.Context, name string) (any, error)
}

// DeadImpactHandler serves GET /dead_letters/{run_id}/impact: the dead letter's
// blast radius (the closed class set: poisoned_now, pending, shielded), the same
// radius `iris deadletter show` prints.
type DeadImpactHandler interface {
	Impact(ctx context.Context, runID string) (any, error)
}

// The default (unwired) read-route faults. A route reaching its no* handler means
// the daemon has not wired the reader, so the request is a 500 internal fault
// naming the missing reader -- never a 404 (the route exists) and never a
// fabricated empty payload (the noStats/serveUnwiredRead doctrine).
var (
	// ErrRunsUnavailable signals the runs collection reader is not wired.
	ErrRunsUnavailable = errors.New("api: runs reader not wired")
	// ErrTraceUnavailable signals the run trace reader is not wired.
	ErrTraceUnavailable = errors.New("api: run trace reader not wired")
	// ErrGateUnavailable signals the pipeline gate reader is not wired.
	ErrGateUnavailable = errors.New("api: pipeline gate reader not wired")
	// ErrImpactUnavailable signals the dead letter impact reader is not wired.
	ErrImpactUnavailable = errors.New("api: dead letter impact reader not wired")
)

// noRuns is the default RunsHandler before one is wired: every read is an
// internal fault, never a silent empty payload.
type noRuns struct{}

func (noRuns) ListRuns(context.Context, bool) (any, error)       { return nil, ErrRunsUnavailable }
func (noRuns) GetRun(context.Context, string, bool) (any, error) { return nil, ErrRunsUnavailable }

// noRunTrace is the default RunTraceHandler before one is wired.
type noRunTrace struct{}

func (noRunTrace) Trace(context.Context, string, string) (any, error) {
	return nil, ErrTraceUnavailable
}

// noPipelineGate is the default PipelineGateHandler before one is wired.
type noPipelineGate struct{}

func (noPipelineGate) Gate(context.Context, string) (any, error) { return nil, ErrGateUnavailable }

// noDeadImpact is the default DeadImpactHandler before one is wired.
type noDeadImpact struct{}

func (noDeadImpact) Impact(context.Context, string) (any, error) { return nil, ErrImpactUnavailable }

// WithRuns wires the runs collection handler for GET /runs and GET /runs/{id}. A
// nil handler is ignored, keeping the safe default (the route faults internal
// until a real reader is installed).
func WithRuns(h RunsHandler) MuxOption {
	return func(m *mux) {
		if h != nil {
			m.runs = h
		}
	}
}

// WithRunTrace wires the run trace handler for GET /runs/{id}/trace. A nil handler
// is ignored.
func WithRunTrace(h RunTraceHandler) MuxOption {
	return func(m *mux) {
		if h != nil {
			m.runTrace = h
		}
	}
}

// WithPipelineGate wires the pipeline gate handler for GET /pipelines/{name}/gate.
// A nil handler is ignored.
func WithPipelineGate(h PipelineGateHandler) MuxOption {
	return func(m *mux) {
		if h != nil {
			m.pipelineGate = h
		}
	}
}

// WithDeadImpact wires the dead letter impact handler for GET
// /dead_letters/{run_id}/impact. A nil handler is ignored.
func WithDeadImpact(h DeadImpactHandler) MuxOption {
	return func(m *mux) {
		if h != nil {
			m.deadImpact = h
		}
	}
}

// serveRuns handles GET /runs[?include=inputs]. It renders the collection as
// the data envelope, or -- when the client asks for NDJSON -- one JSON row per
// line with no envelope, like any collection route. An unwired reader is a 500
// internal fault.
func (m *mux) serveRuns(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		WriteError(w, http.StatusMethodNotAllowed, "method_not_allowed", "GET "+r.URL.Path+" only")
		return
	}
	include, ok := includeInputs(w, r)
	if !ok {
		return
	}
	res, err := m.runs.ListRuns(r.Context(), include)
	if err != nil {
		WriteError(w, http.StatusInternalServerError, string(CodeInternal), err.Error())
		return
	}
	if wantsNDJSON(r) {
		writeRunsNDJSON(w, res)
		return
	}
	WriteData(w, http.StatusOK, res)
}

// serveRun handles GET /runs/{id}[?include=inputs]: a single run readout with the
// same optional lineage attributes. It is a single resource, so it never streams
// NDJSON.
func (m *mux) serveRun(w http.ResponseWriter, r *http.Request, id string) {
	if r.Method != http.MethodGet {
		WriteError(w, http.StatusMethodNotAllowed, "method_not_allowed", "GET "+r.URL.Path+" only")
		return
	}
	include, ok := includeInputs(w, r)
	if !ok {
		return
	}
	res, err := m.runs.GetRun(r.Context(), id, include)
	if err != nil {
		WriteError(w, http.StatusInternalServerError, string(CodeInternal), err.Error())
		return
	}
	WriteData(w, http.StatusOK, res)
}

// serveRunTrace handles GET /runs/{id}/trace?direction=up|down: the ancestry walk.
// direction defaults to up (the run's ancestry); down inverts it (who consumed
// this run). An unknown direction is a 400 naming it.
func (m *mux) serveRunTrace(w http.ResponseWriter, r *http.Request, id string) {
	if r.Method != http.MethodGet {
		WriteError(w, http.StatusMethodNotAllowed, "method_not_allowed", "GET "+r.URL.Path+" only")
		return
	}
	q := r.URL.Query()
	if err := checkKnownSingle(q, map[string]struct{}{"direction": {}}); err != nil {
		WriteError(w, http.StatusBadRequest, string(CodeBadParam), err.Error())
		return
	}
	direction := q.Get("direction")
	switch direction {
	case "", "up", "down":
		// up is the default ancestry walk; down inverts it.
	default:
		WriteError(w, http.StatusBadRequest, string(CodeBadParam), "param \"direction\": must be up or down")
		return
	}
	res, err := m.runTrace.Trace(r.Context(), id, direction)
	if err != nil {
		WriteError(w, http.StatusInternalServerError, string(CodeInternal), err.Error())
		return
	}
	WriteData(w, http.StatusOK, res)
}

// servePipelineGate handles GET /pipelines/{name}/gate: the depends_on gate ledger.
func (m *mux) servePipelineGate(w http.ResponseWriter, r *http.Request, name string) {
	if r.Method != http.MethodGet {
		WriteError(w, http.StatusMethodNotAllowed, "method_not_allowed", "GET "+r.URL.Path+" only")
		return
	}
	if !noParams(w, r) {
		return
	}
	res, err := m.pipelineGate.Gate(r.Context(), name)
	if err != nil {
		WriteError(w, http.StatusInternalServerError, string(CodeInternal), err.Error())
		return
	}
	WriteData(w, http.StatusOK, res)
}

// serveDeadImpact handles GET /dead_letters/{run_id}/impact: the blast radius.
func (m *mux) serveDeadImpact(w http.ResponseWriter, r *http.Request, runID string) {
	if r.Method != http.MethodGet {
		WriteError(w, http.StatusMethodNotAllowed, "method_not_allowed", "GET "+r.URL.Path+" only")
		return
	}
	if !noParams(w, r) {
		return
	}
	res, err := m.deadImpact.Impact(r.Context(), runID)
	if err != nil {
		WriteError(w, http.StatusInternalServerError, string(CodeInternal), err.Error())
		return
	}
	WriteData(w, http.StatusOK, res)
}

// includeInputs reads the runs route's sole optional param, include=inputs: the
// flag that embeds each run's consumed upstream ids and replayed_from. Any
// other param -- or include with any value other than inputs -- is a 400 naming
// it (unknown or unparseable params are rejected, never ignored). It reports
// the resolved flag and whether the request may proceed.
func includeInputs(w http.ResponseWriter, r *http.Request) (include, ok bool) {
	q := r.URL.Query()
	if err := checkKnownSingle(q, map[string]struct{}{"include": {}}); err != nil {
		WriteError(w, http.StatusBadRequest, string(CodeBadParam), err.Error())
		return false, false
	}
	switch v, present := single(q, "include"); {
	case !present:
		return false, true
	case v == "inputs":
		return true, true
	default:
		WriteError(w, http.StatusBadRequest, string(CodeBadParam), "param \"include\": only include=inputs is supported")
		return false, false
	}
}

// writeRunsNDJSON streams a runs collection payload as NDJSON: one JSON row
// object per line, no envelope. The header is set before the first row; a late
// marshal fault ends the stream, since the 200 is already committed.
func writeRunsNDJSON(w http.ResponseWriter, res any) {
	w.Header().Set("Content-Type", "application/x-ndjson")
	for _, row := range asRows(res) {
		b, err := json.Marshal(row)
		if err != nil {
			return
		}
		_, _ = w.Write(b)
		_, _ = w.Write([]byte("\n"))
		if fl, ok := w.(http.Flusher); ok {
			fl.Flush()
		}
	}
}

// asRows coerces a runs payload into an iterable row slice for NDJSON streaming.
// The handler returns any (its concrete row shape is the daemon's), so the
// production RunsCollection (an object wrapping the rows) is unwrapped to its rows,
// and both the map-slice and interface-slice forms are accepted; anything else
// streams empty. Unwrapping the collection keeps the NDJSON stream envelope-free
// (one row per line) while the enveloped read stays { "data": { "runs": [...] } }.
func asRows(res any) []any {
	switch rows := res.(type) {
	case RunsCollection:
		out := make([]any, len(rows.Runs))
		for i, r := range rows.Runs {
			out[i] = r
		}
		return out
	case []any:
		return rows
	case []map[string]any:
		out := make([]any, len(rows))
		for i, r := range rows {
			out[i] = r
		}
		return out
	default:
		return nil
	}
}
