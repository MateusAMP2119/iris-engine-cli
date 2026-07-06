package store

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// This file is the live meta client: the one place store turns the daemon-owned
// admin DSN into real connections (specification sections 2, 9, and 10). It owns
// two connection kinds, the split the spec draws:
//
//   - one session-pinned *pgx.Conn -- the leader's single session. It carries the
//     leader-election advisory lock AND the single-writer meta path, so every meta
//     write rides the exact session that holds the lock (specification section 15:
//     "meta writes never ride a session that has not re-acquired the lock").
//   - a *pgxpool.Pool -- readers, plain MVCC, no session pinning, no busy-retry.
//
// This is the sole entry point the daemon calls; the daemon never imports pgx, so
// store stays the only meta database client (specification section 10). It is
// exercised against a real Postgres at conformance tier (the one tier with a live
// database); the seams it composes (leader lock, reader, writer) are proven with
// fakes at integration tier.

// Client is the live meta client: the leader's session (lock + writes) and the
// reader pool, all derived from the one admin DSN. Build it with Connect and tear
// it down with Close.
type Client struct {
	session *pgx.Conn
	pool    *pgxpool.Pool
	lock    *PgxLeaderLock
	writer  MetaWriteConn
	reader  Reader
}

// Connect opens the meta client from the admin-derived connection source: it
// ensures the dedicated meta database exists (CREATE DATABASE if missing, on the
// admin/maintenance connection, since CREATE DATABASE has no IF NOT EXISTS), then
// opens the leader's session-pinned connection and the reader pool against meta.
// It does NOT create the control tables -- that is a leader-only meta write the
// dispatcher issues (Writer.EnsureSchema) once the lock is held. On any error it
// closes whatever it had opened, so a failed Connect leaks no connection.
func Connect(ctx context.Context, src ConnSource) (*Client, error) {
	if src == nil {
		return nil, errors.New("store: nil connection source")
	}
	adminDSN := src.ConnString()

	if err := ensureMetaDatabase(ctx, adminDSN); err != nil {
		return nil, err
	}

	metaCfg, err := metaConnConfig(adminDSN)
	if err != nil {
		return nil, err
	}
	session, err := pgx.ConnectConfig(ctx, metaCfg)
	if err != nil {
		return nil, fmt.Errorf("store: open leader session on meta: %w", err)
	}

	pool, err := metaReaderPool(ctx, adminDSN)
	if err != nil {
		_ = session.Close(ctx)
		return nil, err
	}

	lock, err := newPgxLeaderLock(&pgxSessionConn{conn: session})
	if err != nil {
		pool.Close()
		_ = session.Close(ctx)
		return nil, err
	}

	return &Client{
		session: session,
		pool:    pool,
		lock:    lock,
		writer:  &pgxWriteConn{conn: session},
		reader:  newPgxReader(&pgxReadPool{pool: pool}),
	}, nil
}

// Lock returns the leader-election lock, held on the session-pinned connection.
func (c *Client) Lock() LeaderLock { return c.lock }

// WriteConn returns the leader's single meta write connection: the dispatcher wraps
// it in the one Writer, so every meta write rides the lock-holding session.
func (c *Client) WriteConn() MetaWriteConn { return c.writer }

// Reader returns the plain-MVCC meta reader (the pool), for read paths that must
// never block behind the single writer or the leader lock.
func (c *Client) Reader() Reader { return c.reader }

// Close tears down the client: it closes the reader pool and the leader session. It
// is safe to call after the lock has already released the session (Close is
// idempotent), so the daemon can Release the lock then Close the client.
func (c *Client) Close(ctx context.Context) error {
	if c.pool != nil {
		c.pool.Close()
	}
	if c.session != nil {
		if err := c.session.Close(ctx); err != nil && !c.session.IsClosed() {
			return fmt.Errorf("store: close leader session: %w", err)
		}
	}
	return nil
}

// pgxWriteConn adapts the leader's session *pgx.Conn to the MetaWriteConn seam:
// meta writes ride this one session, the same one the advisory lock is pinned to.
type pgxWriteConn struct {
	conn *pgx.Conn
}

// compile-time proof the leader session adapter satisfies the write seam.
var _ MetaWriteConn = (*pgxWriteConn)(nil)

func (c *pgxWriteConn) Exec(ctx context.Context, sql string, args ...any) error {
	_, err := c.conn.Exec(ctx, sql, args...)
	return err
}

// ensureMetaDatabase creates the dedicated meta database if it does not yet exist,
// on the admin/maintenance connection (the admin DSN as configured points at a
// connectable maintenance database, never at meta, which may not exist yet). The
// probe + create is idempotent: an existing meta database is left untouched.
func ensureMetaDatabase(ctx context.Context, adminDSN string) error {
	conn, err := pgx.Connect(ctx, adminDSN)
	if err != nil {
		return fmt.Errorf("store: open admin/maintenance connection: %w", err)
	}
	defer func() { _ = conn.Close(ctx) }()

	var exists int
	err = conn.QueryRow(ctx, MetaExistsQuery).Scan(&exists)
	switch {
	case errors.Is(err, pgx.ErrNoRows):
		if _, err := conn.Exec(ctx, CreateMetaDatabaseDDL()); err != nil {
			return fmt.Errorf("store: create meta database: %w", err)
		}
	case err != nil:
		return fmt.Errorf("store: probe meta database: %w", err)
	}
	return nil
}

// metaConnConfig parses the admin DSN and points it at the meta database: the
// leader's session connects to meta itself, not the maintenance database.
func metaConnConfig(adminDSN string) (*pgx.ConnConfig, error) {
	cfg, err := pgx.ParseConfig(adminDSN)
	if err != nil {
		return nil, fmt.Errorf("store: parse admin DSN: %w", err)
	}
	cfg.Database = MetaDatabase
	return cfg, nil
}

// metaReaderPool opens the reader pool against the meta database. The pool serves
// plain MVCC reads; it is never the leader lock's connection (that is session-
// pinned), so returning a pooled connection can never release the lock.
func metaReaderPool(ctx context.Context, adminDSN string) (*pgxpool.Pool, error) {
	cfg, err := pgxpool.ParseConfig(adminDSN)
	if err != nil {
		return nil, fmt.Errorf("store: parse admin DSN for reader pool: %w", err)
	}
	cfg.ConnConfig.Database = MetaDatabase
	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		return nil, fmt.Errorf("store: open meta reader pool: %w", err)
	}
	return pool, nil
}
