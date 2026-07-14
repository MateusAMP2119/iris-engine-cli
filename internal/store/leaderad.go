package store

import (
	"context"
	"fmt"
)

// This file is the leader-advertisement read seam: the plain-MVCC read a standby
// uses to learn the current leader's advertised address and name it for
// retargeting. The leader writes the address through the single writer
// (Writer.AdvertiseLeader); a standby -- which shares meta by the HA model --
// reads it here off the reader pool, never blocking behind the leader lock or the
// single writer, so a standby learns the leader's address on any candidate.

// LeaderAddrReader reads the current leader's advertised address from the single-row
// leadership table. A meta-backed implementation and a test fake both satisfy it; the
// read is plain MVCC through the reader pool. An empty return means no leader has
// advertised an address yet (or the leader is socket-only), which the caller renders
// as "unknown".
type LeaderAddrReader interface {
	// LeaderAddr returns the leader's advertised address, or "" when none is recorded
	// (no leadership row yet, or a socket-only leader that advertised the empty
	// address).
	LeaderAddr(ctx context.Context) (string, error)
}

// selectLeaderAddrSQL reads the advertised address from the single-row leadership
// table. It is a plain SELECT (an MVCC snapshot), pinned to the singleton id = 1, no
// locking clause and no advisory-lock interplay.
const selectLeaderAddrSQL = "SELECT advertised_addr FROM leadership WHERE id = 1"

// pgxLeaderAddrReader is the pgx-pool-backed LeaderAddrReader.
type pgxLeaderAddrReader struct {
	pool readPool
}

// compile-time proof the pgx adapter satisfies the leader-address read seam.
var _ LeaderAddrReader = (*pgxLeaderAddrReader)(nil)

// newPgxLeaderAddrReader builds a leader-address reader over a pooled-query seam.
func newPgxLeaderAddrReader(pool readPool) *pgxLeaderAddrReader {
	return &pgxLeaderAddrReader{pool: pool}
}

// LeaderAddr reads the advertised address in one plain MVCC query, returning "" when
// the leadership table holds no row yet (before any leader has advertised).
func (r *pgxLeaderAddrReader) LeaderAddr(ctx context.Context) (string, error) {
	rows, err := r.pool.query(ctx, selectLeaderAddrSQL)
	if err != nil {
		return "", fmt.Errorf("store: read leader address: %w", err)
	}
	defer rows.Close()

	if !rows.Next() {
		if err := rows.Err(); err != nil {
			return "", fmt.Errorf("store: read leader address: %w", err)
		}
		return "", nil
	}
	var addr string
	if err := rows.Scan(&addr); err != nil {
		return "", fmt.Errorf("store: scan leader address: %w", err)
	}
	return addr, nil
}
