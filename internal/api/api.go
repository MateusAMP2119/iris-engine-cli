// Package api is the one net/http handler the Iris daemon's two listeners serve:
// the control plane and the read API on a single mux. It is deliberately a leaf:
// it renders exactly the resource-shaped HTTP/JSON views the CLI's read commands
// print and never reaches back up into the daemon, dispatcher, or a database.
//
// The mux mounts the full read roster (roster.go): the engine-state routes and
// their item sub-routes, the data surface (/data, /q), and the control-plane
// mutations, with the closed error envelope for unknown routes and non-GET
// methods. A route whose reader is not wired is still mounted, but answers the
// internal-fault envelope rather than a fabricated payload.
//
// Authorization is a two-step split. Transport: a unix-socket request is
// ambient (local, filesystem-guarded), while a TCP request must present a PAT
// -- that half lives in RequirePAT (auth.go), which the daemon wraps around
// this mux for the TCP listener only, attaching the PAT's resolved authority
// to the request. Scope: every route is then scope-checked against that
// authority (authority.go) -- read for engine state, data for /data and /q,
// control for mutations -- so a data-only PAT sees no engine internals and a
// read-only PAT no table data.
package api

import (
	"encoding/json"
	"net/http"
)

// Envelope is the read-API success document: {"data": ...}. Every non-streaming
// success response is one Envelope on the wire.
type Envelope struct {
	// Data is the response payload, shaped per route.
	Data any `json:"data"`
	// Page is the pagination half on paged collection responses ({"page":
	// {"next_after": <key|null>, "limit": <n>}}); nil -- and absent on the wire --
	// everywhere else.
	Page *Page `json:"page,omitempty"`
}

// ErrorEnvelope is the read-API error document:
// {"error": {"code": ..., "message": ...}} with a code drawn from the closed set
// (bad_param, unauthorized, forbidden, not_found, method_not_allowed, internal).
type ErrorEnvelope struct {
	// Error is the error object.
	Error ErrorBody `json:"error"`
}

// ErrorBody is the error object inside an ErrorEnvelope.
type ErrorBody struct {
	// Code is a closed-set machine code.
	Code string `json:"code"`
	// Message is the human-readable detail.
	Message string `json:"message"`
}

// Health is the payload of GET /healthz: the daemon's liveness and its leadership
// role. Role is leader, standby, or unknown (before election resolves), reported on
// both listeners so the CLI's daemon-reachability check and any HTTP consumer
// can read the daemon's role.
type Health struct {
	// Status is "ok" when the daemon is serving.
	Status string `json:"status"`
	// Role is the leadership role: leader, standby, or unknown.
	Role string `json:"role"`
}

// MuxOption configures the mux at construction.
type MuxOption func(*mux)

// WithRole wires the leadership role reporter the mux consults per request: it
// reports the role on GET /healthz and gates mutations to the leader. A nil
// reporter is ignored, keeping the safe default (unknown role, mutations rejected).
func WithRole(r RoleReporter) MuxOption {
	return func(m *mux) {
		if r != nil {
			m.role = r
		}
	}
}

// NewMux returns the single http.Handler both daemon listeners serve. It owns the
// route roster; a standby rejects mutations with leader guidance, unknown routes
// and disallowed methods get the closed error envelope, and GET /healthz reports
// liveness and the leadership role. With no WithRole option the role is unknown, so
// mutations are rejected until election confirms a leader.
func NewMux(opts ...MuxOption) http.Handler {
	m := &mux{
		role:         unknownRole{},
		control:      noControl{},
		pipelines:    noPipelines{},
		build:        noBuild{},
		promote:      noPromote{},
		wipe:         noWipe{},
		runCancel:    noRunCancel{},
		ps:           noPs{},
		inspect:      noInspect{},
		pipelineShow: noPipelineShow{},
		workloadShow: noWorkloadShow{},
		provenance:   noProvenance{},
		runs:         noRuns{},
		runTrace:     noRunTrace{},
		runLogs:      noRunLogs{},
		pipelineGate: noPipelineGate{},
		deadImpact:   noDeadImpact{},
		endpointCtl:  noEndpointControl{},
		patMint:      noPATMint{},
		replay:       noReplay{},
		drain:        noDrain{},
		catalog:      noCatalog{},
		catalogList:  noCatalogList{},
	}
	for _, o := range opts {
		o(m)
	}
	return m
}

// mux is the daemon's route table. It is a hand-rolled matcher (rather than
// http.ServeMux) so unknown routes and disallowed methods return the read-API
// error envelope, not net/http's plain-text 404/405. It consults role to gate
// mutations to the leader and routes the control-plane mutations to the injected
// ControlHandler.
type mux struct {
	role         RoleReporter
	control      ControlHandler
	pipelines    PipelineHandler
	build        BuildHandler
	promote      PromoteHandler
	wipe         WipeHandler
	runCancel    RunCancelHandler
	ps           PsHandler
	inspect      InspectHandler
	pipelineShow PipelineShowHandler
	workloadShow WorkloadShowHandler
	provenance   ProvenanceHandler
	// runs, runTrace, pipelineGate, and deadImpact are the E14 read-route seams
	// (readroutes.go): the runs collection with its ?include=inputs lineage
	// attributes and the trace, gate, and impact triage walks. Each defaults to
	// its no* handler (an unwired seam faults with the internal envelope, never a
	// silent empty payload), so the daemon wires the pgx-backed implementation.
	runs         RunsHandler
	runTrace     RunTraceHandler
	runLogs      RunLogsHandler
	pipelineGate PipelineGateHandler
	deadImpact   DeadImpactHandler
	// endpointCtl runs the leader-side POST /endpoint/apply (endpointapply.go);
	// patMint runs the leader-side POST /pat/create (patcreate.go). Both default to
	// an internal-fault handler until the daemon installs the real one on leadership.
	endpointCtl EndpointControlHandler
	patMint     PATMintHandler
	// replay and drain are the two leader-only dead-letter dispositions
	// (deadletter.go): POST /deadletter/replay and POST /deadletter/drain. Each
	// defaults to its no* handler (an unwired mutation faults), so the leader wires
	// the real handler on winning leadership.
	replay ReplayHandler
	drain  DrainHandler
	// catalog runs the leader-side POST /catalog/install (catalog.go, #217);
	// catalogList serves the GET /catalog pack listing on any role (catalogread.go, #219).
	catalog     CatalogHandler
	catalogList CatalogListHandler
	// endpoints and qreader are the /q serving seams (endpoint.go): the live
	// compiled-shape source and the read executor. Both default nil (unwired):
	// /q then answers the internal-fault envelope, per the unwired-seam doctrine.
	endpoints EndpointSource
	qreader   EndpointReader
	// datasrc and readexec are the /data serving seams (dataroute.go,
	// readexec.go): the declared-table shape source and the shared read pool's
	// statement executor. Both default nil (unwired): /data then answers the
	// internal-fault envelope, per the unwired-seam doctrine.
	datasrc  DataSource
	readexec ReadExecutor
}

// ServeHTTP gates mutations to the leader, scope-checks the request's authority
// against its route, then dispatches the request, or returns the closed-code
// error envelope for an unknown route or a disallowed method. A mutating request
// on any non-leader role is rejected with the not_leader envelope and leader
// guidance before it ever reaches a route: standbys reject mutations, reads work
// anywhere. Exact-path routes match first; the parameterized read roster
// (roster.go) matches next; everything else is not_found.
func (m *mux) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if !isSafeMethod(r.Method) && m.role.Role() != RoleLeader {
		WriteNotLeader(w, m.role.LeaderHint())
		return
	}
	if !m.authorize(w, r) {
		return
	}
	switch r.URL.Path {
	case "/healthz":
		m.serveHealthz(w, r)
	case "/apply":
		m.serveApply(w, r)
	case "/destroy":
		m.serveDestroy(w, r)
	case "/deadletter/drain":
		m.serveDeadletterDrain(w, r)
	case "/deadletter/replay":
		m.serveDeadletterReplay(w, r)
	case "/pipeline/build":
		m.servePipelineBuild(w, r)
	case "/pipeline/promote":
		m.servePipelinePromote(w, r)
	case "/pipeline/run":
		m.servePipelineRun(w, r)
	case "/workload/wipe":
		m.serveWorkloadWipe(w, r)
	case "/run/cancel":
		m.serveRunCancel(w, r)
	case "/endpoint/apply":
		m.serveEndpointApply(w, r)
	case "/catalog/install":
		m.serveCatalogInstall(w, r)
	case "/catalog":
		m.serveCatalogList(w, r)
	case "/pat/create":
		m.servePATCreate(w, r)
	case "/pipeline/list":
		m.servePipelineList(w, r)
	case "/pipeline/show":
		m.servePipelineShow(w, r)
	case "/ps":
		m.servePs(w, r)
	case "/inspect":
		m.serveInspect(w, r)
	default:
		if m.serveRoster(w, r) {
			return
		}
		// Deliberately unrouted: /metrics stays a not_found like any unknown
		// route (no metrics endpoint in core; a monitor consumes GET /ps
		// instead).
		WriteError(w, http.StatusNotFound, "not_found", "no such route: "+r.URL.Path)
	}
}

// serveHealthz handles GET /healthz: the liveness-plus-role probe the CLI's
// daemon-reachability check hits, served identically on every role.
func (m *mux) serveHealthz(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		WriteError(w, http.StatusMethodNotAllowed, "method_not_allowed", "GET /healthz only")
		return
	}
	if !noParams(w, r) {
		return
	}
	WriteData(w, http.StatusOK, Health{Status: "ok", Role: string(m.role.Role())})
}

// WriteData writes a success envelope wrapping v at the given status. It is the
// one place a data response is serialized, so every route emits the identical
// {"data": ...} shape.
func WriteData(w http.ResponseWriter, status int, v any) {
	writeJSON(w, status, Envelope{Data: v})
}

// WriteError writes an error envelope with the given closed-set code and detail
// at the given status. It is the one place an error response is serialized, so
// every failure emits the identical {"error": {...}} shape.
func WriteError(w http.ResponseWriter, status int, code, message string) {
	writeJSON(w, status, ErrorEnvelope{Error: ErrorBody{Code: code, Message: message}})
}

// writeJSON sets the JSON content type, writes the status, and encodes v. A
// late encode error cannot change the already-sent status, so it is dropped
// rather than wrapped.
func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}
