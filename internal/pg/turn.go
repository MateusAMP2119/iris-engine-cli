package pg

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strconv"
	"strings"

	"github.com/jackc/pgx/v5"
)

// This file is the data-database half of the turn protocol (#206): the engine-owned
// feed position, the delta input feed, and the atomic turn commit. Under the turn
// protocol the pipeline process holds no database credentials -- the engine reads a
// pipeline's declared-read delta out of the journal and performs the pipeline's
// declared writes itself on the lane's warm connection, so a turn's output rows,
// their capture stamps, and the advanced feed position commit in ONE data-database
// transaction: any failure commits nothing, and partial writes are structurally
// impossible. The meta run record rides the single meta writer separately (two
// databases, never one transaction); the commit order -- run row first, data
// transaction second, terminal stamp last -- means a crash between the two leaves a
// running run for the next leader's reconciliation, never data without a record.

// TurnPositionsName is the engine-owned feed-position table: one row per pipeline,
// the highest journal id its input feed has consumed. It lives in the iris schema
// beside the capture function, where no pipeline role is granted table access.
const TurnPositionsName = CaptureSchema + ".turn_positions"

// turnPositionsDDL is the create-if-missing DDL for the feed-position table.
// position is the highest consumed journal id (0 = nothing consumed).
const turnPositionsDDL = `CREATE TABLE IF NOT EXISTS ` + TurnPositionsName + ` (
    pipeline text PRIMARY KEY,
    position bigint NOT NULL
);`

// EnsureTurnPositions ensures the iris schema and the engine-owned feed-position
// table exist. It is idempotent (create-if-missing) and self-contained, so every
// provisioning path and the leader's lane plane can call it safely; a dropped
// table self-heals at the next call.
func EnsureTurnPositions(ctx context.Context, db DB) error {
	for _, stmt := range []string{CaptureSchemaDDL(), turnPositionsDDL} {
		if err := db.Exec(ctx, stmt); err != nil {
			return fmt.Errorf("pg: ensure turn positions: %w", err)
		}
	}
	return nil
}

// TurnRead is one declared read a turn's input feed covers: the schema.table and
// the declared fields the fed rows are projected to (the engine reads with its own
// identity, so projection is what keeps undeclared fields from leaking).
type TurnRead struct {
	// Schema is the read's schema.
	Schema string
	// Table is the read's table.
	Table string
	// Fields are the declared read fields, the fed rows' projection.
	Fields []string
}

// FeedRow is one input row the feed produced: the dotted table it came from and
// the row's current values as a JSON object projected to the declared fields.
type FeedRow struct {
	// Table is the dotted schema.table the row belongs to.
	Table string
	// Row is the row's JSON object.
	Row json.RawMessage
}

// TurnFeed is one turn's input delta: the rows to feed, the feed position the
// turn will have consumed once it commits, and whether that position advanced
// past the stored one (a turn with no new journal entries feeds nothing and
// leaves the position untouched -- zero writes).
type TurnFeed struct {
	// Rows are the input rows, in journal order of each row key's newest entry.
	Rows []FeedRow
	// Position is the highest journal id this feed consumed.
	Position int64
	// Advanced reports whether Position moved past the stored position.
	Advanced bool
}

// feedBatchLimit bounds how many journal entries one turn's feed reads. A
// pipeline far behind the journal catches up across consecutive turns (the
// position advances to the last entry actually fed), so one turn never carries an
// unbounded input set.
const feedBatchLimit = 10000

// ReadTurnFeed reads the pipeline's input delta: every data-journal entry past
// the stored feed position that targets one of the declared reads, deduplicated
// to each row key's newest entry, resolved to the row's CURRENT values projected
// to the declared fields. A row whose current version is gone (deleted since) is
// skipped -- the feed carries rows, not history. With no declared reads it
// performs no reads at all and returns an empty, unadvanced feed.
func (c *Client) ReadTurnFeed(ctx context.Context, pipeline string, reads []TurnRead) (TurnFeed, error) {
	if len(reads) == 0 {
		return TurnFeed{}, nil
	}
	pos, err := c.turnPosition(ctx, pipeline)
	if err != nil {
		return TurnFeed{}, err
	}

	entries, err := c.feedEntries(ctx, pos, reads)
	if err != nil {
		return TurnFeed{}, err
	}
	if len(entries) == 0 {
		return TurnFeed{Position: pos}, nil
	}

	// Deduplicate to each row key's newest entry, preserving journal order of
	// that newest entry.
	type keyed struct {
		schema, table, rowPK string
	}
	newest := map[keyed]int{}
	var order []keyed
	for _, e := range entries {
		k := keyed{e.schema, e.table, e.rowPK}
		if _, seen := newest[k]; !seen {
			order = append(order, k)
		}
		newest[k] = len(order) - 1
	}
	sort.SliceStable(order, func(i, j int) bool { return newest[order[i]] < newest[order[j]] })

	fieldsByTable := map[string][]string{}
	for _, r := range reads {
		fieldsByTable[r.Schema+"."+r.Table] = r.Fields
	}

	feed := TurnFeed{Position: entries[len(entries)-1].id, Advanced: true}
	for _, k := range order {
		row, ok, ferr := c.fetchCurrentRow(ctx, k.schema, k.table, k.rowPK, fieldsByTable[k.schema+"."+k.table])
		if ferr != nil {
			return TurnFeed{}, ferr
		}
		if !ok {
			continue // deleted since its journal entry: nothing current to feed
		}
		feed.Rows = append(feed.Rows, FeedRow{Table: k.schema + "." + k.table, Row: row})
	}
	return feed, nil
}

// feedEntry is one journal entry the feed considers.
type feedEntry struct {
	id                   int64
	schema, table, rowPK string
}

// feedEntries reads the journal delta past pos for the declared read tables, in
// id order, bounded by feedBatchLimit.
func (c *Client) feedEntries(ctx context.Context, pos int64, reads []TurnRead) ([]feedEntry, error) {
	var conds []string
	args := []any{pos}
	for _, r := range reads {
		conds = append(conds, fmt.Sprintf(`("schema" = $%d AND "table" = $%d)`, len(args)+1, len(args)+2))
		args = append(args, r.Schema, r.Table)
	}
	q := fmt.Sprintf(`SELECT id, "schema", "table", row_pk FROM public.data_journal
WHERE id > $1 AND (%s) ORDER BY id LIMIT %d`, strings.Join(conds, " OR "), feedBatchLimit)
	rows, err := c.pool.Query(ctx, q, args...)
	if err != nil {
		return nil, fmt.Errorf("pg: read turn feed delta: %w", err)
	}
	defer rows.Close()
	var out []feedEntry
	for rows.Next() {
		var e feedEntry
		if err := rows.Scan(&e.id, &e.schema, &e.table, &e.rowPK); err != nil {
			return nil, fmt.Errorf("pg: scan turn feed entry: %w", err)
		}
		out = append(out, e)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("pg: iterate turn feed delta: %w", err)
	}
	return out, nil
}

// turnPosition reads the pipeline's stored feed position (0 when absent).
func (c *Client) turnPosition(ctx context.Context, pipeline string) (int64, error) {
	var pos int64
	err := c.pool.QueryRow(ctx, `SELECT position FROM `+TurnPositionsName+` WHERE pipeline = $1`, pipeline).Scan(&pos)
	if errors.Is(err, pgx.ErrNoRows) {
		return 0, nil
	}
	if err != nil {
		return 0, fmt.Errorf("pg: read turn position for %q: %w", pipeline, err)
	}
	return pos, nil
}

// primaryKey resolves a table's primary-key columns and their rendered types, in
// key order, mirroring the capture function's row_pk resolution so a journal
// row_pk splits back onto the same columns.
func (c *Client) primaryKey(ctx context.Context, schema, table string) ([]pkColumn, error) {
	rows, err := c.pool.Query(ctx, `
SELECT a.attname, format_type(a.atttypid, a.atttypmod)
FROM pg_catalog.pg_index i
CROSS JOIN LATERAL unnest(i.indkey::int2[]) WITH ORDINALITY AS k(attnum, ord)
JOIN pg_catalog.pg_attribute a ON a.attrelid = i.indrelid AND a.attnum = k.attnum
WHERE i.indrelid = $1::regclass AND i.indisprimary
ORDER BY k.ord`, quoteIdentifier(schema)+"."+quoteIdentifier(table))
	if err != nil {
		return nil, fmt.Errorf("pg: resolve primary key of %s.%s: %w", schema, table, err)
	}
	defer rows.Close()
	var pk []pkColumn
	for rows.Next() {
		var col pkColumn
		if err := rows.Scan(&col.name, &col.typ); err != nil {
			return nil, fmt.Errorf("pg: scan primary key of %s.%s: %w", schema, table, err)
		}
		pk = append(pk, col)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("pg: iterate primary key of %s.%s: %w", schema, table, err)
	}
	if len(pk) == 0 {
		return nil, fmt.Errorf("pg: table %s.%s has no primary key", schema, table)
	}
	return pk, nil
}

// pkColumn is one primary-key column: its name and rendered type (the cast the
// text row_pk parts are resolved through).
type pkColumn struct {
	name, typ string
}

// fetchCurrentRow reads one row's current values by its journal row_pk, projected
// to the declared fields, as a JSON object. It reports false when the row no
// longer exists or the row_pk does not split onto the primary key (a composite
// text key containing the separator is ambiguous by capture's own encoding and is
// skipped rather than misread).
func (c *Client) fetchCurrentRow(ctx context.Context, schema, table, rowPK string, fields []string) (json.RawMessage, bool, error) {
	if len(fields) == 0 {
		return nil, false, nil
	}
	pk, err := c.primaryKey(ctx, schema, table)
	if err != nil {
		return nil, false, err
	}
	parts := strings.Split(rowPK, "|")
	if len(parts) != len(pk) {
		return nil, false, nil
	}
	cols := make([]string, len(fields))
	for i, f := range fields {
		cols[i] = quoteIdentifier(f)
	}
	var conds []string
	args := make([]any, len(pk))
	for i, col := range pk {
		conds = append(conds, fmt.Sprintf("%s = $%d::%s", quoteIdentifier(col.name), i+1, col.typ))
		args[i] = parts[i]
	}
	q := fmt.Sprintf(`SELECT row_to_json(x) FROM (SELECT %s FROM %s.%s WHERE %s) x`,
		strings.Join(cols, ", "), quoteIdentifier(schema), quoteIdentifier(table), strings.Join(conds, " AND "))
	var row json.RawMessage
	err = c.pool.QueryRow(ctx, q, args...).Scan(&row)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, false, nil
	}
	if err != nil {
		return nil, false, fmt.Errorf("pg: fetch current row %s.%s %s: %w", schema, table, rowPK, err)
	}
	return row, true, nil
}

// TurnWrite is one output row a turn commits: the dotted schema.table and the row
// object (already validated against the declared writes by the collector).
type TurnWrite struct {
	// Schema is the target schema.
	Schema string
	// Table is the target table.
	Table string
	// Row is the row's JSON object, verbatim from the pipeline's frame.
	Row json.RawMessage
}

// TurnCommit is the atomic data-database half of a turn's commit: the output rows
// to write attributed to RunID, and the feed position to advance. Rows are
// upserted on the target's primary key (a re-produced key updates the row), and
// every write fires the capture trigger inside the same transaction, so the
// journal stamps commit with the rows they describe.
type TurnCommit struct {
	// Pipeline is the committing pipeline (its feed-position row).
	Pipeline string
	// RunID is the minted run the writes are attributed to (the iris.run_id GUC).
	RunID int64
	// Writes are the turn's output rows, in arrival order.
	Writes []TurnWrite
	// Position is the feed position the turn consumed.
	Position int64
	// AdvancePosition reports whether Position should be persisted.
	AdvancePosition bool
}

// TurnStamps are the snapshot-pin values a producing turn's data transaction
// read: the LSN and the journal window delimiting exactly the turn's own stamps.
type TurnStamps struct {
	// SnapshotLSN is the data database's LSN inside the commit transaction.
	SnapshotLSN string
	// JournalFloor is the journal high id before the turn's writes.
	JournalFloor int64
	// JournalCeiling is the journal high id after the turn's writes.
	JournalCeiling int64
}

// CommitTurn commits one turn's data-database effects in a single transaction:
// the run-id attribution GUC (SET LOCAL, so the capture trigger stamps every row
// to the minted run), the upserted output rows with their journal stamps, the
// snapshot-pin reads, and the advanced feed position. Any failure rolls the whole
// turn back -- rows, stamps, and position together. A commit with no writes and
// no position advance is refused (the caller skips the transaction entirely; a
// quiet turn costs nothing).
func (c *Client) CommitTurn(ctx context.Context, tc TurnCommit) (TurnStamps, error) {
	if len(tc.Writes) == 0 && !tc.AdvancePosition {
		return TurnStamps{}, errors.New("pg: commit turn: nothing to commit")
	}
	tx, err := c.pool.Begin(ctx)
	if err != nil {
		return TurnStamps{}, fmt.Errorf("pg: begin turn commit: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }() // safe no-op after commit

	var stamps TurnStamps
	if len(tc.Writes) > 0 {
		// Attribution rides the transaction, never the connection: SET LOCAL scopes
		// the GUC to this commit, so the lane's warm connection carries no residue.
		if _, err := tx.Exec(ctx, `SELECT set_config('`+RunIDSetting+`', $1, true)`, strconv.FormatInt(tc.RunID, 10)); err != nil {
			return TurnStamps{}, fmt.Errorf("pg: set turn run id: %w", err)
		}
		if err := tx.QueryRow(ctx, `SELECT COALESCE(max(id), 0), pg_current_wal_lsn()::text FROM public.data_journal`).Scan(&stamps.JournalFloor, &stamps.SnapshotLSN); err != nil {
			return TurnStamps{}, fmt.Errorf("pg: read turn journal floor: %w", err)
		}
		for _, w := range tc.Writes {
			if err := c.upsertTurnRow(ctx, tx, w); err != nil {
				return TurnStamps{}, err
			}
		}
		if err := tx.QueryRow(ctx, `SELECT COALESCE(max(id), 0) FROM public.data_journal`).Scan(&stamps.JournalCeiling); err != nil {
			return TurnStamps{}, fmt.Errorf("pg: read turn journal ceiling: %w", err)
		}
	}
	if tc.AdvancePosition {
		if _, err := tx.Exec(ctx, `INSERT INTO `+TurnPositionsName+` AS tp (pipeline, position) VALUES ($1, $2)
ON CONFLICT (pipeline) DO UPDATE SET position = GREATEST(tp.position, EXCLUDED.position)`, tc.Pipeline, tc.Position); err != nil {
			return TurnStamps{}, fmt.Errorf("pg: advance turn position for %q: %w", tc.Pipeline, err)
		}
	}
	if err := tx.Commit(ctx); err != nil {
		return TurnStamps{}, fmt.Errorf("pg: commit turn: %w", err)
	}
	return stamps, nil
}

// upsertTurnRow writes one output row: an INSERT over json_populate_record (so
// Postgres casts each JSON value to its column's type) upserted on the table's
// primary key -- a re-produced key updates the non-key fields, all-key rows
// insert-or-skip. The capture trigger fires inside the caller's transaction.
func (c *Client) upsertTurnRow(ctx context.Context, tx pgx.Tx, w TurnWrite) error {
	var m map[string]json.RawMessage
	if err := json.Unmarshal(w.Row, &m); err != nil {
		return fmt.Errorf("pg: turn row for %s.%s is not a JSON object: %w", w.Schema, w.Table, err)
	}
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)

	pk, err := c.primaryKey(ctx, w.Schema, w.Table)
	if err != nil {
		return err
	}
	pkSet := map[string]bool{}
	var pkCols []string
	for _, col := range pk {
		pkSet[col.name] = true
		pkCols = append(pkCols, quoteIdentifier(col.name))
	}

	cols := make([]string, len(keys))
	for i, k := range keys {
		cols[i] = quoteIdentifier(k)
	}
	var sets []string
	for _, k := range keys {
		if !pkSet[k] {
			sets = append(sets, quoteIdentifier(k)+" = EXCLUDED."+quoteIdentifier(k))
		}
	}
	conflict := "DO NOTHING"
	if len(sets) > 0 {
		conflict = "DO UPDATE SET " + strings.Join(sets, ", ")
	}
	q := fmt.Sprintf(`INSERT INTO %s.%s (%s) SELECT %s FROM json_populate_record(NULL::%s.%s, $1::json)
ON CONFLICT (%s) %s`,
		quoteIdentifier(w.Schema), quoteIdentifier(w.Table), strings.Join(cols, ", "), strings.Join(cols, ", "),
		quoteIdentifier(w.Schema), quoteIdentifier(w.Table), strings.Join(pkCols, ", "), conflict)
	if _, err := tx.Exec(ctx, q, string(w.Row)); err != nil {
		return fmt.Errorf("pg: write turn row into %s.%s: %w", w.Schema, w.Table, err)
	}
	return nil
}
