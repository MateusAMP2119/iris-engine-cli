package daemon

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/MateusAMP2119/iris-lakehouse/internal/api"
	"github.com/MateusAMP2119/iris-lakehouse/internal/buildinfo"
	"github.com/MateusAMP2119/iris-lakehouse/internal/config"
	"github.com/MateusAMP2119/iris-lakehouse/internal/store"
)

// This file is the daemon's process-status plane: the api.PsHandler behind GET
// /ps (and therefore behind `iris ps` -- one route, one payload). It composes
// the run snapshot (one plain-MVCC read over the reader pool), the live
// leadership role, and the host-load probe: the engine load is the daemon's
// descendant tree plus the managed postmaster's (Postgres daemonizes into its
// own session, so parentage -- not the process group -- finds its backends),
// and each running run's load is its recorded process group. It is a read,
// served on any role, so a remote `iris ps` against a standby answers with the
// standby's own facts.
//
// Load is best-effort: a failed host probe logs at debug and the payload
// carries null load -- absence over fabrication. Uptime keeps the display-only
// wall-clock doctrine: rendered to a string here, so no computable time value
// ever reaches the wire.

// RunSnapshotReader is the run read the ps plane draws from: one plain-MVCC
// snapshot of the run records. store.Reader satisfies it.
type RunSnapshotReader interface {
	// Runs returns the runs matching filter, in ordering-identity order.
	Runs(ctx context.Context, filter store.RunFilter) ([]store.Run, error)
}

// psPlane is the api.PsHandler over the run reader, the role state, and the
// host-load probe. managedPG reports the managed postmaster's live pid (0 for
// none: external mode, or a stopped instance), so its tree is summed into the
// engine load.
type psPlane struct {
	role      api.RoleReporter
	runs      RunSnapshotReader
	probe     loadProber
	managedPG func() int
	counters  *turnCounters // resident turn tallies (#206); nil renders none
	logger    *slog.Logger
	pid       int
	started   time.Time
}

// compile-time proof the plane satisfies the mux's ps seam.
var _ api.PsHandler = (*psPlane)(nil)

// NewPsPlane builds the ps handler the daemon wires into the api mux: role is
// the daemon's live leadership role (nil reads unknown), runs the meta-backed
// (or fake) run read seam, managedPG the managed postmaster's pid locator (nil
// for none). The plane records its own pid at construction and counts uptime
// from it: the plane is built at daemon start, so its age is the daemon's. A
// nil logger discards output.
func NewPsPlane(role api.RoleReporter, runs RunSnapshotReader, managedPG func() int, counters *turnCounters, logger *slog.Logger) api.PsHandler {
	if logger == nil {
		logger = slog.New(slog.NewTextHandler(io.Discard, nil))
	}
	if managedPG == nil {
		managedPG = func() int { return 0 }
	}
	return &psPlane{
		role:      role,
		runs:      runs,
		probe:     psProbe{},
		managedPG: managedPG,
		counters:  counters,
		logger:    logger,
		pid:       os.Getpid(),
		started:   time.Now(),
	}
}

// ManagedPostmasterPID returns a locator for the managed Postgres postmaster's
// live pid: it reads <pg data dir>/postmaster.pid (its first line is the pid,
// per Postgres's own contract) on each call, so a restarted instance reports
// its current pid. It answers 0 -- no managed load to sum -- when the file is
// absent (external mode, stopped instance) or unreadable.
func ManagedPostmasterPID(s config.Settings) func() int {
	pidPath := filepath.Join(ManagedPGDir(s), "data", "postmaster.pid")
	return func() int {
		raw, err := os.ReadFile(pidPath) //nolint:gosec // G304: pidPath is the engine-owned managed-Postgres data dir's postmaster.pid, never user or network input.
		if err != nil {
			return 0
		}
		line, _, _ := strings.Cut(string(raw), "\n")
		pid, err := strconv.Atoi(strings.TrimSpace(line))
		if err != nil {
			return 0
		}
		return pid
	}
}

// Ps composes the current process-status payload: the engine block over the
// live role and the host-load sample, and the run rows newest first -- queued
// and running only, or the whole history under all.
func (p *psPlane) Ps(ctx context.Context, all bool) (api.PsPayload, error) {
	runs, err := p.runs.Runs(ctx, store.RunFilter{})
	if err != nil {
		p.logger.Error("ps run snapshot failed", "err", err)
		return api.PsPayload{}, fmt.Errorf("daemon: ps run snapshot: %w", err)
	}

	engine := api.PsEngine{
		Version: buildinfo.Version,
		Role:    string(api.RoleUnknown),
		PID:     p.pid,
		Uptime:  renderUptime(time.Since(p.started)),
	}
	if p.role != nil {
		role := p.role.Role()
		engine.Role = string(role)
		if role != api.RoleLeader {
			engine.Leader = p.role.LeaderHint()
		}
	}

	// The load sample is best-effort: a host without ps(1) logs at debug and the
	// payload carries null load, never a fabricated zero.
	groupLoad := map[int]*api.PsLoad{}
	if samples, perr := p.probe.Sample(ctx); perr != nil {
		p.logger.Debug("ps host-load probe failed", "err", perr)
	} else {
		for _, s := range samples {
			l := groupLoad[s.PGID]
			if l == nil {
				l = &api.PsLoad{}
				groupLoad[s.PGID] = l
			}
			l.CPUPercent += s.CPUPercent
			l.RSSBytes += s.RSSBytes
		}
		engine.Load = sumTrees(samples, p.pid, p.managedPG())
	}

	rows := make([]api.PsRun, 0, len(runs))
	// The reader returns ascending ordering identity; the readout is newest first.
	for i := len(runs) - 1; i >= 0; i-- {
		run := runs[i]
		switch run.State {
		case store.RunQueued:
			engine.QueuedRuns++
		case store.RunRunning:
			engine.RunningRuns++
		}
		if !all && run.State != store.RunQueued && run.State != store.RunRunning {
			continue
		}
		row := api.PsRun{
			ID:       run.ID,
			Pipeline: run.Pipeline,
			Lane:     run.Lane,
			State:    string(run.State),
		}
		// The reader coalesces a missing exit code to zero; only a terminal run
		// carries a real one on the wire.
		if run.ExitCode != nil && (run.State == store.RunSucceeded || run.State == store.RunDeadLettered) {
			code := *run.ExitCode
			row.ExitCode = &code
		}
		if run.State == store.RunRunning && run.Handle != 0 {
			row.Load = groupLoad[run.Handle]
		}
		rows = append(rows, row)
	}
	return api.PsPayload{Engine: engine, Runs: rows, Residents: p.counters.snapshot()}, nil
}

// sumTrees sums the host sample over the engine's process trees: every process
// whose parentage reaches one of the roots (the daemon, the managed
// postmaster), roots included. A zero root is skipped (no managed instance).
// It returns nil -- null on the wire -- when no sampled process matched, so a
// dead root never reads as a zero-load engine.
func sumTrees(samples []procSample, roots ...int) *api.PsLoad {
	inTree := map[int]bool{}
	for _, r := range roots {
		if r != 0 {
			inTree[r] = true
		}
	}
	// Children reparent below their root, never above it, so one pass per depth
	// level converges; the loop caps at the sample size for safety.
	for grew := true; grew; {
		grew = false
		for _, s := range samples {
			if !inTree[s.PID] && inTree[s.PPID] {
				inTree[s.PID] = true
				grew = true
			}
		}
	}
	var load *api.PsLoad
	for _, s := range samples {
		if !inTree[s.PID] {
			continue
		}
		if load == nil {
			load = &api.PsLoad{}
		}
		load.CPUPercent += s.CPUPercent
		load.RSSBytes += s.RSSBytes
	}
	return load
}

// renderUptime renders the daemon's age as the display-only uptime string (the
// one wall-clock readout, display only). Rendering happens here,
// second-truncated, so the wire never carries a computable duration or
// timestamp.
func renderUptime(d time.Duration) string {
	if d < 0 {
		d = 0
	}
	return d.Truncate(time.Second).String()
}
