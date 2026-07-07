package store

import (
	"context"
	"encoding/json"
	"fmt"
)

// This file is the persistence half of the endpoint apply lifecycle
// (specification section 7): the meta writes that publish and retire declared
// read endpoints in the endpoints and endpoint_filters tables (shape locked by
// S04/endpoints-table-shape). Like every registry write it rides the single
// meta writer, and each operation is one atomic transaction: an apply persists
// every endpoint's shape row and filter rows together or not at all
// (all-or-nothing, effective on commit), and a remove retires the filter rows
// and the shape row together. The derived SQL is deliberately not persisted:
// it is a pure function of the shape (S07/endpoint-sql-deterministic), so the
// daemon recompiles it from these rows rather than trusting a stored text.

// EndpointFilterRow is one persisted endpoint filter param: its query-param
// name (the source column it filters) and its kind, drawn from the closed set
// the endpoint_filters CHECK pins (eq, range). One endpoint_filters row.
type EndpointFilterRow struct {
	// Param is the filter's query-param name (the source column).
	Param string
	// Op is the filter kind: eq or range.
	Op string
}

// EndpointRow is one persisted read endpoint: the endpoints row (name keys the
// shape; source is the dotted schema.table; Fields persists as the JSON
// projection; Sort is the unique keyset column) plus its filter rows in
// declaration order.
type EndpointRow struct {
	// Name is the endpoint name, the endpoints primary key.
	Name string
	// Source is the dotted schema.table the endpoint reads.
	Source string
	// Fields is the explicit field projection, persisted as JSON.
	Fields []string
	// Sort is the keyset-pagination column.
	Sort string
	// Filters are the endpoint's filter params, in declaration order.
	Filters []EndpointFilterRow
}

// The endpoint lifecycle statements. Each is a single parameterized statement;
// the writer methods group them into one atomic transaction per operation.
const (
	// endpointUpsertSQL publishes or refreshes one endpoint's shape row. On
	// re-apply the shape columns are replaced wholesale: apply owns every column,
	// so nothing is preserved across a shape change.
	endpointUpsertSQL = `INSERT INTO endpoints (name, source, fields, sort)
VALUES ($1, $2, $3, $4)
ON CONFLICT (name) DO UPDATE SET source = EXCLUDED.source, fields = EXCLUDED.fields, sort = EXCLUDED.sort`

	// deleteEndpointFiltersSQL clears an endpoint's filter rows so a re-apply
	// replaces the filter grammar wholesale rather than accumulating stale rows.
	deleteEndpointFiltersSQL = `DELETE FROM endpoint_filters WHERE endpoint = $1`

	// insertEndpointFilterSQL writes one filter row, keyed (endpoint, param).
	insertEndpointFilterSQL = `INSERT INTO endpoint_filters (endpoint, param, op) VALUES ($1, $2, $3)`

	// deleteEndpointSQL retires an endpoint's shape row, after its filter rows
	// (the foreign-key child goes first).
	deleteEndpointSQL = `DELETE FROM endpoints WHERE name = $1`
)

// ApplyEndpoints persists a set of compiled endpoints as one atomic meta
// transaction (specification section 7: iris endpoint apply is atomic,
// all-or-nothing, effective on commit): for each endpoint, an upsert of its
// endpoints row, a clearing delete of its endpoint_filters rows, and one
// insert per filter in declaration order. Every endpoint in the set commits
// together or none does, so a multi-endpoint apply never publishes half a
// folder. Re-apply rides the same path: the upsert replaces the shape columns
// and the delete+insert replaces the filter grammar wholesale. It touches no
// workload table (pipelines, dependencies, lanes): the endpoint lifecycle is
// independent of declare apply. It is a leader-only meta write, riding the
// single Writer.
func (w *Writer) ApplyEndpoints(ctx context.Context, rows []EndpointRow) error {
	var stmts []Statement
	for _, row := range rows {
		fieldsJSON, err := json.Marshal(row.Fields)
		if err != nil {
			return fmt.Errorf("store: writer apply endpoint %q: marshal fields: %w", row.Name, err)
		}
		stmts = append(stmts,
			Statement{SQL: endpointUpsertSQL, Args: []any{row.Name, row.Source, string(fieldsJSON), row.Sort}},
			Statement{SQL: deleteEndpointFiltersSQL, Args: []any{row.Name}},
		)
		for _, f := range row.Filters {
			stmts = append(stmts, Statement{SQL: insertEndpointFilterSQL, Args: []any{row.Name, f.Param, f.Op}})
		}
	}
	if err := w.execTx(ctx, stmts); err != nil {
		return fmt.Errorf("store: writer apply %d endpoint(s): %w", len(rows), err)
	}
	return nil
}

// RemoveEndpoint retires one endpoint's persisted shape as one atomic meta
// transaction: its filter rows first (the foreign-key child), then its
// endpoints row. It deletes shape only -- no declared table, no data, no
// workload row is touched (specification section 7: remove retires a read
// surface, independent of declare destroy). It is a leader-only meta write,
// riding the single Writer.
func (w *Writer) RemoveEndpoint(ctx context.Context, name string) error {
	stmts := []Statement{
		{SQL: deleteEndpointFiltersSQL, Args: []any{name}},
		{SQL: deleteEndpointSQL, Args: []any{name}},
	}
	if err := w.execTx(ctx, stmts); err != nil {
		return fmt.Errorf("store: writer remove endpoint %q: %w", name, err)
	}
	return nil
}
