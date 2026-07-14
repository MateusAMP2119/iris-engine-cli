package api

import (
	"context"
	"errors"
	"net/http"
)

// This file is the daemon's engine-inspect surface: GET /inspect, the read-only
// dump of the engine-table DDL `iris engine inspect` prints. The DDL is
// embedded in the binary (create-if-missing at bootstrap), so the dump renders
// the engine's own schema model and reads no rows and writes nothing -- a
// mutation-free readout. It is a GET, served on any role (reads work anywhere),
// and non-GET methods are refused, so the route can never mutate engine state.
//
// Like the other read surfaces, api stays a leaf: it defines the InspectHandler
// seam and the payload shape but reaches nothing up the stack; the daemon supplies
// the handler that renders the embedded meta and journal DDL.

// InspectPayload is the GET /inspect document: the engine-table DDL as an ordered
// list of create-if-missing statements (the meta control tables followed by the
// data journal). It is a pure dump of the embedded schema model -- no row data, no
// engine state -- so `iris engine inspect` mutates nothing.
type InspectPayload struct {
	// DDL is the engine-table DDL, one statement per element, in emission order.
	DDL []string `json:"ddl"`
}

// InspectHandler serves the engine-table DDL dump. The daemon implements it by
// rendering the embedded schema model; the mux depends only on this interface, so
// api never imports store/pg.
type InspectHandler interface {
	// Inspect returns the engine-table DDL dump. It is read-only: it renders the
	// embedded schema and mutates no engine state.
	Inspect(ctx context.Context) (InspectPayload, error)
}

// ErrInspectUnavailable is returned by the default (unwired) inspect handler: an
// inspect read reached the mux but no handler is installed. The daemon wires the
// handler at construction, so it is an internal fault, never an empty dump.
var ErrInspectUnavailable = errors.New("api: inspect not available")

// WithInspect wires the inspect handler the mux routes GET /inspect to. A nil
// handler is ignored, keeping the safe default (the route faults with an internal
// error until a real handler is installed).
func WithInspect(h InspectHandler) MuxOption {
	return func(m *mux) {
		if h != nil {
			m.inspect = h
		}
	}
}

// noInspect is the default InspectHandler before one is wired: every read is an
// internal fault, never a silent empty dump.
type noInspect struct{}

func (noInspect) Inspect(context.Context) (InspectPayload, error) {
	return InspectPayload{}, ErrInspectUnavailable
}

// serveInspect handles GET /inspect: run the wired inspect handler and render
// the data envelope. It is a read, served on any role, and only ever a GET, so
// it cannot mutate engine state. An unwired handler is 500 internal.
func (m *mux) serveInspect(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		WriteError(w, http.StatusMethodNotAllowed, "method_not_allowed", "GET "+r.URL.Path+" only")
		return
	}
	payload, err := m.inspect.Inspect(r.Context())
	if err != nil {
		WriteError(w, http.StatusInternalServerError, "internal", err.Error())
		return
	}
	WriteData(w, http.StatusOK, payload)
}
