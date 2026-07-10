package store

import (
	"context"
	"errors"
	"fmt"
	"sync"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
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
	// adminDSN is the admin-derived connection string the leader session and reader
	// pool are opened from; retained so a demoted daemon can mint a FRESH leader
	// session (a new session-pinned connection carrying a new advisory-lock handle
	// and lock-guarded writer) to re-enter standby -- a dead session can never
	// re-acquire the lock (specification section 15).
	adminDSN string

	// mu guards session across a fresh-session renewal: NewLeaderSession swaps in a
	// new session-pinned connection, and Close reads the current one, so the two must
	// not race. In practice the daemon calls them from a single election goroutine
	// and Close only after it returns, but the guard makes the ownership explicit.
	mu      sync.Mutex
	session *pgx.Conn

	pool     *pgxpool.Pool
	lock     *PgxLeaderLock
	writer   MetaWriteConn
	reader   Reader
	registry RegistryReader
	ledger   AppliedHeadReader
	pipes    PipelineLister
	manual   ManualReader
	show     ShowReader
	promote  PromoteStateReader
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

	session, lock, writer, err := openLeaderSession(ctx, adminDSN)
	if err != nil {
		return nil, err
	}

	pool, err := metaReaderPool(ctx, adminDSN)
	if err != nil {
		_ = session.Close(ctx)
		return nil, err
	}

	readPoolSeam := &pgxReadPool{pool: pool}
	return &Client{
		adminDSN: adminDSN,
		session:  session,
		pool:     pool,
		lock:     lock,
		writer:   writer,
		reader:   newPgxReader(readPoolSeam),
		registry: &pgxRegistryReader{pool: readPoolSeam},
		ledger:   &pgxAppliedHeadReader{pool: readPoolSeam},
		pipes:    newPgxPipelineLister(readPoolSeam),
		manual:   newPgxManualReader(readPoolSeam),
		show:     newPgxShowReader(readPoolSeam),
		promote:  &pgxPromoteReader{pool: readPoolSeam},
	}, nil
}

// openLeaderSession opens a fresh session-pinned connection on the meta database and
// builds the leader lock and the lock-guarded write connection over it: the leader's
// single session, carrying BOTH the advisory lock and the single-writer meta path, so
// every meta write rides the exact session that holds the lock (specification section
// 15). It is the one construction Connect and NewLeaderSession share, so a first
// election and a post-demotion re-entry open identical sessions. On any error it closes
// the connection it opened, leaking nothing.
func openLeaderSession(ctx context.Context, adminDSN string) (*pgx.Conn, *PgxLeaderLock, MetaWriteConn, error) {
	metaCfg, err := metaConnConfig(adminDSN)
	if err != nil {
		return nil, nil, nil, err
	}
	session, err := pgx.ConnectConfig(ctx, metaCfg)
	if err != nil {
		return nil, nil, nil, fmt.Errorf("store: open leader session on meta: %w", err)
	}

	lock, err := newPgxLeaderLock(&pgxSessionConn{conn: session})
	if err != nil {
		_ = session.Close(ctx)
		return nil, nil, nil, err
	}

	// The write connection is the SAME session the leader lock is pinned to, and it
	// is lock-guarded: every meta write first checks that this session currently
	// holds the leader lock, so a write is never issued over a session that has not
	// re-acquired it (specification section 15) -- not before election, and not
	// after a demotion.
	writer, err := NewLockGuardedConn(lock, &pgxWriteConn{conn: session})
	if err != nil {
		_ = session.Close(ctx)
		return nil, nil, nil, err
	}
	return session, lock, writer, nil
}

// NewLeaderSession mints a FRESH leader session for standby re-entry after a
// self-demotion (specification section 15): a NEW session-pinned connection carrying a
// new advisory-lock handle and its lock-guarded write connection. A demoted daemon's
// old session is dead -- it can never re-acquire the lock and its write guard refuses
// forever -- so re-contending requires a genuinely new session, which is exactly what
// this returns. The client tracks the new connection so Close tears down the live
// session, not the dead one. The reader pool is untouched (reads never block behind the
// lock, so they survive a demotion).
func (c *Client) NewLeaderSession(ctx context.Context) (LeaderLock, MetaWriteConn, error) {
	session, lock, writer, err := openLeaderSession(ctx, c.adminDSN)
	if err != nil {
		return nil, nil, err
	}
	c.mu.Lock()
	c.session = session
	c.mu.Unlock()
	return lock, writer, nil
}

// Lock returns the leader-election lock, held on the session-pinned connection.
func (c *Client) Lock() LeaderLock { return c.lock }

// WriteConn returns the leader's single meta write connection: the dispatcher wraps
// it in the one Writer, so every meta write rides the lock-holding session.
func (c *Client) WriteConn() MetaWriteConn { return c.writer }

// Reader returns the plain-MVCC meta reader (the pool), for read paths that must
// never block behind the single writer or the leader lock.
func (c *Client) Reader() Reader { return c.reader }

// RegistryReader returns the plain-MVCC registry reader (the pool): the pipelines
// and dependencies read seam the apply op rebuilds the dependency graph from.
func (c *Client) RegistryReader() RegistryReader { return c.registry }

// AppliedHeadReader returns the plain-MVCC applied-migration-head reader (the pool):
// the meta migrations read seam provisioning builds its per-table ledger view from.
func (c *Client) AppliedHeadReader() AppliedHeadReader { return c.ledger }

// PipelineLister returns the plain-MVCC pipeline-list reader (the pool): the iris
// pipeline list read seam (active-run default and --all every-registered views).
func (c *Client) PipelineLister() PipelineLister { return c.pipes }

// ManualReader returns the plain-MVCC manual-run reader (the pool): the pipeline run
// target, latest-run, run_inputs consumed, and lane-roster reads the manual `iris
// pipeline run` op composes.
func (c *Client) ManualReader() ManualReader { return c.manual }

// ShowReader returns the plain-MVCC pipeline-show reader (the pool): the
// declaration detail, role grants, runs, and gate-ledger input reads the `iris
// pipeline show` readout composes.
func (c *Client) ShowReader() ShowReader { return c.show }

// PromoteStateReader returns the plain-MVCC promote-gate reader (the pool): the
// registration/data-mode, built-state, and upstream-data-mode reads the promote
// op's gate and cross-mode warning are decided from.
func (c *Client) PromoteStateReader() PromoteStateReader { return c.promote }

// Close tears down the client: it closes the reader pool and the leader session. It
// is safe to call after the lock has already released the session, so the daemon can
// Release the lock then Close the client. The double-close guard checks IsClosed
// BEFORE closing: pgx marks a connection closed regardless of whether the terminate
// succeeded, so testing IsClosed after Close would always be true and would swallow
// a genuine close error.
func (c *Client) Close(ctx context.Context) error {
	if c.pool != nil {
		c.pool.Close()
	}
	// Read the current session under the guard: a fresh-session renewal may have
	// swapped it, and Close must tear down the live one, not a stale reference.
	c.mu.Lock()
	session := c.session
	c.mu.Unlock()
	if session != nil {
		if session.IsClosed() {
			return nil // already released by the lock; nothing to close.
		}
		if err := session.Close(ctx); err != nil {
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

// compile-time proof the leader session adapter satisfies the write and
// atomic-transaction seams: the same lock-holding session carries both.
var (
	_ MetaWriteConn = (*pgxWriteConn)(nil)
	_ MetaTxConn    = (*pgxWriteConn)(nil)
)

func (c *pgxWriteConn) Exec(ctx context.Context, sql string, args ...any) error {
	_, err := c.conn.Exec(ctx, sql, args...)
	return err
}

// ExecTx runs stmts as one atomic Postgres transaction on the leader's session: it
// opens a transaction, executes each statement in order, and commits. A deferred
// rollback on a background context guards every exit: on a statement error or a
// failed commit it sends ROLLBACK -- and it uses context.Background(), not the
// caller's ctx, so a cancelled apply still delivers the ROLLBACK wire message
// rather than short-circuiting and stranding the persistent leader connection in
// an aborted transaction (where every later command would fail). After a successful
// Commit the rollback is a no-op. So a failed registry apply leaves meta exactly as
// it was and the connection reusable. The whole batch rides the one lock-holding
// session, like every other meta write.
func (c *pgxWriteConn) ExecTx(ctx context.Context, stmts []Statement) error {
	tx, err := c.conn.Begin(ctx)
	if err != nil {
		return fmt.Errorf("store: begin registry transaction: %w", err)
	}
	defer func() { _ = tx.Rollback(context.Background()) }()

	for _, s := range stmts {
		if _, err := tx.Exec(ctx, s.SQL, s.Args...); err != nil {
			return fmt.Errorf("store: registry transaction exec: %w", err)
		}
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("store: commit registry transaction: %w", err)
	}
	return nil
}

// duplicateDatabaseCode is Postgres' SQLSTATE for duplicate_database: a CREATE
// DATABASE that lost a race to another candidate creating the same database first.
const duplicateDatabaseCode = "42P04"

// ensureMetaDatabase creates the dedicated meta database if it does not yet exist,
// on the admin/maintenance connection (the admin DSN as configured points at a
// connectable maintenance database, never at meta, which may not exist yet). The
// probe + create is idempotent and race-tolerant (see ensureMetaDatabaseOn).
func ensureMetaDatabase(ctx context.Context, adminDSN string) error {
	conn, err := pgx.Connect(ctx, adminDSN)
	if err != nil {
		return fmt.Errorf("store: open admin/maintenance connection: %w", err)
	}
	defer func() { _ = conn.Close(ctx) }()

	exists := func(ctx context.Context) (bool, error) {
		var one int
		qerr := conn.QueryRow(ctx, MetaExistsQuery).Scan(&one)
		if errors.Is(qerr, pgx.ErrNoRows) {
			return false, nil
		}
		if qerr != nil {
			return false, qerr
		}
		return true, nil
	}
	create := func(ctx context.Context) error {
		_, cerr := conn.Exec(ctx, CreateMetaDatabaseDDL())
		return cerr
	}
	return ensureMetaDatabaseOn(ctx, exists, create)
}

// ensureMetaDatabaseOn runs the create-if-missing logic over injected probe and
// create seams, so its race handling is provable with no live Postgres. It probes
// for meta and, if absent, creates it -- tolerating a concurrent create: another
// candidate creating meta between the probe and the CREATE makes CREATE DATABASE
// fail with duplicate_database (42P04), which is not an error but the goal already
// met (the database exists), so it is treated as success.
func ensureMetaDatabaseOn(ctx context.Context, exists func(context.Context) (bool, error), create func(context.Context) error) error {
	present, err := exists(ctx)
	if err != nil {
		return fmt.Errorf("store: probe meta database: %w", err)
	}
	if present {
		return nil
	}
	if err := create(ctx); err != nil {
		if isDuplicateDatabase(err) {
			return nil
		}
		return fmt.Errorf("store: create meta database: %w", err)
	}
	return nil
}

// isDuplicateDatabase reports whether err is Postgres' duplicate_database (42P04):
// a CREATE DATABASE lost the race to a concurrent candidate that created meta first.
func isDuplicateDatabase(err error) bool {
	var pgErr *pgconn.PgError
	return errors.As(err, &pgErr) && pgErr.Code == duplicateDatabaseCode
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
