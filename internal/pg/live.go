package pg

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"strconv"
	"strings"

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
// The client serves two provisioning needs: it Execs the CREATE / ALTER / trigger DDL
// the provisioner emits, and it reads the live-Postgres
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

// PrepareVerify prepare-verifies a derived endpoint statement against the data
// database: endpoint apply prepare-verifies the derived SQL. It checks one connection
// out of the data pool and issues a server-side
// Parse (Postgres itself vets the statement -- source present, columns real, types
// castable), then deallocates it so a pooled reuse can re-prepare, returning
// Postgres's refusal verbatim on failure. It runs as the engine's own data role, so
// it verifies statement validity only, never a per-caller grant (grants are enforced
// at read time, per role). name is prefixed and hashed so a re-verify never collides
// with a prior prepared name on a reused session, satisfying dispatch.PrepareVerifier.
func (c *Client) PrepareVerify(ctx context.Context, name, sql string) error {
	conn, err := c.pool.Acquire(ctx)
	if err != nil {
		return fmt.Errorf("pg: prepare-verify %q: acquire connection: %w", name, err)
	}
	defer conn.Release()

	sum := sha256.Sum256([]byte(sql))
	stmtName := "iris_epverify_" + name + "_" + hex.EncodeToString(sum[:4])
	if _, err := conn.Conn().Prepare(ctx, stmtName, sql); err != nil {
		// Postgres refused the statement: return its message verbatim (the applier
		// wraps it), so a bad endpoint never publishes.
		return fmt.Errorf("pg: prepare-verify %q against the data database: %w", name, err)
	}
	// Deallocate so the pooled session can re-prepare on a later verify with the same
	// name; a deallocate failure is non-fatal (the statement is session-scoped and the
	// verification already succeeded).
	_ = conn.Conn().Deallocate(ctx, stmtName)
	return nil
}

// Close releases the pool. It is safe to call once at teardown.
func (c *Client) Close() {
	if c.pool != nil {
		c.pool.Close()
	}
}

// JournalHighID returns the current high id of public.data_journal (max(id) or 0
// if empty). It satisfies dispatch.JournalHighWatermark for snapshot pin stamping.
func (c *Client) JournalHighID(ctx context.Context) (int64, error) {
	var id int64
	if err := c.pool.QueryRow(ctx, `SELECT COALESCE(max(id), 0) FROM public.data_journal`).Scan(&id); err != nil {
		return 0, fmt.Errorf("pg: read journal high id: %w", err)
	}
	return id, nil
}

// ResidentJournalStats reports the resident (unsealed) journal partition's current
// row count and its inclusive id span (min and max id), all zero when the journal
// is empty. The seal step reads it to decide whether the live partition has crossed
// the journal_partition_rows threshold and, when it seals, to stamp the checkpoint's
// exact id_from/id_to from the partition's actual entries. It is one plain-MVCC read;
// the count is over the resident tail only, since
// sealed partitions are exported and dropped.
func (c *Client) ResidentJournalStats(ctx context.Context) (count, minID, maxID int64, err error) {
	if err := c.pool.QueryRow(ctx,
		`SELECT count(*), COALESCE(min(id), 0), COALESCE(max(id), 0) FROM public.data_journal`,
	).Scan(&count, &minID, &maxID); err != nil {
		return 0, 0, 0, fmt.Errorf("pg: read resident journal stats: %w", err)
	}
	return count, minID, maxID, nil
}

// ResidentRunIDs returns the distinct run ids that have written entries into the
// resident (unsealed) journal partition. The seal step reads it to identify which
// runs have written into the partition, so it can check whether any of them is still
// in flight before it cuts a seal (a partition seals only when every in-flight run
// writing into it has finished). A run that writes nothing
// -- an idle-lane no-op pass -- never appears here, so it never blocks a seal. It is
// one plain-MVCC read over the resident tail.
func (c *Client) ResidentRunIDs(ctx context.Context) ([]int64, error) {
	rows, err := c.pool.Query(ctx, `SELECT DISTINCT run_id FROM public.data_journal`)
	if err != nil {
		return nil, fmt.Errorf("pg: read resident journal run ids: %w", err)
	}
	defer rows.Close()
	var ids []int64
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			return nil, fmt.Errorf("pg: scan resident journal run id: %w", err)
		}
		ids = append(ids, id)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("pg: iterate resident journal run ids: %w", err)
	}
	return ids, nil
}

// OpenUndoRunIDs returns the run id of every resident journal entry still undo =
// open (written under disposable data_mode, unreleased by promotion), one element
// per entry so the count is the wipe scope's size once attributed. The
// destructive-op gate reads it to evaluate the un-promoted-data soft-block on a
// teardown: the caller attributes each run id to its pipeline (meta-side) and
// counts the entries under the teardown's scope. It is one plain-MVCC read.
func (c *Client) OpenUndoRunIDs(ctx context.Context) ([]int64, error) {
	rows, err := c.pool.Query(ctx, `SELECT run_id FROM public.data_journal WHERE undo = 'open'`)
	if err != nil {
		return nil, fmt.Errorf("pg: read open-undo journal run ids: %w", err)
	}
	defer rows.Close()
	var ids []int64
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			return nil, fmt.Errorf("pg: scan open-undo journal run id: %w", err)
		}
		ids = append(ids, id)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("pg: iterate open-undo journal run ids: %w", err)
	}
	return ids, nil
}

// CurrentLSN returns the data database's current WAL LSN in text form. It satisfies
// dispatch.LSNReader for snapshot pin stamping.
func (c *Client) CurrentLSN(ctx context.Context) (string, error) {
	var lsn string
	if err := c.pool.QueryRow(ctx, `SELECT pg_current_wal_lsn()::text`).Scan(&lsn); err != nil {
		return "", fmt.Errorf("pg: read current lsn: %w", err)
	}
	return lsn, nil
}

// Query for archive seal row reads.
func (c *Client) Query(ctx context.Context, sql string, args ...any) (interface {
	Next() bool
	Scan(dest ...any) error
	Close()
	Err() error
}, error) {
	return c.pool.Query(ctx, sql, args...)
}

// CompactJournalRange nulls released pre-images and folds dups (conformance helper).
// Performed in one transaction so compaction is atomic (multi-statement state change).
func (c *Client) CompactJournalRange(ctx context.Context, from, to int64) error {
	if from <= 0 {
		from = 0
	}
	toExpr := "9223372036854775807"
	if to > 0 {
		toExpr = fmt.Sprintf("%d", to)
	}
	tx, err := c.pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("pg: begin compact tx: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }() // safe no-op after commit

	// Null all pre in the sealed range (past undo for sealed history) and fold dups.
	if _, err := tx.Exec(ctx, fmt.Sprintf(`UPDATE public.data_journal SET pre_image=NULL WHERE id>=%d AND id<%s`, from, toExpr)); err != nil {
		return fmt.Errorf("pg: compact null preimages: %w", err)
	}
	if _, err := tx.Exec(ctx, fmt.Sprintf(`DELETE FROM public.data_journal j USING (SELECT "schema","table",row_pk,run_id,max(id) k FROM public.data_journal WHERE id>=%d AND id<%s GROUP BY "schema","table",row_pk,run_id) kx WHERE j."schema"=kx."schema" AND j."table"=kx."table" AND j.row_pk=kx.row_pk AND j.run_id=kx.run_id AND j.id>=%d AND j.id<%s AND j.id<>kx.k`, from, toExpr, from, toExpr)); err != nil {
		return fmt.Errorf("pg: compact fold dups: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("pg: commit compact: %w", err)
	}
	return nil
}

// QueryCompactedRows returns a canonical serialization of rows in [from,to) for
// checkpoint digest computation (id order). Used by archive seal.
func (c *Client) QueryCompactedRows(ctx context.Context, from, to int64) ([][]byte, error) {
	q := `SELECT id, pg_role, run_id, "schema", "table", row_pk, op, COALESCE(pre_image::text, ''), undo, recorded_at
FROM public.data_journal WHERE id >= $1 AND ($2 = 0 OR id < $2) ORDER BY id`
	rows, err := c.pool.Query(ctx, q, from, to)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out [][]byte
	for rows.Next() {
		var id int64
		var role, schema, table, pk, op, pre, undo, rec string
		var rn int64
		if err := rows.Scan(&id, &role, &rn, &schema, &table, &pk, &op, &pre, &undo, &rec); err != nil {
			return nil, err
		}
		b := []byte(fmt.Sprintf("%d|%s|%d|%s|%s|%s|%s|%s|%s|%s", id, role, rn, schema, table, pk, op, pre, undo, rec))
		out = append(out, b)
	}
	return out, rows.Err()
}

// ParseCompactedRow decodes one canonical compacted-row serialization -- the
// exact shape QueryCompactedRows produces and the archive export persists
// (id|pg_role|run_id|schema|table|row_pk|op|pre_image|undo|recorded_at) -- back
// into a JournalEntry, so stamps recovered from an archived partition feed the
// same provenance walk resident stamps do. The pre_image field is JSON and may
// itself contain the delimiter, so the head fields split from the LEFT and the
// undo/recorded_at tail from the RIGHT, leaving everything between as the
// pre-image. It reports false for bytes that do not parse (a foreign or
// corrupted row is skipped, never misread). pg_role and recorded_at are not part
// of the in-memory entry and are dropped.
func ParseCompactedRow(b []byte) (JournalEntry, bool) {
	head := strings.SplitN(string(b), "|", 8)
	if len(head) != 8 {
		return JournalEntry{}, false
	}
	id, err := strconv.ParseInt(head[0], 10, 64)
	if err != nil {
		return JournalEntry{}, false
	}
	runID, err := strconv.ParseInt(head[2], 10, 64)
	if err != nil {
		return JournalEntry{}, false
	}
	rest := head[7] // pre_image|undo|recorded_at, pre_image possibly containing '|'
	i := strings.LastIndexByte(rest, '|')
	if i < 0 {
		return JournalEntry{}, false
	}
	j := strings.LastIndexByte(rest[:i], '|')
	if j < 0 {
		return JournalEntry{}, false
	}
	return JournalEntry{
		ID:       id,
		RunID:    runID,
		Schema:   head[3],
		Table:    head[4],
		RowPK:    head[5],
		Op:       WriteOp(head[6]),
		PreImage: rest[:j],
		Undo:     UndoState(rest[j+1 : i]),
	}, true
}

// DropPartitionForRange detaches and drops the bootstrap p0 partition (the
// sealed range, whose rows are already exported under their checkpoint digest)
// and immediately recreates an empty p0 spanning the full range so the journal
// stays writable for subsequent captures. Leaving the table partition-less would
// make the next journal insert fail, so the recreate is part of the same seal
// step. Best-effort: the export + checkpoint hold the sealed history.
func (c *Client) DropPartitionForRange(ctx context.Context, _ int64) error {
	_, _ = c.pool.Exec(ctx, `ALTER TABLE IF EXISTS public.data_journal DETACH PARTITION IF EXISTS public.data_journal_p0;`)
	_, _ = c.pool.Exec(ctx, `DROP TABLE IF EXISTS public.data_journal_p0;`)
	_, _ = c.pool.Exec(ctx, `CREATE TABLE IF NOT EXISTS public.data_journal_p0 PARTITION OF public.data_journal FOR VALUES FROM (MINVALUE) TO (MAXVALUE);`)
	return nil
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
// provisioner diffs the declared world against. It reads the existing schemas, base
// tables, per-table capture-trigger counts, and whether
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

// Stamps reads the data_journal stamps for one provenance key (schema.table + row_pk).
// It returns entries in id order (WalkProvenance sorts newest-first). Pre-image is
// not loaded: provenance is lineage only.
func (c *Client) Stamps(ctx context.Context, key RowKey) ([]JournalEntry, error) {
	if c.pool == nil {
		return nil, errors.New("pg: closed")
	}
	rows, err := c.pool.Query(ctx, `
		SELECT id, run_id, "schema", "table", row_pk, op, undo
		FROM public.data_journal
		WHERE "schema" = $1 AND "table" = $2 AND row_pk = $3
		ORDER BY id
	`, key.Schema, key.Table, key.RowPK)
	if err != nil {
		return nil, fmt.Errorf("pg: query stamps %s.%s %s: %w", key.Schema, key.Table, key.RowPK, err)
	}
	defer rows.Close()

	var out []JournalEntry
	for rows.Next() {
		var e JournalEntry
		var opStr, undoStr string
		if err := rows.Scan(&e.ID, &e.RunID, &e.Schema, &e.Table, &e.RowPK, &opStr, &undoStr); err != nil {
			return nil, fmt.Errorf("pg: scan stamp: %w", err)
		}
		e.Op = WriteOp(opStr)
		e.Undo = UndoState(undoStr)
		out = append(out, e)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("pg: iterate stamps: %w", err)
	}
	return out, nil
}
