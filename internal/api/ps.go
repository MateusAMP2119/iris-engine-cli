package api

import (
	"context"
	"errors"
	"net/http"
)

// This file is the daemon's process-status surface: GET /ps, the one payload
// `iris ps` prints. It is the docker-ps-shaped readout of the engine: what the
// engine is (version, role, pid), what it is doing (the run rows, queued and
// running by default, the whole history under ?all=true), and what it costs the
// host (the sampled CPU and resident-memory load of the engine's own process
// group and of each running run's process group). It is a read, served on any
// role, so a remote client with a read PAT sees the same readout the local
// socket does.
//
// Load is a best-effort host sample: when the daemon cannot probe its host
// (an exotic platform, a stripped container), the load fields are null rather
// than fabricated zeros -- absence is honest, zero is a claim. Uptime keeps the
// engine's display-only wall-clock doctrine: a rendered string, never a
// duration or timestamp a caller could compute on.

// PsPayload is the GET /ps document: the engine block and the run rows,
// newest first.
type PsPayload struct {
	// Engine is the engine block: identity, role, and host load.
	Engine PsEngine `json:"engine"`
	// Runs are the run rows, newest first. Queued and running only by default;
	// the whole history under ?all=true. Always present, possibly empty.
	Runs []PsRun `json:"runs"`
	// Residents are the leader's live resident workers' turn counters (#206):
	// quiet-loop visibility held in memory, no rows. Present on the leader only.
	Residents []PsResident `json:"residents,omitempty"`
	// SampleTick is the daemon load collector's monotonic sample counter: it
	// advances once per collector sample, so a poller can tell a fresh load
	// reading from a repeat of the last one. Zero before the first sample (or
	// on a daemon without a collector).
	SampleTick uint64 `json:"sample_tick,omitempty"`
	// History is the collector's recorded load history, present only under
	// ?history=1. It is daemon memory, not persistence: it survives any number
	// of client restarts and dies with the daemon.
	History *PsHistory `json:"history,omitempty"`
}

// PsHistory is the daemon-held load history under ?history=1: one series per
// sampled entity, on two grids. The fine grid holds one sample per collector
// tick; the coarse grid holds one per-bucket maximum per aggregation bucket,
// retaining hours where the fine grid retains minutes. The intervals are the
// collector's fixed cadence configuration -- grid spacing, not wall-clock
// readings -- so the display-only wall-clock doctrine holds.
type PsHistory struct {
	// FineIntervalSeconds is the fine grid's spacing: seconds per sample.
	FineIntervalSeconds int `json:"fine_interval_seconds"`
	// CoarseIntervalSeconds is the coarse grid's spacing: seconds per bucket.
	CoarseIntervalSeconds int `json:"coarse_interval_seconds"`
	// Series are the recorded entities' histories. Keys: "engine" for the
	// engine's own load, "lane:<name>" and "pipeline:<name>" for the running
	// runs summed under a lane or pipeline.
	Series []PsSeries `json:"series"`
}

// PsHistoryNoSample marks a grid slot with no load sample (an idle entity, a
// failed host probe) in a history series' CPU array. Absence over fabrication:
// a client renders it as a gap, never a zero.
const PsHistoryNoSample = -1

// PsSeries is one entity's recorded load history, oldest first, newest last.
// Every live series ends at the same tick (the collector pushes all series in
// lockstep), so series of different lengths align from their ends. A slot with
// no sample carries PsHistoryNoSample CPU and zero RSS.
type PsSeries struct {
	// Key names the entity: "engine", "lane:<name>", or "pipeline:<name>".
	Key string `json:"key"`
	// CPU is the fine grid's CPU history in percent of one core
	// (PsHistoryNoSample for a sampleless tick).
	CPU []float64 `json:"cpu"`
	// RSS is the fine grid's resident-memory history in bytes.
	RSS []int64 `json:"rss"`
	// CoarseCPU is the coarse grid's CPU history: each slot the maximum fine
	// sample of its bucket, so a short spike stays visible at this zoom.
	CoarseCPU []float64 `json:"coarse_cpu"`
	// CoarseRSS is the coarse grid's resident-memory history: per-bucket maxima.
	CoarseRSS []int64 `json:"coarse_rss"`
}

// PsResident is one pipeline's turn readout under the turn protocol (#206): how
// many turns its worker has been given this leadership term, and how many have
// elapsed since one last recorded a run. A high turns_since_run over a live
// worker is a quiet loop, not a stuck one -- turns that record nothing write
// nothing, so the counters are the only trace they leave.
type PsResident struct {
	// Pipeline is the resident worker's pipeline.
	Pipeline string `json:"pipeline"`
	// Turns is the pipeline's total driven turns this leadership term.
	Turns uint64 `json:"turns"`
	// TurnsSinceRun counts turns since the last one that recorded a run row.
	TurnsSinceRun uint64 `json:"turns_since_run"`
}

// PsLoad is one sampled host-load reading: CPU percentage and resident memory.
type PsLoad struct {
	// CPUPercent is the sampled CPU usage in percent of one core.
	CPUPercent float64 `json:"cpu_percent"`
	// RSSBytes is the sampled resident set size in bytes.
	RSSBytes int64 `json:"rss_bytes"`
}

// PsEngine is the engine block of the ps readout: identity, leadership role,
// run counts, and the engine process group's sampled host load.
type PsEngine struct {
	// Version is the daemon's build version (linker-stamped, "dev" unstamped).
	Version string `json:"version"`
	// Role is the daemon's leadership role: leader, standby, or unknown.
	Role string `json:"role"`
	// Leader is the current leader's address when this daemon is a standby that
	// knows it; empty on the leader or when no leader is known.
	Leader string `json:"leader,omitempty"`
	// PID is the daemon's process id on its host.
	PID int `json:"pid"`
	// Uptime is the display-only wall-clock readout, a rendered string, never a
	// duration or timestamp a caller could compute on.
	Uptime string `json:"uptime"`
	// QueuedRuns and RunningRuns count the engine's queued and running runs,
	// whatever run rows the request's ?all filter returned.
	QueuedRuns  int64 `json:"queued_runs"`
	RunningRuns int64 `json:"running_runs"`
	// Load is the engine process group's sampled host load (the daemon and its
	// managed Postgres), or null when the host could not be probed.
	Load *PsLoad `json:"load"`
}

// PsRun is one run row of the ps readout.
type PsRun struct {
	// ID is the run's meta id.
	ID string `json:"id"`
	// Pipeline is the run's pipeline.
	Pipeline string `json:"pipeline"`
	// Lane is the lane the pipeline belongs to; empty when none is recorded.
	Lane string `json:"lane,omitempty"`
	// State is the run's lifecycle state.
	State string `json:"state"`
	// ExitCode is the subprocess exit code, present on terminal runs only.
	ExitCode *int `json:"exit_code,omitempty"`
	// Load is the run's process group's sampled host load, present only on a
	// running run whose process group answered the probe.
	Load *PsLoad `json:"load,omitempty"`
}

// PsHandler serves the process-status readout. The daemon implements it over
// the run reader, the leadership role, and its load collector; the mux depends
// only on this interface, so api never imports store/dispatch.
type PsHandler interface {
	// Ps returns the current process-status payload. all widens the run rows
	// from queued+running to the whole history; history attaches the recorded
	// load history to the payload.
	Ps(ctx context.Context, all, history bool) (PsPayload, error)
}

// ErrPsUnavailable is returned by the default (unwired) ps handler: a ps read
// reached the mux but no handler is installed. The daemon wires the handler at
// construction, so it is an internal fault, never an empty payload.
var ErrPsUnavailable = errors.New("api: ps not available")

// WithPs wires the ps handler the mux routes GET /ps to. A nil handler is
// ignored, keeping the safe default (the route faults with an internal error
// until a real handler is installed).
func WithPs(h PsHandler) MuxOption {
	return func(m *mux) {
		if h != nil {
			m.ps = h
		}
	}
}

// noPs is the default PsHandler before one is wired: every read is an internal
// fault, never a silent empty payload.
type noPs struct{}

func (noPs) Ps(context.Context, bool, bool) (PsPayload, error) {
	return PsPayload{}, ErrPsUnavailable
}

// servePs handles GET /ps[?all=true][&history=1]: run the wired ps handler and
// render the data envelope. It is a read, served on any role. An unwired
// handler is 500 internal; any read error is 500 internal too -- a ps read has
// no operation-failure category of its own.
func (m *mux) servePs(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		WriteError(w, http.StatusMethodNotAllowed, "method_not_allowed", "GET "+r.URL.Path+" only")
		return
	}
	q := r.URL.Query()
	for k := range q {
		if k != "all" && k != "history" {
			WriteError(w, http.StatusBadRequest, "bad_param", "unknown query parameter: "+k)
			return
		}
	}
	all := q.Get("all") == "true" || q.Get("all") == "1"
	history := q.Get("history") == "true" || q.Get("history") == "1"
	payload, err := m.ps.Ps(r.Context(), all, history)
	if err != nil {
		WriteError(w, http.StatusInternalServerError, "internal", err.Error())
		return
	}
	WriteData(w, http.StatusOK, payload)
}
