package daemon

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"strconv"

	"github.com/MateusAMP2119/iris-engine-cli/internal/api"
	"github.com/MateusAMP2119/iris-engine-cli/internal/store"
)

// This file is the daemon's runs-collection plane: the api.RunsHandler behind GET
// /runs[?include=inputs] and GET /runs/{id} (and therefore behind `iris run list` --
// specification section 7). It composes the store's plain-MVCC run-lineage reads
// into the collection the rail renderer draws: each row its run, and -- under
// include=inputs -- its consumed upstream ids and replayed_from as plain attributes
// (parents-per-row, never a separate edge array). It is a read, served on any role
// from the reader pool, and mutates nothing.
//
// The consumed upstream ids come straight off run_inputs, which is FK-free
// (specification section 4): an id may name a run since pruned, so it is carried
// verbatim -- the renderer's visible gap, never resolved away against a live run.

// runsPlane is the api.RunsHandler over the store's run-lineage read seam.
type runsPlane struct {
	reader store.RunLineageReader
	logger *slog.Logger
}

// compile-time proof the plane satisfies the mux's runs-collection seam.
var _ api.RunsHandler = (*runsPlane)(nil)

// newRunsPlane builds the runs-collection handler the daemon wires into the api
// mux: reader is the meta-backed (or fake) run-lineage read seam. A nil logger
// discards output.
func newRunsPlane(reader store.RunLineageReader, logger *slog.Logger) *runsPlane {
	if logger == nil {
		logger = slog.New(slog.NewTextHandler(io.Discard, nil))
	}
	return &runsPlane{reader: reader, logger: logger}
}

// ListRuns returns the whole run history as the runs collection, newest first.
// Under includeInputs each row carries its consumed upstream ids and replayed_from;
// without it the rows are bare (id, pipeline, state). The result is a RunsCollection
// so the enveloped read is { "data": { "runs": [...] } } and the NDJSON stream
// serves the same rows one per line.
func (p *runsPlane) ListRuns(ctx context.Context, includeInputs bool) (any, error) {
	rows, err := p.reader.RunLineages(ctx)
	if err != nil {
		p.logger.Error("run list failed", "err", err)
		return nil, fmt.Errorf("daemon: run list: %w", err)
	}
	out := make([]api.RunRow, 0, len(rows))
	for _, rl := range rows {
		out = append(out, runRowFrom(rl, includeInputs))
	}
	return api.RunsCollection{Runs: out}, nil
}

// GetRun returns a single run by id with the same optional lineage attributes. A
// malformed id is a bad-request-shaped error; an id that resolves to no run is a
// not-found-shaped error (never a fabricated empty run).
func (p *runsPlane) GetRun(ctx context.Context, id string, includeInputs bool) (any, error) {
	n, err := strconv.ParseInt(id, 10, 64)
	if err != nil {
		return nil, fmt.Errorf("daemon: run show: %q is not a run id", id)
	}
	rl, found, err := p.reader.RunLineage(ctx, n)
	if err != nil {
		p.logger.Error("run show failed", "run", id, "err", err)
		return nil, fmt.Errorf("daemon: run show %s: %w", id, err)
	}
	if !found {
		return nil, fmt.Errorf("daemon: run %s not found", id)
	}
	return runRowFrom(rl, includeInputs), nil
}

// runRowFrom maps one store.RunLineage into the wire row. The lineage attributes
// (consumed upstream ids, replayed_from) ride only under includeInputs -- the bare
// /runs view is id, pipeline, state alone (specification section 7). Inputs are
// rendered as the upstream ids' decimal strings, ascending, one solid edge each.
func runRowFrom(rl store.RunLineage, includeInputs bool) api.RunRow {
	row := api.RunRow{
		ID:       strconv.FormatInt(rl.ID, 10),
		Pipeline: rl.Pipeline,
		State:    string(rl.State),
	}
	if !includeInputs {
		return row
	}
	if len(rl.Inputs) > 0 {
		row.Inputs = make([]string, len(rl.Inputs))
		for i, up := range rl.Inputs {
			row.Inputs[i] = strconv.FormatInt(up, 10)
		}
	}
	if rl.ReplayedFrom != nil {
		row.ReplayedFrom = strconv.FormatInt(*rl.ReplayedFrom, 10)
	}
	return row
}
