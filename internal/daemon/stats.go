package daemon

import (
	"context"
	"fmt"
	"io"
	"log/slog"

	"github.com/MateusAMP2119/iris-lakehouse/internal/api"
	"github.com/MateusAMP2119/iris-lakehouse/internal/store"
)

// This file is the daemon's stats plane: the api.StatsHandler behind GET /stats
// (and therefore behind `iris engine stats` -- one route, one payload). It composes
// the store's stats rollup (one plain-MVCC snapshot of the meta reads) with the
// leader-held per-lane pass counter and maps the rollup onto the wire payload
// field-for-field. It is a read, served on any role: a standby answers with its own
// (zero) pass counts because it has dispatched no passes -- the counter is the
// leader's runtime state, never a meta row.

// PassCountReader reads the per-lane loop pass counts: the leader-held runtime
// counter (dispatch.PassCounter) satisfies it, and a nil reader reads all
// zeros (a daemon that never led has completed no passes).
type PassCountReader interface {
	// Counts returns a snapshot of loop passes completed per lane.
	Counts() map[string]int64
}

// statsPlane is the api.StatsHandler over the store stats source and the pass
// counter.
type statsPlane struct {
	src    store.StatsSource
	passes PassCountReader
	logger *slog.Logger
}

// compile-time proof the plane satisfies the mux's stats seam.
var _ api.StatsHandler = (*statsPlane)(nil)

// NewStatsPlane builds the stats handler the daemon wires into the api mux:
// src is the meta-backed (or fake) stats read seam, passes the leader-held
// pass counter (nil reads all zeros). A nil logger discards output.
func NewStatsPlane(src store.StatsSource, passes PassCountReader, logger *slog.Logger) api.StatsHandler {
	if logger == nil {
		logger = slog.New(slog.NewTextHandler(io.Discard, nil))
	}
	return &statsPlane{src: src, passes: passes, logger: logger}
}

// Stats composes the current rollup and maps it onto the wire payload.
func (p *statsPlane) Stats(ctx context.Context) (api.StatsPayload, error) {
	var passCounts map[string]int64
	if p.passes != nil {
		passCounts = p.passes.Counts()
	}
	rollup, err := store.BuildStats(ctx, p.src, passCounts)
	if err != nil {
		p.logger.Error("stats rollup failed", "err", err)
		return api.StatsPayload{}, fmt.Errorf("daemon: stats rollup: %w", err)
	}
	return statsPayload(rollup), nil
}

// statsPayload maps the store rollup onto the api payload field-for-field. The
// mapping is mechanical on purpose: the payload's shape is api's (the wire
// contract), the numbers are store's (the snapshot), and nothing is computed
// here -- so the CLI and HTTP surfaces cannot diverge from the rollup.
func statsPayload(r store.StatsRollup) api.StatsPayload {
	engine := api.EngineStats{
		DeadLetterDepth:     r.Engine.DeadLetterDepth,
		DeadLettersByReason: r.Engine.DeadLettersByReason,
		RunningRuns:         r.Engine.RunningRuns,
		CapturedWrites:      r.Engine.CapturedWrites,
		WipeEligibleRows:    r.Engine.WipeEligibleRows,
		JournalRows:         r.Engine.JournalRows,
		HotRows:             r.Engine.HotRows,
		SealedPartitions:    r.Engine.SealedPartitions,
		ArchivedPartitions:  r.Engine.ArchivedPartitions,
	}
	if head := r.Engine.ChainHead; head != nil {
		engine.CheckpointChainHead = &api.ChainHead{Seq: head.Seq, Digest: head.Digest, Location: head.Location}
	}

	lanes := make([]api.LaneStats, 0, len(r.Lanes))
	for _, l := range r.Lanes {
		lanes = append(lanes, api.LaneStats{
			Lane:      l.Lane,
			Pipelines: l.Pipelines,
			Queued:    l.Queued,
			Running:   l.Running,
			Passes:    l.Passes,
		})
	}

	pipelines := make([]api.PipelineStats, 0, len(r.Pipelines))
	for _, p := range r.Pipelines {
		pipelines = append(pipelines, api.PipelineStats{
			Pipeline:       p.Pipeline,
			LatestRunState: p.LatestRunState,
			RunsByState:    p.RunsByState,
			LastExitCode:   p.LastExitCode,
			LastRunID:      p.LastRunID,
		})
	}
	return api.StatsPayload{Engine: engine, Lanes: lanes, Pipelines: pipelines}
}
