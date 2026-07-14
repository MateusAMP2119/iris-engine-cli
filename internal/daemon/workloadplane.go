package daemon

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"strconv"

	"github.com/MateusAMP2119/iris-lakehouse/internal/api"
	"github.com/MateusAMP2119/iris-lakehouse/internal/dispatch"
	"github.com/MateusAMP2119/iris-lakehouse/internal/store"
)

// This file is the daemon's workload wiring panel plane: the
// api.WorkloadShowHandler behind GET /workload (and `iris workload show`).
// It renders the standing wiring as a panel using dispatch.BuildWalk for the
// composer lanes and dispatch.Gate (over the same ConsumedReader) for per-edge
// live gate state. Artifact/data modes and run tips come from the show reader.
// A named pipeline zooms the panel to its neighborhood. No new meta state.

// workloadPlane implements the wiring panel over the show reads and dispatch
// walks/gates.
type workloadPlane struct {
	reader store.ShowReader
	logger *slog.Logger
}

var _ api.WorkloadShowHandler = (*workloadPlane)(nil)

// NewWorkloadPlane builds the handler for wiring panel.
func NewWorkloadPlane(reader store.ShowReader, logger *slog.Logger) api.WorkloadShowHandler {
	if logger == nil {
		logger = slog.New(slog.NewTextHandler(io.Discard, nil))
	}
	return &workloadPlane{reader: reader, logger: logger}
}

// ShowWorkload builds the panel reusing BuildWalk and Gate. Zoom filters to
// the named pipeline's lane neighborhood (containing lane).
func (p *workloadPlane) ShowWorkload(ctx context.Context, pipeline string) (api.WorkloadShowResult, error) {
	if p.reader == nil {
		return api.WorkloadShowResult{}, fmt.Errorf("daemon: workload show: no reader")
	}

	// Registered for BuildWalk and detail lookup.
	names, err := p.reader.RegisteredPipelines(ctx)
	if err != nil {
		return api.WorkloadShowResult{}, fmt.Errorf("daemon: workload show: read registered: %w", err)
	}
	reg := map[string]bool{}
	for _, n := range names {
		reg[n] = true
	}

	// Lane rows for composer walk.
	laneRows, err := p.reader.LaneRows(ctx)
	if err != nil {
		return api.WorkloadShowResult{}, fmt.Errorf("daemon: workload show: read lanes: %w", err)
	}
	// Convert store.LaneEntry -> dispatch.LaneRow (identical shape).
	dRows := make([]dispatch.LaneRow, 0, len(laneRows))
	for _, le := range laneRows {
		dRows = append(dRows, dispatch.LaneRow{Lane: le.Lane, Pipeline: le.Pipeline, Pos: le.Pos})
	}
	lanes := dispatch.BuildWalk(dRows, reg)

	// All dependency edges (global).
	allEdges, err := p.reader.DependencyEdges(ctx)
	if err != nil {
		return api.WorkloadShowResult{}, fmt.Errorf("daemon: workload show: read edges: %w", err)
	}

	// Build per-pipeline wiring info.
	byName := map[string]api.PipelineWiring{}
	gate := dispatch.NewGate(p.reader)
	for _, ln := range lanes {
		for _, pname := range ln.Pipelines {
			if _, seen := byName[pname]; seen {
				continue
			}
			detail, found, err := p.reader.PipelineDetail(ctx, pname)
			if err != nil {
				return api.WorkloadShowResult{}, fmt.Errorf("daemon: workload show %q: detail: %w", pname, err)
			}
			if !found {
				continue // should not happen
			}

			// Collect this pipeline's depends_on edges in declaration order.
			var dEdges []dispatch.Edge
			for _, dep := range allEdges {
				if dep.From != pname {
					continue
				}
				info, has, err := p.reader.LatestRun(ctx, dep.To)
				if err != nil {
					return api.WorkloadShowResult{}, fmt.Errorf("daemon: workload show %q: latest %q: %w", pname, dep.To, err)
				}
				dEdges = append(dEdges, dispatch.Edge{
					Upstream:    dep.To,
					Latest:      upstreamState(has, info.State),
					LatestRunID: info.ID,
				})
			}

			// Resolve live gate ledger for this pipeline (same as dispatcher/pipeline show).
			dec, err := gate.Evaluate(ctx, pname, dEdges)
			if err != nil {
				return api.WorkloadShowResult{}, fmt.Errorf("daemon: workload show %q: gate: %w", pname, err)
			}

			// Run tip from latest.
			lt, hasLt, err := p.reader.LatestRun(ctx, pname)
			if err != nil {
				return api.WorkloadShowResult{}, fmt.Errorf("daemon: workload show %q: tip: %w", pname, err)
			}
			tip := ""
			if hasLt {
				tip = fmt.Sprintf("%s %d", lt.State, lt.ID)
			} else {
				tip = "none"
			}

			led := make([]api.EdgeVerdictView, 0, len(dec.Ledger))
			for _, ev := range dec.Ledger {
				v := api.EdgeVerdictView{Upstream: ev.Upstream, Verdict: ev.Verdict.String()}
				if ev.LatestRunID != 0 {
					v.LatestRunID = strconv.FormatInt(ev.LatestRunID, 10)
				}
				led = append(led, v)
			}

			byName[pname] = api.PipelineWiring{
				Name:     pname,
				Folder:   detail.Folder,
				Artifact: string(detail.Artifact),
				DataMode: string(detail.DataMode),
				RunTip:   tip,
				Gate:     led,
			}
		}
	}

	// Assemble lanes, filtering for zoom if requested.
	var outLanes []api.LaneWiring
	for _, ln := range lanes {
		keep := true
		if pipeline != "" {
			keep = false
			for _, pn := range ln.Pipelines {
				if pn == pipeline {
					keep = true
					break
				}
			}
		}
		if !keep {
			continue
		}
		pws := make([]api.PipelineWiring, 0, len(ln.Pipelines))
		for _, pn := range ln.Pipelines {
			if pw, ok := byName[pn]; ok {
				pws = append(pws, pw)
			}
		}
		if len(pws) > 0 {
			outLanes = append(outLanes, api.LaneWiring{Name: ln.Name, Pipelines: pws})
		}
	}

	// If zoom to a name that exists but filtered all (e.g. solo anon), still include if registered.
	if pipeline != "" && len(outLanes) == 0 {
		if reg[pipeline] {
			detail, _, _ := p.reader.PipelineDetail(ctx, pipeline)
			// minimal single lane neighborhood
			// (for completeness; our seed always has lane)
			pw := byName[pipeline]
			if pw.Name == "" {
				// build minimal if not walked
				lt, hasLt, _ := p.reader.LatestRun(ctx, pipeline)
				tip := "none"
				if hasLt {
					tip = fmt.Sprintf("%s %d", lt.State, lt.ID)
				}
				pw = api.PipelineWiring{Name: pipeline, Folder: detail.Folder, Artifact: string(detail.Artifact), DataMode: string(detail.DataMode), RunTip: tip}
			}
			outLanes = append(outLanes, api.LaneWiring{Name: pipeline, Pipelines: []api.PipelineWiring{pw}})
		} else {
			return api.WorkloadShowResult{}, fmt.Errorf("daemon: pipeline %q is not registered", pipeline)
		}
	}

	return api.WorkloadShowResult{Lanes: outLanes}, nil
}
