package daemon

import (
	"context"
	"sort"
	"time"

	"github.com/MateusAMP2119/iris-engine-cli/internal/api"
	"github.com/MateusAMP2119/iris-engine-cli/internal/pg"
	"github.com/MateusAMP2119/iris-engine-cli/internal/store"
)

// This file is the daemon's engine-info plane: the api.InfoHandler behind GET /info
// (and therefore behind `iris engine info` -- one route, one payload). It composes
// the daemon-held runtime facts -- the leadership role (naming the leader when
// known), the resolved listeners, the data and meta targets, the leader-held
// per-lane pass counts, and uptime -- while the CLI merges in the local
// configuration (engine and Go version, Postgres mode, objects path) and the engine
// key's public half. It is a read, served on any role: a standby answers with its
// own role, the leader it knows, and its own (zero) pass counts.
//
// Uptime is the engine's sole wall-clock readout and it is display-only: the plane
// renders it to a string here, so no computable time value ever reaches the wire.
// No last-heartbeat or last-seen exists anywhere; connection state is the only
// liveness signal.

// InfoConfig is the resolved listener and target facts the info plane reports:
// the unix socket, the TCP listener when one is configured, and the data/meta
// database targets (names only, never a DSN or credential). Empty targets default
// to the engine's fixed database names.
type InfoConfig struct {
	// Socket is the unix control socket the daemon serves on.
	Socket string
	// TCP is the TCP listener address, empty when none is configured.
	TCP string
	// DataTarget names the data database target; empty defaults to pg.DataDatabase.
	DataTarget string
	// MetaTarget names the meta database target; empty defaults to store.MetaDatabase.
	MetaTarget string
}

// infoPlane is the api.InfoHandler over the daemon's role state, the leader-held
// pass counter, and the resolved listener/target configuration.
type infoPlane struct {
	role    api.RoleReporter
	passes  PassCountReader
	cfg     InfoConfig
	started time.Time
}

// compile-time proof the plane satisfies the mux's info seam.
var _ api.InfoHandler = (*infoPlane)(nil)

// NewInfoPlane builds the info handler the daemon wires into the api mux: role is
// the daemon's live leadership role (nil reads unknown), passes the leader-held
// per-lane pass counter (nil reads no lanes -- a daemon that never led has
// completed no passes), and cfg the resolved listeners and targets. Uptime counts
// from construction: the plane is built at daemon start, so its age is the
// daemon's.
func NewInfoPlane(role api.RoleReporter, passes PassCountReader, cfg InfoConfig) api.InfoHandler {
	if cfg.DataTarget == "" {
		cfg.DataTarget = pg.DataDatabase
	}
	if cfg.MetaTarget == "" {
		cfg.MetaTarget = store.MetaDatabase
	}
	return &infoPlane{role: role, passes: passes, cfg: cfg, started: time.Now()}
}

// Info composes the current runtime readout. It cannot fail: every fact is held
// in-process (no meta read), so the readout is served on any role at any time.
func (p *infoPlane) Info(context.Context) (api.InfoPayload, error) {
	role, leader := api.RoleUnknown, ""
	if p.role != nil {
		role = p.role.Role()
		if role != api.RoleLeader {
			leader = p.role.LeaderHint()
		}
	}
	return api.InfoPayload{
		Role:       string(role),
		Leader:     leader,
		Socket:     p.cfg.Socket,
		TCP:        p.cfg.TCP,
		DataTarget: p.cfg.DataTarget,
		MetaTarget: p.cfg.MetaTarget,
		LanePasses: lanePasses(p.passes),
		Uptime:     renderUptime(time.Since(p.started)),
	}, nil
}

// lanePasses snapshots the per-lane pass counts in lane-name order, so the
// readout is stable. A nil reader yields the empty (never nil) list.
func lanePasses(passes PassCountReader) []api.LanePasses {
	out := []api.LanePasses{}
	if passes == nil {
		return out
	}
	counts := passes.Counts()
	lanes := make([]string, 0, len(counts))
	for lane := range counts {
		lanes = append(lanes, lane)
	}
	sort.Strings(lanes)
	for _, lane := range lanes {
		out = append(out, api.LanePasses{Lane: lane, Passes: counts[lane]})
	}
	return out
}

// renderUptime renders the daemon's age as the display-only uptime string (the one
// wall-clock readout, display only). Rendering happens here, second-truncated, so
// the wire never carries a computable duration or timestamp.
func renderUptime(d time.Duration) string {
	if d < 0 {
		d = 0
	}
	return d.Truncate(time.Second).String()
}
