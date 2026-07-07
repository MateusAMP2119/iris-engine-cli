package store

import (
	"context"
	"fmt"
)

// This file is the applied-migration-head read path: the plain-MVCC read of the meta
// migrations table that reconstructs each declared table's applied head, so
// provisioning can diff the on-disk migration ledger against what is already applied
// and stay idempotent (specification sections 4 and 5). Like every meta read it is a
// plain pooled MVCC query, never serialized through the single writer and never
// retried.

// AppliedHeadReader reads each declared table's greatest applied migration id from
// the meta migrations ledger. A pgx-pool-backed implementation and a fake both
// satisfy it; provisioning consults it to build the per-table ledger view it plans
// against.
type AppliedHeadReader interface {
	// AppliedHeads returns a map from "schema.table" to the greatest migration id
	// recorded applied for that table, omitting tables with no recorded head.
	AppliedHeads(ctx context.Context) (map[string]string, error)
}

// selectAppliedHeadsSQL reads the greatest applied migration id per declared table.
// The ids are zero-padded fixed-width (e.g. "0001", "0002"), so the lexicographic
// max is the numeric head; the reserved column names schema and table are quoted.
const selectAppliedHeadsSQL = `SELECT "schema", "table", max(migration_id)
FROM migrations
GROUP BY "schema", "table"`

// pgxAppliedHeadReader is the pgx-pool-backed AppliedHeadReader: a plain MVCC read
// over the reader pool, no session pinning and no busy-retry.
type pgxAppliedHeadReader struct {
	pool readPool
}

// compile-time proof the pgx adapter satisfies the applied-head read seam.
var _ AppliedHeadReader = (*pgxAppliedHeadReader)(nil)

// AppliedHeads reads the greatest applied migration id per table in one plain MVCC
// query. A query error is returned immediately, never retried.
func (r *pgxAppliedHeadReader) AppliedHeads(ctx context.Context) (map[string]string, error) {
	rows, err := r.pool.query(ctx, selectAppliedHeadsSQL)
	if err != nil {
		return nil, fmt.Errorf("store: read applied migration heads: %w", err)
	}
	defer rows.Close()

	out := map[string]string{}
	for rows.Next() {
		var schema, table, head string
		if err := rows.Scan(&schema, &table, &head); err != nil {
			return nil, fmt.Errorf("store: scan applied migration head: %w", err)
		}
		out[schema+"."+table] = head
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: read applied migration heads: %w", err)
	}
	return out, nil
}
