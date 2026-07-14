package store

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5/pgconn"
)

// This file is the map-shaped execution surface the data routes ride: one call
// executes one engine-built read statement through the shared read pool's full
// checkout cycle and returns the served rows as column-keyed maps in served order
// -- the form /q and /data serialize. It also translates the one Postgres refusal
// the surface must recognize: SQLSTATE 42501 (insufficient_privilege), the
// database physically bounding a role's read to its granted fields, surfaces as
// ErrReadForbidden so the route layer can answer 403 forbidden without ever
// parsing Postgres error text.

// ErrReadForbidden marks a read Postgres itself refused for a missing grant
// (SQLSTATE 42501): the caller's role lacks a privilege on a field the
// statement touches. The route layer maps it to the 403 forbidden envelope --
// naming the addressed endpoint or table, never the missing fields.
var ErrReadForbidden = errors.New("store: postgres refused the read: the role lacks a grant")

// sqlstateInsufficientPrivilege is Postgres' SQLSTATE for a privilege
// violation: the code the database raises when a role touches a column or
// table it was never granted.
const sqlstateInsufficientPrivilege = "42501"

// ExecuteRead runs one engine-built statement as role through the shared
// pool's SET ROLE / read-only transaction / RESET ROLE cycle (Read), scanning
// every served row into a column-keyed map in served order. columns is the
// statement's projection, in SELECT order; name and text form the fixed
// prepared statement (NewReadStatement refuses anything but a single SELECT).
// A Postgres privilege refusal is returned wrapped in ErrReadForbidden.
func (p *ReadPool) ExecuteRead(ctx context.Context, role, name, text string, args []any, columns []string) ([]map[string]any, error) {
	if role == "" {
		return nil, fmt.Errorf("store: execute read %s: %w", name, ErrInvalidRoleOwner)
	}
	return p.executeRead(ctx, role, name, text, args, columns)
}

// ExecuteReadSelf runs one engine-built statement as the pool's own login role --
// the engine itself, the identity an ambient (unix-socket) request carries
// (socket requests are ambiently authorized). It is a separate, deliberate entry
// so no caller can reach the engine's own read authority by accidentally passing
// an empty role to ExecuteRead. The cycle is identical minus SET ROLE.
func (p *ReadPool) ExecuteReadSelf(ctx context.Context, name, text string, args []any, columns []string) ([]map[string]any, error) {
	return p.executeRead(ctx, "", name, text, args, columns)
}

// executeRead builds the fixed statement, runs the checkout cycle, and scans
// rows into column maps.
func (p *ReadPool) executeRead(ctx context.Context, role, name, text string, args []any, columns []string) ([]map[string]any, error) {
	if len(columns) == 0 {
		return nil, fmt.Errorf("store: execute read %s: empty column list", name)
	}
	stmt, err := NewReadStatement(name, text)
	if err != nil {
		return nil, fmt.Errorf("store: execute read: %w", err)
	}

	var out []map[string]any
	consume := func(rows ReadRows) error {
		for rows.Next() {
			vals := make([]any, len(columns))
			ptrs := make([]any, len(columns))
			for i := range vals {
				ptrs[i] = &vals[i]
			}
			if err := rows.Scan(ptrs...); err != nil {
				return err
			}
			row := make(map[string]any, len(columns))
			for i, c := range columns {
				row[c] = vals[i]
			}
			out = append(out, row)
		}
		return nil
	}

	if err := p.read(ctx, role, stmt, args, consume); err != nil {
		var pgErr *pgconn.PgError
		if errors.As(err, &pgErr) && pgErr.Code == sqlstateInsufficientPrivilege {
			// Postgres physically bounded the read; keep both the sentinel and the
			// original chain (the SQLSTATE stays reachable for logs and tests).
			return nil, fmt.Errorf("%w: %w", ErrReadForbidden, err)
		}
		return nil, err
	}
	return out, nil
}
