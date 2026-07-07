// Package api is the one net/http handler the Iris daemon's two listeners serve:
// the control plane and the read API on a single mux (specification sections 2
// and 7). It is deliberately a leaf: it renders exactly the resource-shaped
// HTTP/JSON views the CLI's read commands print and never reaches back up into
// the daemon, dispatcher, or a database.
//
// The route roster is filled by the read-API epics (E09/E12/E14); today the mux
// carries GET /healthz -- the liveness probe both the CLI's daemon-reachability
// check and the conformance harness hit -- plus the closed error envelope for
// unknown routes and non-GET methods, so the mux is built for extension.
//
// Authorization differs by listener, not by route: a unix-socket request is
// ambient (local, filesystem-guarded), while a TCP request must present a PAT.
// That split lives in RequirePAT (auth.go), which the daemon wraps around this
// mux for the TCP listener only.
package api

import (
	"encoding/json"
	"net/http"
)

// Envelope is the read-API success document of specification section 7:
// {"data": ...}. Every non-streaming success response is one Envelope on the
// wire.
type Envelope struct {
	// Data is the response payload, shaped per route.
	Data any `json:"data"`
}

// ErrorEnvelope is the read-API error document of specification section 7:
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
// both listeners so the CLI's daemon-reachability check and the conformance harness
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
	m := &mux{role: unknownRole{}, control: noControl{}}
	for _, o := range opts {
		o(m)
	}
	return m
}

// mux is the daemon's route table. It is a hand-rolled matcher (rather than
// http.ServeMux) so unknown routes and disallowed methods return the read-API
// error envelope, not net/http's plain-text 404/405. It consults role to gate
// mutations to the leader (specification section 15) and routes the control-plane
// mutations to the injected ControlHandler.
type mux struct {
	role    RoleReporter
	control ControlHandler
}

// ServeHTTP gates mutations to the leader, then dispatches a request to its route,
// or returns the closed-code error envelope for an unknown route or a disallowed
// method. A mutating request on any non-leader role is rejected with the not_leader
// envelope and leader guidance before it ever reaches a route: standbys reject
// mutations, reads work anywhere.
func (m *mux) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if !isSafeMethod(r.Method) && m.role.Role() != RoleLeader {
		WriteNotLeader(w, m.role.LeaderHint())
		return
	}
	switch r.URL.Path {
	case "/healthz":
		if r.Method != http.MethodGet {
			WriteError(w, http.StatusMethodNotAllowed, "method_not_allowed", "GET /healthz only")
			return
		}
		WriteData(w, http.StatusOK, Health{Status: "ok", Role: string(m.role.Role())})
	case "/apply":
		m.serveApply(w, r)
	case "/destroy":
		m.serveDestroy(w, r)
	default:
		WriteError(w, http.StatusNotFound, "not_found", "no such route: "+r.URL.Path)
	}
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
