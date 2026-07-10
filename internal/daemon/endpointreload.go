package daemon

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"path/filepath"

	"github.com/MateusAMP2119/iris-engine-cli/internal/declare"
	"github.com/MateusAMP2119/iris-engine-cli/internal/dispatch"
	"github.com/MateusAMP2119/iris-engine-cli/internal/store"
)

// This file is the daemon's startup endpoint reload (specification section 7):
// endpoints applied via `iris endpoint apply` persist to the endpoints and
// endpoint_filters meta tables, but the LIVE serving registry is in-memory and empty
// each process start, so a restart or failover leader would 500 every /q request
// until a re-apply. Reload closes that gap: it reads the persisted shape rows from
// meta (any node, over the reader pool), recompiles each against the leader's own
// schemas/ tree -- the SQL is never stored, it is a pure function of the shape
// (S07/endpoint-sql-deterministic) -- and swaps the compiled set into the one live
// registry the serving mux checks against. It runs on every node before serving, so
// a restart serves every applied endpoint with no re-apply.
//
// The recompile is deliberately from the PERSISTED rows, not a fresh workspace
// discovery: meta is the truth of what was applied, and the schemas/ tree only
// supplies each source table's shape (columns, types) the derived SQL binds against.
// Declared tables are additive-only (validated sources never lose columns), so a
// persisted endpoint always recompiles; a source that has genuinely drifted logs a
// warning and the endpoint is skipped (it 404s until re-applied) rather than blocking
// the daemon from serving everything else.

// reloadEndpoints reads the persisted endpoints from meta, recompiles them against
// the workspace schemas/ tree, and publishes the compiled set into the live serving
// registry. It is best-effort at the whole-op level: the caller logs a non-nil error
// and keeps serving (a read-surface reload must never block the control plane). An
// individual endpoint that no longer compiles is logged and skipped, never fatal.
func reloadEndpoints(ctx context.Context, reader store.EndpointRowReader, registry *dispatch.EndpointRegistry, workspace string, logger *slog.Logger) error {
	if logger == nil {
		logger = slog.New(slog.NewTextHandler(io.Discard, nil))
	}

	rows, err := reader.ReadEndpoints(ctx)
	if err != nil {
		return fmt.Errorf("reload endpoints: read persisted endpoints: %w", err)
	}
	if len(rows) == 0 {
		return nil // nothing applied yet: an empty registry is correct.
	}

	tables, err := declare.ValidateSchemaTree(filepath.Join(workspace, "schemas"))
	if err != nil {
		return fmt.Errorf("reload endpoints: read schemas tree: %w", err)
	}
	index := declare.TableIndex(tables)

	compiled := make([]*declare.CompiledEndpoint, 0, len(rows))
	for _, row := range rows {
		ce, err := declare.CompileEndpoint(endpointFromRow(row), index)
		if err != nil {
			// A persisted endpoint whose source drifted out from under it: log and skip
			// rather than fail the whole reload (or the daemon). The endpoint 404s until
			// re-applied; every other endpoint still serves.
			logger.Warn("iris daemon: skip reloading endpoint whose source no longer compiles",
				"endpoint", row.Name, "err", err)
			continue
		}
		compiled = append(compiled, ce)
	}

	registry.Reload(compiled)
	logger.Info("iris daemon: reloaded persisted endpoints into the serving registry",
		"persisted", len(rows), "serving", len(compiled))
	return nil
}

// endpointFromRow reconstructs a declare.Endpoint from a persisted store.EndpointRow
// so it can be recompiled against the schemas/ tree. The persisted shape is exactly
// the fields declare.CompileEndpoint validates and derives SQL from: the dotted
// source, the projection, the sort key, and the filter grammar. The []Filter is
// assignable to the Endpoint's filter list (identical underlying type).
func endpointFromRow(row store.EndpointRow) *declare.Endpoint {
	filters := make([]declare.Filter, 0, len(row.Filters))
	for _, f := range row.Filters {
		filters = append(filters, declare.Filter{Param: f.Param, Op: declare.FilterOp(f.Op)})
	}
	ep := &declare.Endpoint{
		Name:   row.Name,
		Source: row.Source,
		Fields: append([]string(nil), row.Fields...),
		Sort:   row.Sort,
	}
	ep.Filters = filters
	return ep
}
