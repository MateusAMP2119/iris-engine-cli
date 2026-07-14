package api

import (
	"context"
	"errors"
	"net/http"
)

// This file is the daemon's engine-info surface: GET /info, the daemon-held
// runtime readout `iris engine info` merges with the CLI's local configuration.
// It is a read, served on any role -- reads work anywhere -- so a standby
// answers with its own role and its own (zero) pass counts. Like the stats and
// pipeline surfaces, api stays a leaf: it defines the InfoHandler seam and the
// payload shape but reaches nothing up the stack; the daemon supplies the
// handler that composes the leadership role, the leader-held pass counter, the
// resolved listeners and targets, and the display-only uptime.
//
// The payload's one wall-clock is Uptime, and it is a rendered display STRING,
// not a duration or timestamp a caller could compute on: it is the engine's
// sole, display-only wall-clock readout. Connection state is the only liveness
// signal, so the payload carries no last-heartbeat or last-seen field --
// absence of any such field is the contract.

// InfoPayload is the GET /info document: the daemon-held runtime readout of
// `iris engine info` -- the leadership role (and the leader's address when known),
// the resolved socket and TCP listeners, the data and meta targets, the per-lane
// loop pass counts, and the display-only uptime. The CLI merges it with the local
// configuration (engine and Go version, Postgres mode, objects path) and the
// engine key's public half. It carries no engine key material or credential.
type InfoPayload struct {
	// Role is the daemon's leadership role: leader, standby, or unknown.
	Role string `json:"role"`
	// Leader is the current leader's address when this daemon is a standby that
	// knows it; empty on the leader or when no leader is known.
	Leader string `json:"leader,omitempty"`
	// Socket is the unix control socket the daemon serves on.
	Socket string `json:"socket"`
	// TCP is the TCP listener address when one is configured; empty otherwise.
	TCP string `json:"tcp,omitempty"`
	// DataTarget names the data database target (never the DSN or any credential).
	DataTarget string `json:"data_target"`
	// MetaTarget names the meta database target (never the DSN or any credential).
	MetaTarget string `json:"meta_target"`
	// LanePasses are the per-lane loop pass counts since daemon start, in lane-name
	// order: the leader-held runtime counter (a count, never a duration). Always
	// present, possibly empty.
	LanePasses []LanePasses `json:"lane_passes"`
	// Uptime is the engine's sole wall-clock readout, a rendered display string,
	// display-only. It is a string, never a duration or timestamp, so it can never
	// be mistaken for a value work is gated or ordered on.
	Uptime string `json:"uptime"`
}

// LanePasses is one lane's completed loop-pass count for the info readout: a
// count of passes since daemon start, never a duration.
type LanePasses struct {
	// Lane is the lane's name.
	Lane string `json:"lane"`
	// Passes is the lane's loop passes completed since daemon start.
	Passes int64 `json:"passes"`
}

// InfoHandler serves the engine-info runtime readout. The daemon implements it
// over the leadership role, the leader-held pass counter, and the resolved
// listeners; the mux depends only on this interface, so api never imports
// store/dispatch.
type InfoHandler interface {
	// Info returns the current runtime info payload.
	Info(ctx context.Context) (InfoPayload, error)
}

// ErrInfoUnavailable is returned by the default (unwired) info handler: an info
// read reached the mux but no handler is installed. The daemon wires the handler
// at construction, so it is an internal fault, never an empty payload.
var ErrInfoUnavailable = errors.New("api: info not available")

// WithInfo wires the info handler the mux routes GET /info to. A nil handler is
// ignored, keeping the safe default (the route faults with an internal error until
// a real handler is installed).
func WithInfo(h InfoHandler) MuxOption {
	return func(m *mux) {
		if h != nil {
			m.info = h
		}
	}
}

// noInfo is the default InfoHandler before one is wired: every read is an internal
// fault, never a silent empty payload.
type noInfo struct{}

func (noInfo) Info(context.Context) (InfoPayload, error) {
	return InfoPayload{}, ErrInfoUnavailable
}

// serveInfo handles GET /info: run the wired info handler and render the data
// envelope. It is a read, served on any role. An unwired handler is 500
// internal; any read error is 500 internal too -- an info read has no
// operation-failure category of its own.
func (m *mux) serveInfo(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		WriteError(w, http.StatusMethodNotAllowed, "method_not_allowed", "GET "+r.URL.Path+" only")
		return
	}
	payload, err := m.info.Info(r.Context())
	if err != nil {
		WriteError(w, http.StatusInternalServerError, "internal", err.Error())
		return
	}
	WriteData(w, http.StatusOK, payload)
}
