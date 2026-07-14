package store

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// This file is the meta persistence of the engine's shared read-pool login
// secret. The shared iris_engine_read login is the one identity the read pool
// connects as on every node; its password used to be minted fresh on EVERY daemon
// start and the login re-ALTERed to it, so with two daemons on one data cluster
// (an HA standby, or a restart racing a live leader) the last starter's secret
// won and an earlier node's pool then failed to authenticate.
//
// The fix mirrors the engine_key model (internal/store/enginekey.go): the secret
// lives in a single-row engine-owned meta table (id pinned to 1), minted create-
// once (INSERT ... ON CONFLICT DO NOTHING + read-back, so two daemons converge on
// ONE secret), and every daemon start reads the stored secret back rather than
// minting a fresh one. The shared meta database standbys already read gives HA
// superuser-free: a restart or failover leader reuses the same read-pool
// credential.
//
// The credential ride is a bootstrap-provisioning meta write on the reader pool, not
// a workload write through the single leader writer: it runs on every node before
// election (the read pool opens before a candidate wins the lock), exactly parallel
// to ensureMetaDatabase (which CREATE DATABASE metas from any starting node) and to
// the read-pool login DDL (internal/pg). The create-once ON CONFLICT keeps it safe
// under concurrent daemon starts.

const (
	// selectReadPoolCredentialSQL reads the single persisted read-pool secret. Plain
	// MVCC on the reader pool.
	selectReadPoolCredentialSQL = `SELECT secret FROM read_pool_credential WHERE id = 1`

	// insertReadPoolCredentialSQL mints the read-pool secret create-once: it inserts
	// the single row (id pinned to 1) and does nothing on a conflict, RETURNING the
	// inserted secret. Two daemons that both mint converge on whichever inserted
	// first -- the loser's INSERT returns no row (ON CONFLICT DO NOTHING) and it reads
	// the winner's secret back. The secret rides a bind parameter ($1), never the SQL
	// text, so it never reaches a statement log; created_at is filled DB-side.
	insertReadPoolCredentialSQL = `INSERT INTO read_pool_credential (id, secret, created_at)
VALUES (1, $1, now()::text)
ON CONFLICT (id) DO NOTHING
RETURNING secret`
)

// readPoolCredMeta is the meta seam EnsureReadPoolCredential rides: it ensures the
// single-row table exists (bootstrap, create-if-missing) and performs the create-
// once mint+readback. The live implementation runs on the meta reader pool; a fake
// stands in for integration tests, so the create-once reuse is provable with no
// live Postgres.
type readPoolCredMeta interface {
	// ensureReadPoolCredentialTable creates the single-row table if it is missing, so
	// a daemon started against a meta database bootstrapped before this table existed
	// still persists its credential (upgrade-safe, like ensureMetaDatabase).
	ensureReadPoolCredentialTable(ctx context.Context) error
	// mintReadPoolCredential inserts candidate create-once and returns the PERSISTED
	// secret: candidate when this call won the insert, the pre-existing secret when a
	// concurrent daemon minted first.
	mintReadPoolCredential(ctx context.Context, candidate string) (string, error)
}

// pgxReadPoolCredMeta is the pgx-pool-backed read-pool credential seam: it issues the
// create-if-missing table DDL and the create-once mint+readback on the meta pool.
type pgxReadPoolCredMeta struct {
	pool *pgxpool.Pool
}

// compile-time proof the pool adapter satisfies the credential seam.
var _ readPoolCredMeta = (*pgxReadPoolCredMeta)(nil)

func (m *pgxReadPoolCredMeta) ensureReadPoolCredentialTable(ctx context.Context) error {
	if _, err := m.pool.Exec(ctx, readPoolCredentialTableDDL()); err != nil {
		return err
	}
	return nil
}

func (m *pgxReadPoolCredMeta) mintReadPoolCredential(ctx context.Context, candidate string) (string, error) {
	var stored string
	err := m.pool.QueryRow(ctx, insertReadPoolCredentialSQL, candidate).Scan(&stored)
	if err == nil {
		return stored, nil // this start won the create-once race: candidate persisted.
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return "", err
	}
	// ON CONFLICT DO NOTHING returned no row: a concurrent daemon (or a prior start)
	// already minted. Read the winner's secret back so every node converges on ONE
	// credential.
	if err := m.pool.QueryRow(ctx, selectReadPoolCredentialSQL).Scan(&stored); err != nil {
		return "", err
	}
	return stored, nil
}

// EnsureReadPoolCredential returns the engine's shared read-pool login secret,
// minting and persisting it create-once when absent (read_pool_credential). It is
// concurrent-daemon safe: two daemons starting against one cluster converge on
// one secret, and a restart or HA standby reuses the stored secret rather than
// re-minting and resetting the shared login's password. The caller (the daemon
// read-pool open) sets the login's password to the returned secret; because every
// node ALTERs to the SAME persisted secret, no start ever invalidates another
// node's live pool credential.
func (c *Client) EnsureReadPoolCredential(ctx context.Context) (Secret, error) {
	return ensureReadPoolCredential(ctx, &pgxReadPoolCredMeta{pool: c.pool}, GenerateSecret)
}

// ensureReadPoolCredential drives the credential persistence over the meta seam and a
// secret generator, so the create-once reuse is provable against a fake. It ensures
// the table, mints a fresh candidate, and persists-or-reads-back through the seam;
// the returned Secret is always the PERSISTED one, so a second call reuses the first
// call's secret rather than the fresh candidate it generated.
func ensureReadPoolCredential(ctx context.Context, meta readPoolCredMeta, gen func() (Secret, error)) (Secret, error) {
	if err := meta.ensureReadPoolCredentialTable(ctx); err != nil {
		return Secret{}, fmt.Errorf("store: ensure read-pool credential table: %w", err)
	}
	candidate, err := gen()
	if err != nil {
		return Secret{}, err
	}
	stored, err := meta.mintReadPoolCredential(ctx, candidate.reveal())
	if err != nil {
		return Secret{}, fmt.Errorf("store: mint read-pool credential: %w", err)
	}
	if stored == "" {
		return Secret{}, errors.New("store: read-pool credential missing after create-once mint")
	}
	return Secret{value: stored}, nil
}

// readPoolCredentialTableDDL renders the create-if-missing DDL for the single-row
// read_pool_credential table from the meta schema model, the single source of truth
// for its shape (asserted by the roster and golden-DDL contracts). It is used only
// for the bootstrap create-if-missing at daemon start; the install and leader-election
// schema-ensure paths render the same DDL from the same model.
func readPoolCredentialTableDDL() string {
	for _, t := range MetaSchema().Tables {
		if t.Name == "read_pool_credential" {
			return t.CreateTableDDL()
		}
	}
	// Unreachable: read_pool_credential is a fixed member of the meta roster
	// (TestEighteenTableRoster). An empty string would surface as a clear exec error
	// rather than a silent skip.
	return ""
}
