package store

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"
)

// This file holds the live meta connections `iris engine install` bootstraps
// over, the pgx-backed seams the daemon composes into BootstrapEngine. The daemon
// never imports pgx, so the live probe / CREATE DATABASE / control-table
// connections live here, in store, and the daemon drives them through the
// store.Execer seam its install-sequence test proves with recording fakes. The
// recording fakes stand in at integration tier; these are exercised against a
// real cluster at conformance tier (the one tier with a live database), the
// single place the bootstrap DDL meets a real catalog.

// InstallConns are the live pgx connections the meta bootstrap of `iris engine
// install` rides: an admin/maintenance connection for the meta-existence probe and
// CREATE DATABASE meta, and a meta connection -- opened lazily on the first meta
// statement, since it can only be dialed once meta exists -- for the control-table
// DDL and the engine-key write. Build it with OpenInstallConns and release both
// connections with Close.
type InstallConns struct {
	adminDSN string
	admin    *pgx.Conn
	meta     *pgx.Conn // opened lazily on the first meta Exec, after CREATE DATABASE meta.
}

// OpenInstallConns opens the admin/maintenance connection the meta bootstrap probes
// and creates meta on, from the admin-derived connection source. The meta connection
// is not opened here: it can only be dialed after CREATE DATABASE meta has created
// the database, so it is opened lazily on the first meta statement. On error it opens
// nothing to leak.
func OpenInstallConns(ctx context.Context, src ConnSource) (*InstallConns, error) {
	if src == nil {
		return nil, errors.New("store: nil connection source")
	}
	dsn := src.ConnString()
	admin, err := pgx.Connect(ctx, dsn)
	if err != nil {
		return nil, fmt.Errorf("store: open admin/maintenance connection for install: %w", err)
	}
	return &InstallConns{adminDSN: dsn, admin: admin}, nil
}

// MetaExists reports whether the dedicated meta database already exists, probed on
// the admin/maintenance connection. It satisfies the daemon's meta-existence probe
// seam, so the bootstrap issues CREATE DATABASE meta only when it is missing (CREATE
// DATABASE has no IF NOT EXISTS).
func (c *InstallConns) MetaExists(ctx context.Context) (bool, error) {
	var one int
	err := c.admin.QueryRow(ctx, MetaExistsQuery).Scan(&one)
	if errors.Is(err, pgx.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("store: probe meta database: %w", err)
	}
	return true, nil
}

// Cluster returns the admin/maintenance Execer the bootstrap issues CREATE DATABASE
// meta on. It runs on the admin connection, never on meta (you cannot create meta
// from a connection to meta).
func (c *InstallConns) Cluster() Execer { return adminExec{conn: c.admin} }

// Meta returns the meta-connection Execer the bootstrap ensures the control tables
// and stores the engine key on. The connection is opened lazily on the first Exec,
// so it is dialed only after CREATE DATABASE meta has created the database.
func (c *InstallConns) Meta() Execer { return &lazyMetaExec{conns: c} }

// ReadEngineKey reads the raw ed25519 private key bytes back from the single-row
// engine_key meta table on the install meta connection, so `iris engine install`
// can report the stored key's public half (idempotent: a re-install that hit the
// ON CONFLICT no-op still reports the pre-existing key, never the discarded fresh
// mint). It opens the meta connection lazily if the caller has not issued a meta
// statement yet, exactly like the Meta Execer. The bytes are the private half: the
// caller decodes and exposes only the public half, and never logs them.
func (c *InstallConns) ReadEngineKey(ctx context.Context) ([]byte, error) {
	if c.meta == nil {
		cfg, err := metaConnConfig(c.adminDSN)
		if err != nil {
			return nil, err
		}
		conn, err := pgx.ConnectConfig(ctx, cfg)
		if err != nil {
			return nil, fmt.Errorf("store: open meta connection to read engine key: %w", err)
		}
		c.meta = conn
	}
	var priv []byte
	err := c.meta.QueryRow(ctx, selectEngineKeySQL).Scan(&priv)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("store: read engine key: %w", err)
	}
	return priv, nil
}

// Close releases the meta connection (if it was opened) and the admin connection,
// joining any close errors so neither is silently dropped.
func (c *InstallConns) Close(ctx context.Context) error {
	var errs []error
	if c.meta != nil {
		if err := c.meta.Close(ctx); err != nil {
			errs = append(errs, fmt.Errorf("store: close install meta connection: %w", err))
		}
		c.meta = nil
	}
	if c.admin != nil {
		if err := c.admin.Close(ctx); err != nil {
			errs = append(errs, fmt.Errorf("store: close install admin connection: %w", err))
		}
		c.admin = nil
	}
	return errors.Join(errs...)
}

// adminExec issues one statement on the admin/maintenance connection (CREATE
// DATABASE meta). The bootstrap wraps its error, so it returns the raw pgx error.
type adminExec struct{ conn *pgx.Conn }

func (a adminExec) Exec(ctx context.Context, sql string) error {
	_, err := a.conn.Exec(ctx, sql)
	return err
}

// lazyMetaExec issues meta statements (the control-table DDL and the engine-key
// write) on a meta connection it opens on first use, after CREATE DATABASE meta has
// created the database. Reusing the one connection across the bootstrap's meta
// statements keeps them on a single session; the bootstrap wraps its errors.
type lazyMetaExec struct{ conns *InstallConns }

func (l *lazyMetaExec) Exec(ctx context.Context, sql string) error {
	if l.conns.meta == nil {
		cfg, err := metaConnConfig(l.conns.adminDSN)
		if err != nil {
			return err
		}
		conn, err := pgx.ConnectConfig(ctx, cfg)
		if err != nil {
			return fmt.Errorf("store: open meta connection for install: %w", err)
		}
		l.conns.meta = conn
	}
	_, err := l.conns.meta.Exec(ctx, sql)
	return err
}
