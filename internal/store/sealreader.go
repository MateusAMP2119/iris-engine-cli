package store

import (
	"context"
	"fmt"
)

// This file is the meta read seam the leader-side seal step draws on. Sealing is
// a leader-only, opportunistic dispatcher step, but the two facts it reads are
// plain-MVCC meta reads, so they ride the reader pool exactly like every other
// read seam here: the chain's current head (the parent the next checkpoint links
// to) and the count of in-flight runs that wrote into the resident partition (a
// partition seals only once every in-flight run writing into it has finished).
// The engine key that signs the checkpoint is not a meta read -- it is loaded
// from the engine-owned workspace key file, which is non-superuser-safe unlike
// the per-database GUC install cannot set in external mode.

// JournalSealReader is the meta read seam the seal step consults: the checkpoint
// chain head to link to, the in-flight run count that gates whether a seal may
// cut, and the engine key material the checkpoint is signed with. A meta-backed
// implementation and a test fake both satisfy it; reads are plain MVCC through the
// reader pool, never serialized through the writer.
type JournalSealReader interface {
	// LatestCheckpoint returns the chain head (highest seq journal_checkpoints row),
	// or nil when the chain is empty (the next checkpoint is the first, parent nil).
	LatestCheckpoint(ctx context.Context) (*CheckpointRow, error)
	// RunningAmong returns how many of the given run ids are currently in the running
	// state: the in-flight runs that wrote into the resident partition and a seal must
	// wait for before it may cut. An empty id set counts zero (no writer is in flight).
	RunningAmong(ctx context.Context, runIDs []int64) (int64, error)
	// ReadEngineKey returns the raw ed25519 private key bytes stored in the
	// single-row engine_key meta table. It returns (nil, nil) when the table
	// holds no key yet, so the seal can mint one on first need; the daemon
	// decodes the bytes into its EngineKey (store never imports crypto). The
	// bytes are the private half: a caller must never log or render them.
	ReadEngineKey(ctx context.Context) ([]byte, error)
}

// The seal read statements. Each is a single plain SELECT (an MVCC snapshot), no
// locking clause, no advisory-lock interplay.
const (
	selectLatestCheckpointSQL = `SELECT seq, id_from, id_to, digest, parent_digest, signature, location, coalesce(recorded_at, '')
FROM journal_checkpoints ORDER BY seq DESC LIMIT 1`
	selectRunningAmongSQL = `SELECT count(*) FROM runs WHERE state = $1 AND id = ANY($2)`
)

// pgxSealReader is the pgx-pool-backed JournalSealReader.
type pgxSealReader struct {
	pool readPool
}

// compile-time proof the pgx adapter satisfies the seal read seam.
var _ JournalSealReader = (*pgxSealReader)(nil)

// newPgxSealReader builds a seal reader over a pooled-query seam.
func newPgxSealReader(pool readPool) *pgxSealReader { return &pgxSealReader{pool: pool} }

// LatestCheckpoint reads the chain head (highest seq), or nil when the chain is
// empty, in one plain MVCC query.
func (r *pgxSealReader) LatestCheckpoint(ctx context.Context) (*CheckpointRow, error) {
	rows, err := r.pool.query(ctx, selectLatestCheckpointSQL)
	if err != nil {
		return nil, fmt.Errorf("store: read latest checkpoint: %w", err)
	}
	defer rows.Close()

	if !rows.Next() {
		if err := rows.Err(); err != nil {
			return nil, fmt.Errorf("store: read latest checkpoint: %w", err)
		}
		return nil, nil
	}
	var cp CheckpointRow
	if err := rows.Scan(&cp.Seq, &cp.IDFrom, &cp.IDTo, &cp.Digest, &cp.ParentDigest, &cp.Signature, &cp.Location, &cp.RecordedAt); err != nil {
		return nil, fmt.Errorf("store: scan latest checkpoint: %w", err)
	}
	return &cp, nil
}

// RunningAmong reads how many of runIDs are in the running state in one plain MVCC
// query: the in-flight runs that wrote into the resident partition and a seal must
// wait for. An empty id set short-circuits to zero (no writer is in flight).
func (r *pgxSealReader) RunningAmong(ctx context.Context, runIDs []int64) (int64, error) {
	if len(runIDs) == 0 {
		return 0, nil
	}
	rows, err := r.pool.query(ctx, selectRunningAmongSQL, RunRunning, runIDs)
	if err != nil {
		return 0, fmt.Errorf("store: read running runs among writers: %w", err)
	}
	defer rows.Close()

	if !rows.Next() {
		if err := rows.Err(); err != nil {
			return 0, fmt.Errorf("store: read running runs among writers: %w", err)
		}
		return 0, nil
	}
	var n int64
	if err := rows.Scan(&n); err != nil {
		return 0, fmt.Errorf("store: scan running runs among writers: %w", err)
	}
	return n, nil
}
