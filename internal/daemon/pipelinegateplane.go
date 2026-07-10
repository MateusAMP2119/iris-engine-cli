package daemon

import (
	"context"
	"fmt"
	"io"
	"log/slog"

	"github.com/MateusAMP2119/iris-engine-cli/internal/api"
	"github.com/MateusAMP2119/iris-engine-cli/internal/dispatch"
	"github.com/MateusAMP2119/iris-engine-cli/internal/store"
)

// This file is the daemon's standalone pipeline-gate plane: the
// api.PipelineGateHandler behind GET /pipelines/{name}/gate (specification sections
// 6.2 and 7). It is the gate ledger `iris pipeline show` prints, served as its own
// route for an external renderer or a script that reads only the verdict: per edge
// the upstream, the resolved verdict from the closed set (open, up_to_date, pending,
// poisoned), and the upstream's latest run id. It is a read, served on any role from
// the reader pool, and mutates nothing.
//
// It resolves the ledger exactly as the pipeline-show readout does -- the same
// resolveGateEdges over meta feeding the same pure dispatch.Gate over the same
// run_inputs consumed check -- so the standalone route and the show readout never
// disagree about what the gate would decide now.

// pipelineGatePlane is the api.PipelineGateHandler over the store's show read seam
// and dispatch's depends_on gate.
type pipelineGatePlane struct {
	reader store.ShowReader
	logger *slog.Logger
}

// compile-time proof the plane satisfies the mux's pipeline-gate seam.
var _ api.PipelineGateHandler = (*pipelineGatePlane)(nil)

// newPipelineGatePlane builds the gate handler the daemon wires into the api mux:
// reader is the meta-backed (or fake) show read seam. A nil logger discards.
func newPipelineGatePlane(reader store.ShowReader, logger *slog.Logger) *pipelineGatePlane {
	if logger == nil {
		logger = slog.New(slog.NewTextHandler(io.Discard, nil))
	}
	return &pipelineGatePlane{reader: reader, logger: logger}
}

// Gate resolves the named pipeline's depends_on gate ledger. An unregistered
// pipeline is a not-found-shaped error (never a fabricated empty ledger, which a
// registered pipeline with no edges legitimately has). A pipeline with no edges
// resolves to an empty (present) gate.
func (p *pipelineGatePlane) Gate(ctx context.Context, name string) (any, error) {
	_, found, err := p.reader.PipelineDetail(ctx, name)
	if err != nil {
		p.logger.Error("pipeline gate failed", "pipeline", name, "err", err)
		return nil, fmt.Errorf("daemon: pipeline gate %q: %w", name, err)
	}
	if !found {
		return nil, fmt.Errorf("daemon: pipeline %q is not registered", name)
	}

	_, edges, err := resolveGateEdges(ctx, p.reader, name)
	if err != nil {
		return nil, err
	}
	// The same pure gate a pass, a manual run, or the pipeline-show readout
	// resolves, over the same run_inputs consumed check (ShowReader satisfies
	// dispatch.ConsumedReader).
	decision, err := dispatch.NewGate(p.reader).Evaluate(ctx, name, edges)
	if err != nil {
		return nil, fmt.Errorf("daemon: pipeline gate %q: %w", name, err)
	}
	return api.PipelineGatePayload{Pipeline: name, Gate: edgeVerdictViews(decision.Ledger)}, nil
}
