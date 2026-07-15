package store

import (
	"context"
	"fmt"

	"github.com/MateusAMP2119/iris-lakehouse/internal/declare"
)

// This file is the meta read path for grant-drift detection: the plain-MVCC read
// of every data-PAT role's ledgered field grants (the roles rows minted by
// `iris pat create` and their grants rows), the authoritative set the leader
// reconciles each role's live Postgres grants against.

// RoleGrantLedger is one data-PAT role with its ledgered field grants. A role
// with no grants rows still appears (empty Grants), so a role holding only
// out-of-band grants is still reconciled and its strays reported.
type RoleGrantLedger struct {
	// Role is the data-PAT role name (roles.pg_role, pat non-null).
	Role string
	// Grants are the role's ledgered field grants, in stable order.
	Grants []declare.FieldGrant
}

// DataPATGrantsReader is the plain-MVCC read seam grant-drift reconciliation
// draws from. A pgx-pool-backed implementation and a fake both satisfy it.
type DataPATGrantsReader interface {
	// DataPATRoleGrants returns every data-PAT role with its ledgered grants,
	// ascending by role.
	DataPATRoleGrants(ctx context.Context) ([]RoleGrantLedger, error)
}

// selectDataPATGrantsSQL reads every data-PAT role (roles.pat non-null) with its
// grants rows; the LEFT JOIN keeps a grant-less role in the result so it is
// still reconciled.
const selectDataPATGrantsSQL = `SELECT r.pg_role, coalesce(g."schema", ''), coalesce(g."table", ''), coalesce(g.field, ''), coalesce(g.access, '')
FROM roles r LEFT JOIN grants g ON g.pg_role = r.pg_role
WHERE r.pat IS NOT NULL
ORDER BY r.pg_role, g."schema", g."table", g.field, g.access`

// pgxDataPATGrantsReader is the pgx-pool-backed DataPATGrantsReader.
type pgxDataPATGrantsReader struct {
	pool readPool
}

// compile-time proof the pgx adapter satisfies the drift read seam.
var _ DataPATGrantsReader = (*pgxDataPATGrantsReader)(nil)

// newPgxDataPATGrantsReader builds a drift reader over a pooled-query seam.
func newPgxDataPATGrantsReader(pool readPool) *pgxDataPATGrantsReader {
	return &pgxDataPATGrantsReader{pool: pool}
}

// DataPATRoleGrants reads the roles and their grants in one plain MVCC query.
func (r *pgxDataPATGrantsReader) DataPATRoleGrants(ctx context.Context) ([]RoleGrantLedger, error) {
	rows, err := r.pool.query(ctx, selectDataPATGrantsSQL)
	if err != nil {
		return nil, fmt.Errorf("store: read data-PAT grant ledger: %w", err)
	}
	defer rows.Close()

	var out []RoleGrantLedger
	for rows.Next() {
		var role, schema, table, field, access string
		if err := rows.Scan(&role, &schema, &table, &field, &access); err != nil {
			return nil, fmt.Errorf("store: scan data-PAT grant row: %w", err)
		}
		if len(out) == 0 || out[len(out)-1].Role != role {
			out = append(out, RoleGrantLedger{Role: role})
		}
		if schema == "" && table == "" && field == "" {
			continue // a grant-less role: the LEFT JOIN's empty row.
		}
		last := &out[len(out)-1]
		last.Grants = append(last.Grants, declare.FieldGrant{
			Schema: schema, Table: table, Field: field, Access: declare.AccessKind(access),
		})
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: read data-PAT grant ledger: %w", err)
	}
	return out, nil
}
