package store

import (
	"context"
	"fmt"
	"strings"
	"testing"
)

// endpointRows is a poolRows fake over a fixed set of rows, each a slice of column
// values assigned into Scan's destination pointers in order.
type endpointRows struct {
	rows [][]any
	pos  int
}

func (r *endpointRows) Next() bool { r.pos++; return r.pos <= len(r.rows) }
func (r *endpointRows) Err() error { return nil }
func (r *endpointRows) Close()     {}
func (r *endpointRows) Scan(dest ...any) error {
	row := r.rows[r.pos-1]
	if len(dest) != len(row) {
		return fmt.Errorf("scan arity %d != row width %d", len(dest), len(row))
	}
	for i, d := range dest {
		switch p := d.(type) {
		case *string:
			*p = row[i].(string)
		case *[]byte:
			*p = row[i].([]byte)
		default:
			return fmt.Errorf("endpointRows: unsupported dest type %T", d)
		}
	}
	return nil
}

// endpointPool dispatches the two endpoint reads by SQL prefix, returning scripted
// rows for each.
type endpointPool struct {
	endpoints [][]any
	filters   [][]any
}

func (p *endpointPool) query(_ context.Context, sql string, _ ...any) (poolRows, error) {
	if strings.HasPrefix(sql, "SELECT name, source, fields, sort FROM endpoints") {
		return &endpointRows{rows: p.endpoints}, nil
	}
	return &endpointRows{rows: p.filters}, nil
}

// TestReadEndpointsJoinsShapeAndFilters proves the endpoint reader reconstructs each
// persisted endpoint from its shape row plus its filter rows: it decodes the JSON
// field projection, groups filters onto their endpoint by name, and returns endpoints
// in name order -- exactly the shape the startup reload recompiles from.
func TestReadEndpointsJoinsShapeAndFilters(t *testing.T) {
	t.Run("endpoints-reload-from-meta", func(t *testing.T) {
		pool := &endpointPool{
			endpoints: [][]any{
				{"orders_by_customer", "analytics.orders", []byte(`["id","customer_id","amount"]`), "id"},
				{"orders_recent", "analytics.orders", []byte(`["id","amount"]`), "id"},
			},
			filters: [][]any{
				{"orders_by_customer", "amount", "range"},
				{"orders_by_customer", "customer_id", "eq"},
			},
		}
		r := newPgxEndpointReader(pool)

		rows, err := r.ReadEndpoints(context.Background())
		if err != nil {
			t.Fatalf("ReadEndpoints: %v", err)
		}
		if len(rows) != 2 {
			t.Fatalf("read %d endpoints, want 2", len(rows))
		}
		if rows[0].Name != "orders_by_customer" || rows[1].Name != "orders_recent" {
			t.Errorf("endpoints not in name order: %q, %q", rows[0].Name, rows[1].Name)
		}
		if got := rows[0].Fields; len(got) != 3 || got[0] != "id" || got[2] != "amount" {
			t.Errorf("field projection = %v, want [id customer_id amount]", got)
		}
		if got := rows[0].Filters; len(got) != 2 || got[0].Param != "amount" || got[0].Op != "range" || got[1].Param != "customer_id" {
			t.Errorf("filters = %+v, want the two grouped filters in keyed order", got)
		}
		if len(rows[1].Filters) != 0 {
			t.Errorf("orders_recent has %d filters, want 0 (none persisted)", len(rows[1].Filters))
		}
	})
}
