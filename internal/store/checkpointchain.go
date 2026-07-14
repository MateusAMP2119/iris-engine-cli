package store

import (
	"context"
	"fmt"
)

// This file is the meta read seam for the archived half of the checkpoint chain:
// the journal_checkpoints rows whose partitions have been exported to the object
// store and dropped from Postgres. The provenance plane reads it to recover a
// row's stamps from the archived partitions when the resident journal no longer
// holds them -- sealed history stays answerable after the drop, from the object
// store, keyed by each checkpoint's digest.

// CheckpointChainReader is the plain-MVCC read seam for archived checkpoints. A
// pgx-pool-backed implementation and a fake both satisfy it.
type CheckpointChainReader interface {
	// ArchivedCheckpoints returns every checkpoint whose partition has been
	// exported and dropped (location = archived), ascending by seq. Each row's
	// digest names the exported object under the object-store root.
	ArchivedCheckpoints(ctx context.Context) ([]CheckpointRow, error)
}

// selectArchivedCheckpointsSQL reads the archived chain rows, ascending.
const selectArchivedCheckpointsSQL = `SELECT seq, id_from, id_to, digest, parent_digest, signature, location, coalesce(recorded_at, '')
FROM journal_checkpoints WHERE location = 'archived' ORDER BY seq`

// pgxCheckpointChainReader is the pgx-pool-backed CheckpointChainReader.
type pgxCheckpointChainReader struct {
	pool readPool
}

// compile-time proof the pgx adapter satisfies the chain read seam.
var _ CheckpointChainReader = (*pgxCheckpointChainReader)(nil)

// newPgxCheckpointChainReader builds a chain reader over a pooled-query seam.
func newPgxCheckpointChainReader(pool readPool) *pgxCheckpointChainReader {
	return &pgxCheckpointChainReader{pool: pool}
}

// ArchivedCheckpoints reads the archived chain rows in one plain MVCC query.
func (r *pgxCheckpointChainReader) ArchivedCheckpoints(ctx context.Context) ([]CheckpointRow, error) {
	rows, err := r.pool.query(ctx, selectArchivedCheckpointsSQL)
	if err != nil {
		return nil, fmt.Errorf("store: read archived checkpoints: %w", err)
	}
	defer rows.Close()

	var out []CheckpointRow
	for rows.Next() {
		var cp CheckpointRow
		if err := rows.Scan(&cp.Seq, &cp.IDFrom, &cp.IDTo, &cp.Digest, &cp.ParentDigest, &cp.Signature, &cp.Location, &cp.RecordedAt); err != nil {
			return nil, fmt.Errorf("store: scan archived checkpoint row: %w", err)
		}
		out = append(out, cp)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: read archived checkpoints: %w", err)
	}
	return out, nil
}
