package api

import (
	"net/http"
	"strings"
)

// This file mounts the fixed engine-state route roster and the data-surface
// routes: the meta-roster collections with their item sub-routes --
// /pipelines(/{name}), /runs(/{id}), /dead_letters(/{run_id}), /lanes,
// /dependencies, /leader, /ps, /healthz, /provenance/{schema}/{table}/{pk}
// -- the E14 graph and triage sub-routes (/workload, /runs/{id}/trace,
// /pipelines/{name}/gate, /dead_letters/{run_id}/impact), and the data surface
// (/data/{schema}/{table}, /q/{endpoint}). Every route is GET-only; the roster
// is a closed switch, so it cannot drift at runtime, and anything outside it
// falls through to the mux's not_found envelope.
//
// The mounting, the auth split, and the status matrix live here; each route's
// payload comes from a reader seam the daemon wires in: a seam interface in
// api, the daemon supplying the pgx-backed implementation. Most of the roster
// serves a real payload today -- /leader off
// the role reporter below, and /runs(/{id}), /runs/{id}/trace,
// /pipelines/{name}/gate, /dead_letters/{run_id}/impact, /workload,
// /provenance/..., /data, and /q off their wired readers (/healthz and /ps
// are exact-path routes on the mux itself, api.go and ps.go). The exceptions
// are the /pipelines and /dead_letters collections with their item routes,
// /lanes, and /dependencies: nothing supplies a reader for those, so they answer
// through serveUnwiredRead -- mounted, scope-checked, GET-enforced, but faulting
// with the 500 internal envelope (the unwired-seam doctrine: an unwired
// seam is an internal fault, never a silent empty payload).

// LeaderReport is the payload of GET /leader: the node's leadership role and
// the leader's address as this node knows it, reported on leader and standby
// alike. Leader is "" on the leader itself (it is the leader) and on a
// candidate that has not seen a leader yet.
type LeaderReport struct {
	// Role is the leadership role: leader, standby, or unknown.
	Role string `json:"role"`
	// Leader is the current leader's address as known here, or "".
	Leader string `json:"leader"`
}

// serveRoster matches the read-surface roster routes and serves the matched
// one, reporting whether the path matched. A false return is the mux's cue to
// answer not_found: nothing here writes a 404, so the roster stays additive
// under the mux's existing exact-path routes (which run first).
func (m *mux) serveRoster(w http.ResponseWriter, r *http.Request) bool {
	segs, ok := splitPath(r.URL.Path)
	if !ok {
		return false
	}
	switch segs[0] {
	case "pipelines":
		switch {
		case len(segs) == 1:
			serveUnwiredRead(w, r, "pipelines")
		case len(segs) == 2:
			serveUnwiredRead(w, r, "pipeline")
		case len(segs) == 3 && segs[2] == "gate":
			m.servePipelineGate(w, r, segs[1])
		default:
			return false
		}
	case "runs":
		switch {
		case len(segs) == 1:
			m.serveRuns(w, r)
		case len(segs) == 2:
			m.serveRun(w, r, segs[1])
		case len(segs) == 3 && segs[2] == "trace":
			m.serveRunTrace(w, r, segs[1])
		case len(segs) == 3 && segs[2] == "logs":
			m.serveRunLogs(w, r, segs[1])
		default:
			return false
		}
	case "dead_letters":
		switch {
		case len(segs) == 1:
			serveUnwiredRead(w, r, "dead letters")
		case len(segs) == 2:
			serveUnwiredRead(w, r, "dead letter")
		case len(segs) == 3 && segs[2] == "impact":
			m.serveDeadImpact(w, r, segs[1])
		default:
			return false
		}
	case "lanes", "dependencies":
		if len(segs) != 1 {
			return false
		}
		serveUnwiredRead(w, r, segs[0])
	case "workload":
		if len(segs) != 1 {
			return false
		}
		m.serveWorkload(w, r)
	case "leader":
		if len(segs) != 1 {
			return false
		}
		m.serveLeader(w, r)
	case "provenance":
		// /provenance/{schema}/{table}/{pk}: all three params required.
		if len(segs) != 4 {
			return false
		}
		m.serveProvenance(w, r, segs[1], segs[2], segs[3])
	case "data":
		// /data/{schema}/{table}: the raw ad-hoc table read (dataroute.go),
		// executing through the shared read pool as the calling PAT's role.
		if len(segs) != 3 {
			return false
		}
		m.serveData(w, r, segs[1], segs[2])
	case "q":
		// /q/{endpoint}: the declared endpoint read. endpoint.go owns the
		// live-shape checkout and lifecycle; PoolReader (readexec.go) is the
		// production reader over the shared read pool.
		if len(segs) != 2 {
			return false
		}
		m.serveEndpoint(w, r, segs[1])
	default:
		return false
	}
	return true
}

// splitPath splits a request path into its segments, refusing a path with an
// empty segment (a double slash or trailing slash): an empty path parameter
// never matches a roster pattern, so /pipelines//gate is a 404, not a route
// with an empty name.
func splitPath(path string) ([]string, bool) {
	p, ok := strings.CutPrefix(path, "/")
	if !ok || p == "" {
		return nil, false
	}
	segs := strings.Split(p, "/")
	for _, s := range segs {
		if s == "" {
			return nil, false
		}
	}
	return segs, true
}

// serveLeader handles GET /leader: the leadership readout, reported identically
// on leader and standby ("GET /healthz / GET /leader report role on both"). It
// is a read, served on any role.
func (m *mux) serveLeader(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		WriteError(w, http.StatusMethodNotAllowed, "method_not_allowed", "GET "+r.URL.Path+" only")
		return
	}
	if !noParams(w, r) {
		return
	}
	WriteData(w, http.StatusOK, LeaderReport{
		Role:   string(m.role.Role()),
		Leader: m.role.LeaderHint(),
	})
}

// serveUnwiredRead answers a roster route that has no reader: the route is
// mounted (scope-checked by authorize, GET-enforced here) but nothing supplies
// its payload, so a request faults with the 500 internal envelope naming the
// missing reader -- never a 404 (the route exists) and never a fabricated empty
// payload (the unwired-seam doctrine).
func serveUnwiredRead(w http.ResponseWriter, r *http.Request, reader string) {
	if r.Method != http.MethodGet {
		WriteError(w, http.StatusMethodNotAllowed, "method_not_allowed", "GET "+r.URL.Path+" only")
		return
	}
	WriteError(w, http.StatusInternalServerError, "internal", "api: "+reader+" reader not wired")
}

// noParams enforces a paramless route's wire grammar (an unknown or repeated
// param is a 400 naming it, never ignored): any query param on a route that
// takes none is a 400 bad_param, written here. It reports whether the request
// may proceed.
func noParams(w http.ResponseWriter, r *http.Request) bool {
	q := r.URL.Query()
	if len(q) == 0 {
		return true
	}
	if err := checkKnownSingle(q, nil); err != nil {
		WriteError(w, http.StatusBadRequest, string(CodeBadParam), err.Error())
		return false
	}
	return true
}
