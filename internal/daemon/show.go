package daemon

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"strconv"

	"github.com/MateusAMP2119/iris-lakehouse/internal/api"
	"github.com/MateusAMP2119/iris-lakehouse/internal/dispatch"
	"github.com/MateusAMP2119/iris-lakehouse/internal/pg"
	"github.com/MateusAMP2119/iris-lakehouse/internal/store"
)

// This file is the daemon's pipeline-show plane: the api.PipelineShowHandler behind
// GET /pipeline/show (and therefore behind `iris pipeline show` -- one route, one
// payload). It composes the store's plain-MVCC show reads (declaration detail, role
// grants, runs, dependency edges, upstream latest runs, the run_inputs consumed
// check) with dispatch's pure depends_on gate to produce the readout: the resolved
// declaration, the role and its field-level grants, the recent runs, and the gate
// ledger -- the per-edge verdict from the closed set (open, up_to_date, pending,
// poisoned). It is a read, served on any role from the reader pool, and mutates
// nothing.
//
// The ledger is resolved exactly as a run decision would resolve it -- the same
// dispatch.Gate over the same run_inputs consumed check -- with the zero
// awaited-from baseline the manual read surface uses (manualplane.go edgeReader):
// the readout explains what the gate would decide now, so "why no run" triage
// reads the same truth the dispatcher acts on.

// showPlane is the api.PipelineShowHandler over the store's show read seam and
// dispatch's depends_on gate.
type showPlane struct {
	reader store.ShowReader
	logger *slog.Logger
}

// compile-time proof the plane satisfies the mux's pipeline-show seam.
var _ api.PipelineShowHandler = (*showPlane)(nil)

// NewShowPlane builds the pipeline-show handler the daemon wires into the api
// mux: reader is the meta-backed (or fake) show read seam. A nil logger discards
// output.
func NewShowPlane(reader store.ShowReader, logger *slog.Logger) api.PipelineShowHandler {
	if logger == nil {
		logger = slog.New(slog.NewTextHandler(io.Discard, nil))
	}
	return &showPlane{reader: reader, logger: logger}
}

// ShowPipeline composes the single-pipeline readout for name. An unregistered
// pipeline is an error (the route maps it to operation_failed); any read error
// aborts, so the readout is never half-composed.
func (p *showPlane) ShowPipeline(ctx context.Context, name string) (api.PipelineShowResult, error) {
	detail, found, err := p.reader.PipelineDetail(ctx, name)
	if err != nil {
		p.logger.Error("pipeline show failed", "pipeline", name, "err", err)
		return api.PipelineShowResult{}, fmt.Errorf("daemon: pipeline show %q: %w", name, err)
	}
	if !found {
		return api.PipelineShowResult{}, fmt.Errorf("daemon: pipeline %q is not registered", name)
	}

	role := pg.PipelineRoleName(name)
	grants, err := p.reader.GrantsForRole(ctx, role)
	if err != nil {
		return api.PipelineShowResult{}, fmt.Errorf("daemon: pipeline show %q: read grants: %w", name, err)
	}
	runs, err := p.reader.Runs(ctx, store.RunFilter{Pipeline: name})
	if err != nil {
		return api.PipelineShowResult{}, fmt.Errorf("daemon: pipeline show %q: read runs: %w", name, err)
	}
	upstreams, edges, err := p.gateEdges(ctx, name)
	if err != nil {
		return api.PipelineShowResult{}, err
	}
	// The same pure gate a pass or manual run resolves, over the same run_inputs
	// consumed check (store.ShowReader satisfies dispatch.ConsumedReader).
	decision, err := dispatch.NewGate(p.reader).Evaluate(ctx, name, edges)
	if err != nil {
		return api.PipelineShowResult{}, fmt.Errorf("daemon: pipeline show %q: %w", name, err)
	}

	res := api.PipelineShowResult{
		Name:       name,
		Folder:     detail.Folder,
		Run:        detail.Run,
		Artifact:   string(detail.Artifact),
		DataMode:   string(detail.DataMode),
		DependsOn:  upstreams,
		Role:       role,
		Grants:     make([]api.GrantView, 0, len(grants)),
		RecentRuns: make([]api.RunView, 0, len(runs)),
		GateLedger: edgeVerdictViews(decision.Ledger),
	}
	for _, g := range grants {
		res.Grants = append(res.Grants, api.GrantView{Schema: g.Schema, Table: g.Table, Field: g.Field, Access: string(g.Access)})
	}
	for _, r := range runs {
		res.RecentRuns = append(res.RecentRuns, api.RunView{ID: r.ID, State: string(r.State), ExitCode: r.ExitCode})
	}
	return res, nil
}

// gateEdges resolves name's depends_on edges from meta for the ledger, delegating
// to the shared resolveGateEdges so the pipeline-show readout and the standalone
// gate route (pipelinegateplane.go) resolve identical edges.
func (p *showPlane) gateEdges(ctx context.Context, name string) ([]string, []dispatch.Edge, error) {
	return resolveGateEdges(ctx, p.reader, name)
}

// resolveGateEdges resolves a pipeline's depends_on edges from meta for the gate
// ledger: the dependency edges in declaration order, each joined to its upstream's
// most recent run, with the zero awaited-from baseline of the manual read surface.
// It returns the upstream names (the depends_on list) beside the gate edges. Both
// the pipeline-show plane and the standalone gate route compose their ledger from
// this, so "why no run" triage reads the same edges everywhere.
func resolveGateEdges(ctx context.Context, reader store.ShowReader, name string) ([]string, []dispatch.Edge, error) {
	all, err := reader.DependencyEdges(ctx)
	if err != nil {
		return nil, nil, fmt.Errorf("daemon: gate %q: read dependency edges: %w", name, err)
	}
	upstreams := []string{}
	var edges []dispatch.Edge
	for _, dep := range all {
		if dep.From != name {
			continue
		}
		info, found, err := reader.LatestRun(ctx, dep.To)
		if err != nil {
			return nil, nil, fmt.Errorf("daemon: gate %q: read upstream %q latest run: %w", name, dep.To, err)
		}
		upstreams = append(upstreams, dep.To)
		edges = append(edges, dispatch.Edge{
			Upstream:    dep.To,
			Latest:      upstreamState(found, info.State),
			LatestRunID: info.ID,
		})
	}
	return upstreams, edges, nil
}

// edgeVerdictViews maps a resolved gate ledger to the wire view rows: the upstream,
// the closed-set verdict token, and the upstream's latest run id (blank when the
// upstream has produced no run). It is the one mapping the pipeline-show readout and
// the standalone gate route share.
func edgeVerdictViews(ledger []dispatch.EdgeVerdict) []api.EdgeVerdictView {
	out := make([]api.EdgeVerdictView, 0, len(ledger))
	for _, row := range ledger {
		v := api.EdgeVerdictView{Upstream: row.Upstream, Verdict: row.Verdict.String()}
		if row.LatestRunID != 0 {
			v.LatestRunID = strconv.FormatInt(row.LatestRunID, 10)
		}
		out = append(out, v)
	}
	return out
}
