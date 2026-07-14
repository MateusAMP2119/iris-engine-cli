package api

import (
	"context"
	"errors"
	"net/http"
)

// This file is the daemon's stats surface: GET /stats, the read-only rollup
// payload `iris engine stats` prints identically. One handler feeds one route,
// and the CLI reads that route, so the two surfaces cannot drift: parity is
// structural, not maintained.
//
// The payload's doctrine is clock-free purity: every value is a current count
// or a last-value. No time-series, no clock-derived metric, no timestamp -- the
// per-lane pass counter is a count of completed loop passes, never a duration,
// and the checkpoint chain head is a last-value (the newest sealed partition's
// digest). Core exposes no /metrics endpoint: a request to /metrics falls
// through the mux's roster to the not_found envelope (a monitor consumes GET
// /stats with a read PAT instead), which the stats tests pin so the refusal is
// a contract, not an accident.
//
// Like the control and pipeline surfaces, api stays a leaf: it defines the
// StatsHandler seam and the payload shape but reaches nothing up the stack. The
// daemon supplies the handler that composes the meta rollup reads and the
// leader-held pass counter; the mux only routes to it and renders the data
// envelope. Stats is a read, so it is served on any role -- reads work
// anywhere.

// StatsPayload is the one read-only rollup document GET /stats serves and
// `iris engine stats` prints: the engine-wide rollup plus the per-lane and
// per-pipeline rollups, all current counts and last-values.
type StatsPayload struct {
	// Engine is the engine-wide rollup.
	Engine EngineStats `json:"engine"`
	// Lanes are the per-lane rollups, in lane-name order.
	Lanes []LaneStats `json:"lanes"`
	// Pipelines are the per-pipeline rollups, in pipeline-name order.
	Pipelines []PipelineStats `json:"pipelines"`
}

// EngineStats is the engine-wide rollup: the dead-letter worklist, running
// runs, the capture counters, the wipe-eligible slice, total journal size, and
// the journal lifecycle readout (hot rows, sealed and archived partition
// counts, checkpoint chain head).
type EngineStats struct {
	// DeadLetterDepth is the outstanding dead-letter worklist depth.
	DeadLetterDepth int64 `json:"dead_letter_depth"`
	// DeadLettersByReason counts the outstanding worklist entries per reason
	// (the closed dead_letters.reason set). Always present, possibly empty.
	DeadLettersByReason map[string]int64 `json:"dead_letters_by_reason"`
	// RunningRuns is the number of runs currently in the running state.
	RunningRuns int64 `json:"running_runs"`
	// CapturedWrites is the capture counter: journal entries recorded by write
	// capture (a count of stamps, never bytes copied).
	CapturedWrites int64 `json:"captured_writes"`
	// WipeEligibleRows is the wipe-eligible slice: captured rows still
	// revertible by a workload wipe (un-promoted disposable writes).
	WipeEligibleRows int64 `json:"wipe_eligible_rows"`
	// JournalRows is the total journal size as a row count -- count-based like
	// the journal's own partition threshold, never a byte-per-second rate.
	JournalRows int64 `json:"journal_rows"`
	// HotRows is the lifecycle readout's hot slice: journal rows still resident
	// in unsealed (hot) partitions.
	HotRows int64 `json:"hot_rows"`
	// SealedPartitions counts the sealed journal partitions (one checkpoint row
	// each, resident or archived).
	SealedPartitions int64 `json:"sealed_partitions"`
	// ArchivedPartitions counts the sealed partitions exported to the object
	// store and dropped from Postgres.
	ArchivedPartitions int64 `json:"archived_partitions"`
	// CheckpointChainHead is the current head of the checkpoint chain (iris engine
	// stats reports the head). The field is always present and is explicitly null
	// while no partition has ever sealed (no checkpoint row exists yet).
	CheckpointChainHead *ChainHead `json:"checkpoint_chain_head"`
}

// ChainHead names the checkpoint chain's current head: the newest
// journal_checkpoints row (highest seq), a pure last-value.
type ChainHead struct {
	// Seq is the head checkpoint's insert-order identity.
	Seq int64 `json:"seq"`
	// Digest is the head checkpoint's chained digest, hex-encoded.
	Digest string `json:"digest"`
	// Location is where the head's sealed partition lives: resident or archived.
	Location string `json:"location"`
}

// LaneStats is one lane's rollup: pipeline count, queued/running count, and
// loop passes completed since daemon start.
type LaneStats struct {
	// Lane is the lane's name.
	Lane string `json:"lane"`
	// Pipelines is the number of registered pipelines composed into the lane.
	Pipelines int64 `json:"pipelines"`
	// Queued is the number of queued runs across the lane's pipelines.
	Queued int64 `json:"queued"`
	// Running is the number of running runs across the lane's pipelines.
	Running int64 `json:"running"`
	// Passes is the number of loop passes the lane completed since daemon
	// start: the leader-held runtime counter, reset on restart and on leader
	// change. It is clock-free -- a count, never a duration.
	Passes int64 `json:"passes"`
}

// PipelineStats is one pipeline's rollup: latest run state, run counts by
// state, last exit code, last run id -- last-values from the run history's
// ordering identity, never a clock.
type PipelineStats struct {
	// Pipeline is the registered pipeline's name.
	Pipeline string `json:"pipeline"`
	// LatestRunState is the most recent run's state, or "" with no runs.
	LatestRunState string `json:"latest_run_state"`
	// RunsByState counts the pipeline's runs per state. Always present,
	// possibly empty.
	RunsByState map[string]int64 `json:"runs_by_state"`
	// LastExitCode is the exit code of the most recent run carrying one, or
	// null while no run has exited.
	LastExitCode *int `json:"last_exit_code"`
	// LastRunID is the most recent run's id, or "" with no runs.
	LastRunID string `json:"last_run_id"`
}

// StatsHandler serves the stats rollup. The daemon implements it over the meta
// rollup reads and the leader-held pass counter; the mux depends only on this
// interface, so api never imports store/dispatch.
type StatsHandler interface {
	// Stats returns the current rollup payload.
	Stats(ctx context.Context) (StatsPayload, error)
}

// ErrStatsUnavailable is returned by the default (unwired) stats handler: a
// stats read reached the mux but no handler is installed. The daemon wires the
// handler at construction, so it is an internal fault, never an empty payload.
var ErrStatsUnavailable = errors.New("api: stats not available")

// WithStats wires the stats handler the mux routes GET /stats to. A nil
// handler is ignored, keeping the safe default (the route faults with an
// internal error until a real handler is installed).
func WithStats(h StatsHandler) MuxOption {
	return func(m *mux) {
		if h != nil {
			m.stats = h
		}
	}
}

// noStats is the default StatsHandler before one is wired: every read is an
// internal fault, never a silent empty rollup.
type noStats struct{}

func (noStats) Stats(context.Context) (StatsPayload, error) {
	return StatsPayload{}, ErrStatsUnavailable
}

// serveStats handles GET /stats: run the wired rollup handler and render the
// data envelope. It is a read, served on any role. An unwired handler is 500
// internal; any rollup read error is 500 internal too -- a stats read has no
// operation-failure category of its own.
func (m *mux) serveStats(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		WriteError(w, http.StatusMethodNotAllowed, "method_not_allowed", "GET "+r.URL.Path+" only")
		return
	}
	if !noParams(w, r) {
		return
	}
	payload, err := m.stats.Stats(r.Context())
	if err != nil {
		WriteError(w, http.StatusInternalServerError, "internal", err.Error())
		return
	}
	WriteData(w, http.StatusOK, payload)
}
