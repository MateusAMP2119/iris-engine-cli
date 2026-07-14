package pg

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/jackc/pgx/v5"
)

// This file is the live half of `iris workload wipe [<pipeline>]`: the executor
// that takes the pure wipe plan (wipe.go's
// PlanWipe, the decided outcome) and applies it to the real data database in ONE
// transaction, so a mid-wipe failure leaves no partial wipe (journal and tables
// co-reside). It is to wipe what ExecutePromotionFlip is to promotion: the live
// interpreter of an already-decided plan, kept apart from the pure model so the
// algorithm stays unit-testable with no database and the executor stays a dumb
// applier.
//
// The one transaction does exactly four things, in order:
//
//  1. Read the journal (inside the transaction, a consistent snapshot) and hand it
//     to PlanWipe, which selects the scope, replays it in reverse, and decides the
//     row reverts, the undo retirements, and the conflict skips.
//
//  2. Suppress capture on the tables the reverts touch. A wipe's own reverts must
//     NOT be captured: a new stamp would become the row's latest surviving entry
//     and corrupt authorship (the latest surviving stamp names the current
//     author). Capture is suppressed by disabling the tables' user triggers
//     (the capture triggers -- triggers cannot be declared, so a declared table's
//     user triggers are exactly its capture set) for the transaction. DISABLE
//     TRIGGER is a transactional catalog change owned by the table owner (the engine
//     admin), needs no superuser, and -- crucially -- is undone by a rollback, so a
//     failed wipe never leaves capture disabled.
//
//  3. Apply the row reverts in replay order: delete the row for a disposable
//     insert, restore the captured pre-image for an update or delete.
//
//  4. Retire every visited entry's undo marker (wiped for reverted, skipped for
//     conflict-skipped) and re-enable the suppressed triggers, then commit. No
//     journal row is ever deleted -- wipe retains all journal rows -- only undo
//     markers change.
//
// Reading the whole journal into memory is sound here: wipe touches only unsealed
// partitions (sealed history is immutable by construction, partition.go), so the
// scope always lives in the bounded unsealed tail, and a wipe is a dev-loop op, not
// a hot path.

// WipeResult is the summary of a live wipe: the entries reverted and conflict-
// skipped, and the conflict reports naming each skipped entry's blocking run. It
// mirrors the pure WipePlan's summary, the outcome the command layer renders.
type WipeResult struct {
	// Wiped counts the entries reverted (undo retired to wiped).
	Wiped int
	// Skipped counts the entries conflict-skipped (undo retired to skipped).
	Skipped int
	// Conflicts are the conflict-skip reports, each naming the run whose still-in-
	// value write blocked the revert.
	Conflicts []Conflict
}

// ExecuteWipe runs one wipe over the data database in a single transaction and
// returns its summary. target is the pure
// model's scope selector: the zero value is the bare `iris workload wipe` over the
// whole wipe scope, and a populated target (Pipeline plus RunPipeline) is a named
// `iris workload wipe <pipeline>` or declare destroy's data revert, narrowed to one
// pipeline's entries.
//
// It reads the journal, plans the wipe with PlanWipe, disables capture on the
// affected tables, applies the reverts and undo retirements, re-enables capture,
// and commits -- all in one transaction, so any failure rolls the whole wipe back
// with no partial effect and capture left intact. On success the returned WipeResult
// reports the wiped and skipped counts and the conflict reports.
func (c *Client) ExecuteWipe(ctx context.Context, target WipeTarget) (result WipeResult, err error) {
	tx, err := c.pool.Begin(ctx)
	if err != nil {
		return WipeResult{}, fmt.Errorf("pg: begin wipe transaction: %w", err)
	}
	// Roll back unless an explicit commit replaces it; a post-commit rollback is a
	// no-op, so this is safe on every path.
	defer func() {
		if rbErr := tx.Rollback(ctx); rbErr != nil && !errors.Is(rbErr, pgx.ErrTxClosed) {
			err = errors.Join(err, fmt.Errorf("pg: roll back wipe transaction: %w", rbErr))
		}
	}()

	journal, err := readJournalForWipe(ctx, tx)
	if err != nil {
		return WipeResult{}, err
	}
	plan := PlanWipe(journal, target)

	// Suppress capture on exactly the tables the reverts write, for this
	// transaction. Undone by a rollback, re-enabled explicitly before commit.
	tables := affectedTables(plan.Reverts)
	if err := setCaptureTriggers(ctx, tx, tables, false); err != nil {
		return WipeResult{}, err
	}

	pkCache := map[RowKey]string{} // (schema, table) -> concat_ws pk-match expression
	for _, rev := range plan.Reverts {
		pkExpr, cacheErr := pkMatchExpr(ctx, tx, rev.Row, pkCache)
		if cacheErr != nil {
			return WipeResult{}, cacheErr
		}
		if err := applyRevert(ctx, tx, rev, pkExpr); err != nil {
			return WipeResult{}, err
		}
	}

	if err := applyRetirements(ctx, tx, plan.Retirements); err != nil {
		return WipeResult{}, err
	}

	if err := setCaptureTriggers(ctx, tx, tables, true); err != nil {
		return WipeResult{}, err
	}

	if err := tx.Commit(ctx); err != nil {
		return WipeResult{}, fmt.Errorf("pg: commit wipe transaction: %w", err)
	}
	return WipeResult{Wiped: plan.Wiped, Skipped: plan.Skipped, Conflicts: plan.Conflicts}, nil
}

// readJournalForWipe reads every data_journal row the wipe plan needs into memory,
// inside the wipe transaction so the plan and the reverts see one consistent
// snapshot. It reads all rows (any undo state): the conflict rule must see entries
// outside the scope (a later promoted or skipped write still in a row's value), so a
// scope-only read would miss the very writes that protect permanent data.
func readJournalForWipe(ctx context.Context, tx pgx.Tx) ([]JournalEntry, error) {
	rows, err := tx.Query(ctx,
		`SELECT id, run_id, "schema", "table", row_pk, op, pre_image, undo FROM public.data_journal`)
	if err != nil {
		return nil, fmt.Errorf("pg: read journal for wipe: %w", err)
	}
	defer rows.Close()

	var journal []JournalEntry
	for rows.Next() {
		var (
			e   JournalEntry
			op  string
			un  string
			pre *string
		)
		if err := rows.Scan(&e.ID, &e.RunID, &e.Schema, &e.Table, &e.RowPK, &op, &pre, &un); err != nil {
			return nil, fmt.Errorf("pg: scan journal row for wipe: %w", err)
		}
		e.Op = WriteOp(op)
		e.Undo = UndoState(un)
		if pre != nil {
			e.PreImage = *pre
		}
		journal = append(journal, e)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("pg: iterate journal for wipe: %w", err)
	}
	return journal, nil
}

// affectedTables returns the distinct (schema, table) pairs the reverts write, in
// first-seen order, as RowKeys with an empty RowPK. These are the tables whose
// capture must be suppressed for the wipe's own reverts.
func affectedTables(reverts []RowRevert) []RowKey {
	seen := map[RowKey]bool{}
	var out []RowKey
	for _, r := range reverts {
		key := RowKey{Schema: r.Row.Schema, Table: r.Row.Table}
		if seen[key] {
			continue
		}
		seen[key] = true
		out = append(out, key)
	}
	return out
}

// setCaptureTriggers enables or disables the user (capture) triggers on each table,
// so a wipe's own reverts are not re-captured. DISABLE/ENABLE TRIGGER USER is a
// transactional catalog change the table owner may issue with no superuser; inside
// the wipe transaction a disable is undone by a rollback, so a failed wipe never
// leaves capture off. enable re-arms them before commit.
func setCaptureTriggers(ctx context.Context, tx pgx.Tx, tables []RowKey, enable bool) error {
	verb := "DISABLE"
	if enable {
		verb = "ENABLE"
	}
	for _, tbl := range tables {
		stmt := fmt.Sprintf("ALTER TABLE %s.%s %s TRIGGER USER;",
			quoteIdentifier(tbl.Schema), quoteIdentifier(tbl.Table), verb)
		if _, err := tx.Exec(ctx, stmt); err != nil {
			return fmt.Errorf("pg: %s capture triggers on %s.%s: %w", verb, tbl.Schema, tbl.Table, err)
		}
	}
	return nil
}

// applyRevert applies one row rollback: deleting the row for a disposable insert, or
// restoring the captured pre-image for an update or delete. The restore is a DELETE of
// the current version followed by an INSERT of the captured prior row, as two sequential
// statements: a data-modifying CTE cannot be used here because the CTE's delete is not
// visible to the outer insert's unique-index check (both run under one snapshot), so
// restoring an update pre-image would raise a duplicate-key error. Both statements sit
// inside the single wipe transaction, so no other session ever observes the row absent.
func applyRevert(ctx context.Context, tx pgx.Tx, rev RowRevert, pkExpr string) error {
	qualified := quoteIdentifier(rev.Row.Schema) + "." + quoteIdentifier(rev.Row.Table)
	switch rev.Kind {
	case RevertDeleteRow:
		stmt := fmt.Sprintf("DELETE FROM %s WHERE %s = $1;", qualified, pkExpr)
		if _, err := tx.Exec(ctx, stmt, rev.Row.RowPK); err != nil {
			return fmt.Errorf("pg: wipe revert delete row (entry %d, %s.%s pk=%s): %w",
				rev.EntryID, rev.Row.Schema, rev.Row.Table, rev.Row.RowPK, err)
		}
	case RevertRestorePreImage:
		// Delete the current version (present for an update, absent for a delete),
		// then re-insert the captured prior row.
		del := fmt.Sprintf("DELETE FROM %s WHERE %s = $1;", qualified, pkExpr)
		if _, err := tx.Exec(ctx, del, rev.Row.RowPK); err != nil {
			return fmt.Errorf("pg: wipe revert clear current row (entry %d, %s.%s pk=%s): %w",
				rev.EntryID, rev.Row.Schema, rev.Row.Table, rev.Row.RowPK, err)
		}
		ins := fmt.Sprintf(
			"INSERT INTO %s SELECT * FROM json_populate_record(NULL::%s, $1::json);",
			qualified, qualified)
		if _, err := tx.Exec(ctx, ins, rev.PreImage); err != nil {
			return fmt.Errorf("pg: wipe revert restore pre-image (entry %d, %s.%s pk=%s): %w",
				rev.EntryID, rev.Row.Schema, rev.Row.Table, rev.Row.RowPK, err)
		}
	default:
		return fmt.Errorf("pg: wipe revert (entry %d): unknown revert kind %q", rev.EntryID, rev.Kind)
	}
	return nil
}

// applyRetirements flips the undo marker of every visited entry, batched by target
// state into at most two UPDATEs (wiped and skipped). Each is guarded WHERE
// undo='open' so it only ever retires a wipe-scope entry -- never touching promoted,
// wiped, or skipped provenance memory -- and re-running it is a no-op. No journal
// row is deleted; wipe retains all journal rows.
func applyRetirements(ctx context.Context, tx pgx.Tx, retirements []Retirement) error {
	var wiped, skipped []int64
	for _, r := range retirements {
		switch r.Undo {
		case UndoWiped:
			wiped = append(wiped, r.EntryID)
		case UndoSkipped:
			skipped = append(skipped, r.EntryID)
		default:
			return fmt.Errorf("pg: wipe retirement (entry %d): unexpected undo state %q", r.EntryID, r.Undo)
		}
	}
	for _, batch := range []struct {
		undo UndoState
		ids  []int64
	}{{UndoWiped, wiped}, {UndoSkipped, skipped}} {
		if len(batch.ids) == 0 {
			continue
		}
		stmt := fmt.Sprintf(
			"UPDATE %s SET undo = '%s' WHERE undo = 'open' AND id = ANY($1);",
			JournalTable().Qualified(), batch.undo)
		if _, err := tx.Exec(ctx, stmt, batch.ids); err != nil {
			return fmt.Errorf("pg: retire %s journal entries: %w", batch.undo, err)
		}
	}
	return nil
}

// pkMatchExpr returns the SQL expression that reconstructs a table's row_pk from a
// live row, cached per (schema, table): concat_ws('|', "col1"::text, ...) over the
// primary-key columns in key order. It mirrors capture.go's row_pk rendering exactly,
// so matching a stored row_pk against this expression identifies the same row the
// stamp attributes. A revert matches on it (WHERE <expr> = $rowpk).
func pkMatchExpr(ctx context.Context, tx pgx.Tx, row RowKey, cache map[RowKey]string) (string, error) {
	key := RowKey{Schema: row.Schema, Table: row.Table}
	if expr, ok := cache[key]; ok {
		return expr, nil
	}
	cols, err := primaryKeyColumns(ctx, tx, row.Schema, row.Table)
	if err != nil {
		return "", err
	}
	if len(cols) == 0 {
		return "", fmt.Errorf("pg: wipe cannot revert %s.%s: table has no primary key", row.Schema, row.Table)
	}
	parts := make([]string, len(cols))
	for i, col := range cols {
		parts[i] = quoteIdentifier(col) + "::text"
	}
	expr := fmt.Sprintf("concat_ws('|', %s)", strings.Join(parts, ", "))
	cache[key] = expr
	return expr, nil
}

// primaryKeyColumns reads a table's primary-key column names in key order from the
// catalog, mirroring the resolution the capture function performs at write time so
// the wipe reconstructs the same row_pk. It is a plain MVCC catalog read.
func primaryKeyColumns(ctx context.Context, tx pgx.Tx, schema, table string) ([]string, error) {
	qualified := quoteIdentifier(schema) + "." + quoteIdentifier(table)
	rows, err := tx.Query(ctx, `
SELECT a.attname
FROM pg_catalog.pg_index i
CROSS JOIN LATERAL unnest(i.indkey::int2[]) WITH ORDINALITY AS k(attnum, ord)
JOIN pg_catalog.pg_attribute a ON a.attrelid = i.indrelid AND a.attnum = k.attnum
WHERE i.indrelid = $1::regclass AND i.indisprimary
ORDER BY k.ord`, qualified)
	if err != nil {
		return nil, fmt.Errorf("pg: read primary key of %s.%s: %w", schema, table, err)
	}
	defer rows.Close()

	var cols []string
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			return nil, fmt.Errorf("pg: scan primary-key column of %s.%s: %w", schema, table, err)
		}
		cols = append(cols, name)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("pg: iterate primary key of %s.%s: %w", schema, table, err)
	}
	return cols, nil
}
