package pg

import (
	"context"
	"fmt"
	"sort"

	"github.com/MateusAMP2119/iris-engine-cli/internal/declare"
)

// This file is the live half of grant-drift detection: the pg_catalog read that
// snapshots a data-PAT role's CURRENT grants on the data database, which
// Reconcile diffs against the meta access ledger. It reads the ACLs directly
// (aclexplode over pg_attribute.attacl and pg_class.relacl) rather than
// information_schema.column_privileges, because the information_schema views
// show only grants where the CURRENT role is grantor or grantee -- an
// out-of-band grant issued by another superuser would be invisible there, and
// invisible drift is exactly what this read exists to catch.

// liveGrantsSQL reads every SELECT/INSERT/UPDATE privilege the role holds on a
// user table, per column: column-level ACLs directly, and table-level ACLs
// expanded to each of the table's columns (a table-wide grant confers every
// column, and the ledger is field-level, so the expansion is what the diff
// compares). System schemas are excluded; the engine's grants live on declared
// user tables.
const liveGrantsSQL = `
SELECT n.nspname, c.relname, a.attname, x.privilege_type
FROM pg_class c
JOIN pg_namespace n ON n.oid = c.relnamespace
JOIN pg_attribute a ON a.attrelid = c.oid AND a.attnum > 0 AND NOT a.attisdropped
CROSS JOIN LATERAL aclexplode(a.attacl) x
WHERE c.relkind IN ('r', 'p')
  AND n.nspname NOT IN ('pg_catalog', 'information_schema')
  AND x.grantee = (SELECT oid FROM pg_roles WHERE rolname = $1)
  AND x.privilege_type IN ('SELECT', 'INSERT', 'UPDATE')
UNION
SELECT n.nspname, c.relname, a.attname, x.privilege_type
FROM pg_class c
JOIN pg_namespace n ON n.oid = c.relnamespace
CROSS JOIN LATERAL aclexplode(c.relacl) x
JOIN pg_attribute a ON a.attrelid = c.oid AND a.attnum > 0 AND NOT a.attisdropped
WHERE c.relkind IN ('r', 'p')
  AND n.nspname NOT IN ('pg_catalog', 'information_schema')
  AND x.grantee = (SELECT oid FROM pg_roles WHERE rolname = $1)
  AND x.privilege_type IN ('SELECT', 'INSERT', 'UPDATE')`

// compile-time proof the live data client satisfies the grant-drift read seam.
var _ LiveGrantReader = (*Client)(nil)

// ReadFieldGrants reads the role's current field-level grants from the data
// database's catalogs and folds them into the ledger's FieldGrant shape: a
// column SELECT is a read grant; a column holding BOTH INSERT and UPDATE is a
// write grant (RenderGrant issues the pair together, so a write in the ledger
// materializes as both -- a column holding only one of the two maps to no write
// and the additive reconcile re-issues the idempotent pair). The result is
// sorted for stable diffs.
func (c *Client) ReadFieldGrants(ctx context.Context, role string) ([]declare.FieldGrant, error) {
	rows, err := c.pool.Query(ctx, liveGrantsSQL, role)
	if err != nil {
		return nil, fmt.Errorf("pg: read live grants for %q: %w", role, err)
	}
	defer rows.Close()

	type colKey struct{ schema, table, field string }
	privs := make(map[colKey]map[string]bool)
	for rows.Next() {
		var schema, table, field, priv string
		if err := rows.Scan(&schema, &table, &field, &priv); err != nil {
			return nil, fmt.Errorf("pg: scan live grant row for %q: %w", role, err)
		}
		k := colKey{schema: schema, table: table, field: field}
		if privs[k] == nil {
			privs[k] = make(map[string]bool, 3)
		}
		privs[k][priv] = true
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("pg: read live grants for %q: %w", role, err)
	}

	var out []declare.FieldGrant
	for k, p := range privs {
		if p["SELECT"] {
			out = append(out, declare.FieldGrant{Schema: k.schema, Table: k.table, Field: k.field, Access: declare.AccessRead})
		}
		if p["INSERT"] && p["UPDATE"] {
			out = append(out, declare.FieldGrant{Schema: k.schema, Table: k.table, Field: k.field, Access: declare.AccessWrite})
		}
	}
	sort.Slice(out, func(i, j int) bool {
		a, b := out[i], out[j]
		if a.Schema != b.Schema {
			return a.Schema < b.Schema
		}
		if a.Table != b.Table {
			return a.Table < b.Table
		}
		if a.Field != b.Field {
			return a.Field < b.Field
		}
		return a.Access < b.Access
	})
	return out, nil
}
