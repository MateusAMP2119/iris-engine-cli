package pg

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// This file is the live data-database client: the pgx-backed implementation of the
// pg seam (the recording fake in pgtest stands in for it at integration tier). It
// owns one connection pool on the data database -- the database the daemon's admin
// DSN points at, where the declared schemas/ tables and the public.data_journal
// live, distinct from the meta control database store owns. It is the one place pg
// turns the admin-derived connection source into real connections.
//
// The client serves two provisioning needs (specification section 5): it Execs the
// CREATE / ALTER / trigger DDL the provisioner emits, and it reads the live-Postgres
// view the provisioner diffs the declared world against so a re-apply is a no-op. It
// is exercised against a real Postgres at conformance tier (the one tier with a live
// database), the single place the live-view reads and the generated DDL meet a real
// catalog.

// LiveViewReader reads the data database's current physical state as provisioning
// needs it. *Client is the production implementation; a fake satisfies it in tests
// that drive the apply orchestration with no live Postgres.
type LiveViewReader interface {
	// ReadLiveView returns the live-Postgres view: which schemas and tables exist,
	// which tables carry the full capture-trigger set, and whether the partitioned
	// journal exists.
	ReadLiveView(ctx context.Context) (LiveView, error)
}

// Client is the live data-database client: one pgx pool on the data database, from
// which every provisioning DDL and live-view read is issued. Build it with Connect
// and tear it down with Close.
type Client struct {
	pool *pgxpool.Pool
}

// compile-time proof the live client satisfies the DDL and live-view seams.
var (
	_ DB             = (*Client)(nil)
	_ LiveViewReader = (*Client)(nil)
)

// Connect opens the data-database client from the admin-derived connection source. It
// mirrors how store opens meta: it ensures the dedicated data database exists (CREATE
// DATABASE if missing, on the admin/maintenance connection, race-tolerant), then opens
// a pgx pool on that data database -- never on the admin DSN's own database, which in
// external mode the cluster superuser owns and the engine's non-superuser admin cannot
// provision into. The engine-created data database is admin-owned, so provisioning's
// CREATE SCHEMA/TABLE succeeds. On error it opens nothing to leak.
func Connect(ctx context.Context, src ConnSource) (*Client, error) {
	if src == nil {
		return nil, errors.New("pg: nil connection source")
	}
	adminDSN := src.ConnString()
	if err := ensureDataDatabase(ctx, adminDSN); err != nil {
		return nil, err
	}
	cfg, err := pgxpool.ParseConfig(adminDSN)
	if err != nil {
		return nil, fmt.Errorf("pg: parse data-database DSN: %w", err)
	}
	// Point the pool at the engine-owned data database, not the admin DSN's own
	// (maintenance) database.
	cfg.ConnConfig.Database = DataDatabase
	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		return nil, fmt.Errorf("pg: open data-database pool: %w", err)
	}
	return &Client{pool: pool}, nil
}

// ensureDataDatabase creates the dedicated data database if it does not yet exist, on
// the admin/maintenance connection (the admin DSN points at a connectable maintenance
// database, e.g. the cluster default, never at data, which may not exist yet). The
// admin's CREATEDB right makes the admin the owner, so provisioning can create schemas
// in it. The probe + create is idempotent and race-tolerant (ensureDataDatabaseOn),
// mirroring store.ensureMetaDatabase.
func ensureDataDatabase(ctx context.Context, adminDSN string) error {
	conn, err := pgx.Connect(ctx, adminDSN)
	if err != nil {
		return fmt.Errorf("pg: open admin/maintenance connection: %w", err)
	}
	defer func() { _ = conn.Close(ctx) }()

	exists := func(ctx context.Context) (bool, error) {
		var one int
		qerr := conn.QueryRow(ctx, DataExistsQuery, DataDatabase).Scan(&one)
		if errors.Is(qerr, pgx.ErrNoRows) {
			return false, nil
		}
		if qerr != nil {
			return false, qerr
		}
		return true, nil
	}
	create := func(ctx context.Context) error {
		_, cerr := conn.Exec(ctx, CreateDataDatabaseDDL())
		return cerr
	}
	return ensureDataDatabaseOn(ctx, exists, create)
}

// Exec issues one DDL statement against the data database through the pool. It
// satisfies the DB seam, so the provisioner's CREATE / ALTER / trigger stream runs
// against the live database exactly as it runs against the recording fake.
func (c *Client) Exec(ctx context.Context, sql string) error {
	if _, err := c.pool.Exec(ctx, sql); err != nil {
		return fmt.Errorf("pg: exec data-database statement: %w", err)
	}
	return nil
}

// Close releases the pool. It is safe to call once at teardown.
func (c *Client) Close() {
	if c.pool != nil {
		c.pool.Close()
	}
}

// The live-view read statements. Each is a plain MVCC catalog read, so building the
// view never contends with a concurrent DDL apply.
const (
	// selectSchemasSQL reads every existing schema name (user and system alike; the
	// provisioner only consults the declared ones).
	selectSchemasSQL = `SELECT schema_name FROM information_schema.schemata`

	// selectTablesSQL reads every existing base table as schema.table (views and the
	// like excluded: provisioning materializes base tables).
	selectTablesSQL = `SELECT table_schema, table_name
FROM information_schema.tables
WHERE table_type = 'BASE TABLE'`

	// selectCaptureTriggersSQL counts the engine capture triggers installed on each
	// table (the three per-operation iris_capture_* bindings). A table with the full
	// set installed needs no trigger DDL on a re-apply.
	selectCaptureTriggersSQL = `SELECT n.nspname, c.relname, count(*)
FROM pg_trigger t
JOIN pg_class c ON c.oid = t.tgrelid
JOIN pg_namespace n ON n.oid = c.relnamespace
WHERE NOT t.tgisinternal AND t.tgname LIKE 'iris_capture\_%'
GROUP BY n.nspname, c.relname`

	// selectJournalSQL reports whether the partitioned public.data_journal exists.
	selectJournalSQL = `SELECT EXISTS (
    SELECT 1 FROM pg_class c
    JOIN pg_namespace n ON n.oid = c.relnamespace
    WHERE n.nspname = 'public' AND c.relname = 'data_journal'
)`

	// captureTriggerFullSet is the number of per-operation capture triggers a fully
	// provisioned table carries (INSERT, UPDATE, DELETE). A table reporting fewer is
	// treated as lacking the set, so provisioning re-installs it.
	captureTriggerFullSet = 3
)

// ReadLiveView reads the data database's current physical state into a LiveView the
// provisioner diffs the declared world against (specification section 5). It reads
// the existing schemas, base tables, per-table capture-trigger counts, and whether
// the partitioned journal exists -- four plain MVCC catalog reads -- so a re-plan
// against an already-provisioned database is empty (idempotency).
func (c *Client) ReadLiveView(ctx context.Context) (LiveView, error) {
	view := LiveView{
		Schemas:         map[string]bool{},
		Tables:          map[string]bool{},
		CaptureTriggers: map[string]bool{},
	}

	if err := c.scan(ctx, selectSchemasSQL, func(scan func(...any) error) error {
		var name string
		if err := scan(&name); err != nil {
			return err
		}
		view.Schemas[name] = true
		return nil
	}); err != nil {
		return LiveView{}, fmt.Errorf("pg: read live schemas: %w", err)
	}

	if err := c.scan(ctx, selectTablesSQL, func(scan func(...any) error) error {
		var schema, table string
		if err := scan(&schema, &table); err != nil {
			return err
		}
		view.Tables[schema+"."+table] = true
		return nil
	}); err != nil {
		return LiveView{}, fmt.Errorf("pg: read live tables: %w", err)
	}

	if err := c.scan(ctx, selectCaptureTriggersSQL, func(scan func(...any) error) error {
		var schema, table string
		var count int
		if err := scan(&schema, &table, &count); err != nil {
			return err
		}
		if count >= captureTriggerFullSet {
			view.CaptureTriggers[schema+"."+table] = true
		}
		return nil
	}); err != nil {
		return LiveView{}, fmt.Errorf("pg: read live capture triggers: %w", err)
	}

	if err := c.pool.QueryRow(ctx, selectJournalSQL).Scan(&view.HasJournal); err != nil {
		return LiveView{}, fmt.Errorf("pg: read live journal presence: %w", err)
	}
	return view, nil
}

// scan runs sql and applies onRow to each row, closing the rows and surfacing any
// iteration error. onRow receives the row's Scan so the caller binds its own
// destinations; a plain MVCC read, never retried.
func (c *Client) scan(ctx context.Context, sql string, onRow func(scan func(...any) error) error) error {
	rows, err := c.pool.Query(ctx, sql)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		if err := onRow(rows.Scan); err != nil {
			return err
		}
	}
	return rows.Err()
}

// EnsureCaptureFunction ensures the iris schema and the real iris.capture() trigger
// function exist so the provisioner's capture triggers can bind. The capture
// triggers the provisioner installs on every declared table bind to iris.capture(),
// the engine-owned PL/pgSQL function whose body reads the statement's transition
// tables and writes provenance rows into public.data_journal (capture.go owns the
// body). Provisioning cannot install a trigger that binds to a missing function, so
// this ensures the schema and the function first; it is create-if-missing /
// create-or-replace, so a dropped function self-heals and it is idempotent and safe
// to run before every provisioning apply.
func (c *Client) EnsureCaptureFunction(ctx context.Context) error {
	for _, stmt := range []string{CaptureSchemaDDL(), CaptureFunctionDDL()} {
		if _, err := c.pool.Exec(ctx, stmt); err != nil {
			return fmt.Errorf("pg: ensure capture function: %w", err)
		}
	}
	return nil
}
