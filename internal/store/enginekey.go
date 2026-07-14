package store

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"
)

// This file is the meta persistence of the engine's ed25519 signing key. The key
// lives in the single-row engine_key control table (id pinned to 1), not a
// per-database GUC (ALTER DATABASE ... SET needs SUPERUSER the external admin role
// lacks) and not a workspace file (which would force a shared filesystem for HA).
// The shared meta database standbys already read gives HA superuser-free: a
// restarted or standby daemon reads the same key back.
//
// store owns only the bytes: the read seam returns the raw private half and the
// write seam persists it, both keyed on id = 1. The daemon owns the crypto (mint,
// sign, decode) and renders/redacts the material; store never imports crypto and
// never logs the private half.

// selectEngineKeySQL reads the single engine key row's private half. It is a plain
// MVCC read on the reader pool.
const selectEngineKeySQL = `SELECT private_key FROM engine_key WHERE id = 1`

// insertEngineKeySQL mints the engine key create-once: it inserts the single row
// (id pinned to 1) and does nothing on a conflict, so two candidates that both
// mint converge on whichever inserted first -- the loser's INSERT is a no-op and it
// reads the winner's key back. The private half rides a bind parameter ($1), never
// the SQL text, so it never reaches a statement log.
const insertEngineKeySQL = `INSERT INTO engine_key (id, private_key, created_at)
VALUES (1, $1, $2)
ON CONFLICT (id) DO NOTHING`

// ReadEngineKey reads the raw ed25519 private key bytes from the single-row
// engine_key meta table in one plain MVCC query, returning (nil, nil) when the
// table holds no key yet (the seal mints one on first need). It satisfies the
// JournalSealReader read seam.
func (r *pgxSealReader) ReadEngineKey(ctx context.Context) ([]byte, error) {
	rows, err := r.pool.query(ctx, selectEngineKeySQL)
	if err != nil {
		return nil, fmt.Errorf("store: read engine key: %w", err)
	}
	defer rows.Close()

	if !rows.Next() {
		if err := rows.Err(); err != nil {
			return nil, fmt.Errorf("store: read engine key: %w", err)
		}
		return nil, nil // no key row yet
	}
	var priv []byte
	if err := rows.Scan(&priv); err != nil {
		return nil, fmt.Errorf("store: scan engine key: %w", err)
	}
	return priv, nil
}

// ReadEngineKeyOnce opens a short-lived read connection to the meta database from
// src and returns the raw ed25519 private-key bytes the single-row engine_key table
// holds, for the daemonless `iris engine info` key surface. It is a plain SELECT on
// a connection that must already find meta present: it never creates the meta
// database or any table (unlike Connect), so an absent meta database, an absent
// engine_key table, or an unreachable cluster surfaces as a connection/query error
// the caller maps to "engine not installed or unreachable", and an empty table
// returns (nil, nil). The private bytes are the private half: a caller must never
// log or render them. The connection is always closed (on a background context so a
// cancelled read still releases it).
func ReadEngineKeyOnce(ctx context.Context, src ConnSource) ([]byte, error) {
	if src == nil {
		return nil, errors.New("store: nil connection source")
	}
	cfg, err := metaConnConfig(src.ConnString())
	if err != nil {
		return nil, err
	}
	conn, err := pgx.ConnectConfig(ctx, cfg)
	if err != nil {
		return nil, fmt.Errorf("store: open meta connection to read engine key: %w", err)
	}
	defer func() { _ = conn.Close(context.Background()) }()

	var priv []byte
	err = conn.QueryRow(ctx, selectEngineKeySQL).Scan(&priv)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil // no key row yet
	}
	if err != nil {
		return nil, fmt.Errorf("store: read engine key: %w", err)
	}
	return priv, nil
}

// InsertEngineKey mints the engine key into the single-row engine_key meta table
// create-once (INSERT ... ON CONFLICT (id) DO NOTHING): a second minter that lost
// the race is a silent no-op, so the key can never fork under two candidates.
// priv is the raw ed25519 private half and rides a bind parameter, never the SQL
// text, so the private material never reaches a statement log. It is a
// leader-only meta write, riding the single Writer. createdAt is an opaque audit
// string.
func (w *Writer) InsertEngineKey(ctx context.Context, priv []byte, createdAt string) error {
	if err := w.conn.Exec(ctx, insertEngineKeySQL, priv, createdAt); err != nil {
		return fmt.Errorf("store: writer insert engine key: %w", err)
	}
	return nil
}
