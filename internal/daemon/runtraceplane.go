package daemon

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"strconv"

	"github.com/MateusAMP2119/iris-engine-cli/internal/api"
	"github.com/MateusAMP2119/iris-engine-cli/internal/pg"
	"github.com/MateusAMP2119/iris-engine-cli/internal/store"
)

// This file is the daemon's run-trace plane: the api.RunTraceHandler behind GET
// /runs/{id}/trace?direction=up|down (and therefore `iris run show <run> --trace
// [--down]` -- specification sections 7 and 14). It loads the meta lineage
// (runs + summaries + run_inputs) from the reader pool, maps it to the pure
// pg.Lineage, and walks it: up climbs the run's ancestry over run_inputs (one edge
// per consumed upstream), down inverts it (who consumed this run) over the
// upstream_run_id index. It is a read, served on any role, and mutates nothing.
//
// The walk is the full recursive ancestry (pg.FullAncestry) -- the same WITH
// RECURSIVE the CLI surfaces -- resolving a pruned run's parents from its archival
// summary's consumed-upstream list, so lineage never dangles.

// runTracePlane is the api.RunTraceHandler over the store's meta lineage read seam
// and the pure pg ancestry walk.
type runTracePlane struct {
	reader store.Reader
	logger *slog.Logger
}

// compile-time proof the plane satisfies the mux's run-trace seam.
var _ api.RunTraceHandler = (*runTracePlane)(nil)

// newRunTracePlane builds the run-trace handler the daemon wires into the api mux:
// reader is the meta-backed (or fake) lineage read seam. A nil logger discards.
func newRunTracePlane(reader store.Reader, logger *slog.Logger) *runTracePlane {
	if logger == nil {
		logger = slog.New(slog.NewTextHandler(io.Discard, nil))
	}
	return &runTracePlane{reader: reader, logger: logger}
}

// Trace walks the run's ancestry (direction up or the empty default) or its
// descendants (direction down) over run_inputs and returns the walked edges. A
// malformed id is a bad-request-shaped error; an id that resolves to no run (nor its
// archival summary) is a not-found-shaped error. A run with no consumption edges in
// the asked direction resolves to an empty (present) ancestry, never an error.
func (p *runTracePlane) Trace(ctx context.Context, id, direction string) (any, error) {
	n, err := strconv.ParseInt(id, 10, 64)
	if err != nil {
		return nil, fmt.Errorf("daemon: run trace: %q is not a run id", id)
	}

	linStore, err := p.reader.ProvenanceLineage(ctx)
	if err != nil {
		p.logger.Error("run trace lineage read failed", "run", id, "direction", direction, "err", err)
		return nil, fmt.Errorf("daemon: run trace %s: read lineage: %w", id, err)
	}
	lin := metaLineageToPg(linStore)
	if _, ok := lin.RunFacts(n); !ok {
		return nil, fmt.Errorf("daemon: run %s not found", id)
	}

	dir := direction
	if dir == "" {
		dir = "up"
	}
	var edges []pg.AncestryEdge
	switch dir {
	case "up":
		edges = lin.Ancestry(n, pg.FullAncestry)
	case "down":
		edges = lin.Descendants(n, pg.FullAncestry)
	default:
		// The mux already closed direction to up|down before routing here; guard
		// anyway rather than silently walk the wrong way.
		return nil, fmt.Errorf("daemon: run trace %s: unknown direction %q", id, direction)
	}

	out := make([]api.TraceEdge, 0, len(edges))
	for _, e := range edges {
		out = append(out, api.TraceEdge{
			RunID:         strconv.FormatInt(e.RunID, 10),
			UpstreamRunID: strconv.FormatInt(e.UpstreamRunID, 10),
			Depth:         e.Depth,
		})
	}
	return api.RunTracePayload{Run: id, Direction: dir, Ancestry: out}, nil
}
