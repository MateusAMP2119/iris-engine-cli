package store

import (
	"context"
	"encoding/json"
	"fmt"
)

// This file is the read-back half of the endpoint apply lifecycle (specification
// section 7): the plain-MVCC reader the daemon reloads persisted endpoints from at
// startup, so a restart or failover serves every applied endpoint without a re-apply.
// The endpoints and endpoint_filters rows persist the shape; the daemon recompiles
// the derived SQL from them (S07/endpoint-sql-deterministic), which is why the SQL is
// deliberately never stored. Reads ride the reader pool (any node), never the single
// writer.

const (
	// selectEndpointsSQL reads every persisted endpoint's shape row, name-ordered for
	// a deterministic reload.
	selectEndpointsSQL = `SELECT name, source, fields, sort FROM endpoints ORDER BY name`

	// selectEndpointFiltersSQL reads every persisted filter row, keyed order so each
	// endpoint's filters reload in a stable order. Serving binds request params by
	// name, so absolute filter order is not load-bearing; a stable order keeps the
	// recompiled SQL deterministic.
	selectEndpointFiltersSQL = `SELECT endpoint, param, op FROM endpoint_filters ORDER BY endpoint, param`
)

// EndpointRowReader reads persisted endpoints back from meta for the daemon's
// startup reload. Plain MVCC over the reader pool; a fake stands in at integration
// tier.
type EndpointRowReader interface {
	// ReadEndpoints returns every persisted endpoint (shape row plus its filter rows),
	// name-ordered. An endpoint with no filters returns an empty filter slice.
	ReadEndpoints(ctx context.Context) ([]EndpointRow, error)
}

// pgxEndpointReader is the pgx-pool-backed EndpointRowReader.
type pgxEndpointReader struct {
	pool readPool
}

// compile-time proof the reader satisfies the seam.
var _ EndpointRowReader = (*pgxEndpointReader)(nil)

// newPgxEndpointReader builds the endpoint reader over a pooled-query seam.
func newPgxEndpointReader(pool readPool) *pgxEndpointReader { return &pgxEndpointReader{pool: pool} }

// ReadEndpoints reads the persisted endpoints and their filters in two plain MVCC
// queries and joins them in memory: the shape rows first (name-ordered), then the
// filter rows grouped onto their endpoint by name. Fields is stored as a JSON
// projection and decoded back to the ordered column list. A filter whose endpoint has
// no shape row is skipped (a dangling child the FK forbids anyway). Errors abort with
// no retry.
func (r *pgxEndpointReader) ReadEndpoints(ctx context.Context) ([]EndpointRow, error) {
	shapeRows, err := r.pool.query(ctx, selectEndpointsSQL)
	if err != nil {
		return nil, fmt.Errorf("store: read endpoints: %w", err)
	}
	// Collect the shape rows first, preserving name order, and index them so the
	// filter pass can attach to each endpoint. The rows are closed before the second
	// query so only one pooled connection is checked out at a time.
	var order []string
	byName := map[string]*EndpointRow{}
	if err := func() error {
		defer shapeRows.Close()
		for shapeRows.Next() {
			var row EndpointRow
			var fieldsJSON []byte
			if err := shapeRows.Scan(&row.Name, &row.Source, &fieldsJSON, &row.Sort); err != nil {
				return fmt.Errorf("store: scan endpoint: %w", err)
			}
			if len(fieldsJSON) > 0 {
				if err := json.Unmarshal(fieldsJSON, &row.Fields); err != nil {
					return fmt.Errorf("store: decode endpoint %q fields: %w", row.Name, err)
				}
			}
			order = append(order, row.Name)
			stored := row
			byName[row.Name] = &stored
		}
		return shapeRows.Err()
	}(); err != nil {
		return nil, err
	}

	filterRows, err := r.pool.query(ctx, selectEndpointFiltersSQL)
	if err != nil {
		return nil, fmt.Errorf("store: read endpoint filters: %w", err)
	}
	if err := func() error {
		defer filterRows.Close()
		for filterRows.Next() {
			var endpoint, param, op string
			if err := filterRows.Scan(&endpoint, &param, &op); err != nil {
				return fmt.Errorf("store: scan endpoint filter: %w", err)
			}
			if row, ok := byName[endpoint]; ok {
				row.Filters = append(row.Filters, EndpointFilterRow{Param: param, Op: op})
			}
		}
		return filterRows.Err()
	}(); err != nil {
		return nil, err
	}

	out := make([]EndpointRow, 0, len(order))
	for _, name := range order {
		out = append(out, *byName[name])
	}
	return out, nil
}
