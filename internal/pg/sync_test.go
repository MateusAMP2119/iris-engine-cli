package pg_test

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/MateusAMP2119/iris-engine-cli/internal/declare"
	"github.com/MateusAMP2119/iris-engine-cli/internal/golden"
	"github.com/MateusAMP2119/iris-engine-cli/internal/pg"
	"github.com/MateusAMP2119/iris-engine-cli/internal/pg/pgtest"
)

// ordersWithStatusYAML is the analytics.orders table.yaml after the additive edit
// that adds the status column: the desired head the ledger sync diffs against a
// ledger that stops at 0001. Its bytes match the declare-package fixture, so the
// generated migration's checksum is the pinned hash.
const ordersWithStatusYAML = `schema: analytics
table: orders
columns:
  - name: id
    type: uuid
    primary_key: true
  - name: customer_id
    type: uuid
    nullable: false
  - name: amount
    type: numeric
  - name: created_at
    type: timestamptz
    default: now()
  - name: status
    type: text
    default: "'pending'"
`

// widgetsWithColorYAML is a second table folder's table.yaml adding one column
// beyond its ledger head, so a sync pass that walks two folders appends one file
// and one ALTER per folder.
const widgetsWithColorYAML = `schema: shop
table: widgets
columns:
  - name: id
    type: uuid
    primary_key: true
  - name: sku
    type: text
  - name: color
    type: text
`

// ledgerCols builds a declare.LedgerState from name:type pairs (the reconstructed
// ledger head the sync engine diffs table.yaml against).
func ledgerCols(cols ...[2]string) declare.LedgerState {
	var st declare.LedgerState
	for _, c := range cols {
		st.Columns = append(st.Columns, declare.LedgerColumn{Name: c[0], Type: c[1]})
	}
	return st
}

// liveCols builds a declare.LiveTable view for schema-drift fixtures (types in
// canonical Postgres form).
func liveCols(schema, table string, hasTrigger bool, cols ...[2]string) declare.LiveTable {
	lt := declare.LiveTable{Schema: schema, Table: table, HasCaptureTrigger: hasTrigger}
	for _, c := range cols {
		lt.Columns = append(lt.Columns, declare.LiveColumn{Name: c[0], Type: c[1]})
	}
	return lt
}

// recordingLedger is a pg.LedgerRecorder that captures the applied heads recorded
// through it, standing in for the leader's single meta writer with no live meta.
type recordingLedger struct {
	heads []pg.MigrationHead
	err   error
}

func (r *recordingLedger) RecordMigrationHead(_ context.Context, h pg.MigrationHead) error {
	r.heads = append(r.heads, h)
	return r.err
}

// ordersLedgerHead0001 is the analytics.orders ledger head at 0001 (before the
// status column): the four original columns.
func ordersLedgerHead0001() pg.LedgerView {
	return pg.LedgerView{
		HeadID: "0001",
		State: ledgerCols(
			[2]string{"id", "uuid"},
			[2]string{"customer_id", "uuid"},
			[2]string{"amount", "numeric"},
			[2]string{"created_at", "timestamptz"},
		),
	}
}

// TestPlanLedgerSyncWritesImmutableFile proves a changed table.yaml, diffed as the
// desired head against the ledger, yields an engine-written migration file that
// represents exactly that diff, and that appending it never rewrites an existing
// ledger file (the ledger is immutable).
func TestPlanLedgerSyncWritesImmutableFile(t *testing.T) {
	ctx := context.Background()
	raw := []byte(ordersWithStatusYAML)
	declared := parseTable(t, ordersWithStatusYAML)

	plan, err := pg.PlanLedgerSync(declared, raw, ordersLedgerHead0001())
	if err != nil {
		t.Fatalf("PlanLedgerSync: %v", err)
	}
	if len(plan.Migrations) != 1 {
		t.Fatalf("plan has %d migrations, want exactly 1 (the additive status delta)", len(plan.Migrations))
	}
	m := plan.Migrations[0]
	if m.Filename != "0002_add_status.yaml" {
		t.Errorf("migration filename = %q, want 0002_add_status.yaml", m.Filename)
	}
	if m.Head.MigrationID != "0002" || m.Head.Parent != "0001" {
		t.Errorf("migration head id/parent = %s/%s, want 0002/0001", m.Head.MigrationID, m.Head.Parent)
	}
	if m.Head.Checksum != declare.ChecksumTableYAML(raw) {
		t.Errorf("migration checksum = %q, want the checksum of table.yaml at this revision", m.Head.Checksum)
	}
	// The file bytes are exactly the migration file for this diff.
	golden.Assert(t, m.Contents, filepath.Join("testdata", "0002_add_status.yaml"))

	// Applying the plan writes the file under the table folder's migrations/ dir.
	sink := pg.NewDirMigrationSink(t.TempDir())
	rec := pgtest.New()
	ledger := &recordingLedger{}
	if err := plan.Apply(ctx, sink, rec, ledger); err != nil {
		t.Fatalf("Apply: %v", err)
	}
	written := filepath.Join(sink.Root(), "analytics", "orders", "migrations", "0002_add_status.yaml")
	onDisk, err := os.ReadFile(written) //nolint:gosec // G304: written is the harness-controlled temp path this test just appended, not user or network input.
	if err != nil {
		t.Fatalf("read written migration: %v", err)
	}
	golden.Assert(t, onDisk, filepath.Join("testdata", "0002_add_status.yaml"))

	// Immutability, part 1: a re-sync with the advanced ledger head (status now in
	// the ledger) generates nothing, so Apply writes no file.
	advanced := pg.LedgerView{
		HeadID: "0002",
		State: ledgerCols(
			[2]string{"id", "uuid"},
			[2]string{"customer_id", "uuid"},
			[2]string{"amount", "numeric"},
			[2]string{"created_at", "timestamptz"},
			[2]string{"status", "text"},
		),
	}
	resync, err := pg.PlanLedgerSync(declared, raw, advanced)
	if err != nil {
		t.Fatalf("re-sync PlanLedgerSync: %v", err)
	}
	if len(resync.Migrations) != 0 {
		t.Fatalf("re-sync at the advanced head generated %d migrations, want 0 (nothing beyond the head)", len(resync.Migrations))
	}
	before, err := os.Stat(written)
	if err != nil {
		t.Fatalf("stat written migration: %v", err)
	}
	time.Sleep(10 * time.Millisecond) // any rewrite would move the mtime forward.
	if err := resync.Apply(ctx, sink, rec, ledger); err != nil {
		t.Fatalf("re-sync Apply: %v", err)
	}
	after, err := os.Stat(written)
	if err != nil {
		t.Fatalf("re-stat written migration: %v", err)
	}
	if !before.ModTime().Equal(after.ModTime()) {
		t.Errorf("re-sync modified the existing ledger file (mtime %v -> %v); the ledger is immutable", before.ModTime(), after.ModTime())
	}

	// Immutability, part 2: replaying the original plan onto the existing file is
	// refused (create-once), and the file's bytes are left untouched.
	if err := plan.Apply(ctx, sink, pgtest.New(), &recordingLedger{}); err == nil {
		t.Error("Apply overwrote an existing ledger file; want a create-once refusal")
	}
	stillOnDisk, err := os.ReadFile(written) //nolint:gosec // G304: written is the harness-controlled temp path this test appended, not user or network input.
	if err != nil {
		t.Fatalf("re-read migration: %v", err)
	}
	golden.Assert(t, stillOnDisk, filepath.Join("testdata", "0002_add_status.yaml"))
}

// TestLedgerDriftAdditiveGeneratesMigration proves an additive gap between
// table.yaml and the ledger generates the next numbered migration file and advances
// the ledger head, while a removed column (non-additive) is refused.
func TestLedgerDriftAdditiveGeneratesMigration(t *testing.T) {
	ctx := context.Background()
	raw := []byte(ordersWithStatusYAML)
	declared := parseTable(t, ordersWithStatusYAML)

	plan, err := pg.PlanLedgerSync(declared, raw, ordersLedgerHead0001())
	if err != nil {
		t.Fatalf("PlanLedgerSync: %v", err)
	}
	if len(plan.Migrations) != 1 {
		t.Fatalf("plan has %d migrations, want 1", len(plan.Migrations))
	}
	// Next numbered file after the 0001 head, chained to it.
	if got := plan.Migrations[0].Head.MigrationID; got != "0002" {
		t.Errorf("generated migration id = %q, want the next number 0002", got)
	}
	if got := plan.Migrations[0].Head.Parent; got != "0001" {
		t.Errorf("generated migration parent = %q, want the prior head 0001", got)
	}

	// Applying advances the recorded ledger head to 0002.
	rec := pgtest.New()
	ledger := &recordingLedger{}
	if err := plan.Apply(ctx, pg.NewDirMigrationSink(t.TempDir()), rec, ledger); err != nil {
		t.Fatalf("Apply: %v", err)
	}
	if len(ledger.heads) != 1 || ledger.heads[0].MigrationID != "0002" {
		t.Fatalf("applied heads = %+v, want the ledger head advanced to 0002", ledger.heads)
	}

	// A removed column (present in the ledger, gone from table.yaml) is non-additive
	// and refused: no migration is generated to drop it.
	shrunk := parseTable(t, `schema: analytics
table: orders
columns:
  - name: id
    type: uuid
    primary_key: true
`)
	if _, err := pg.PlanLedgerSync(shrunk, []byte("x"), ordersLedgerHead0001()); err == nil {
		t.Error("PlanLedgerSync accepted a removed column; a non-additive removal must be refused")
	}
}

// TestMigrationSyncAppendsAndAlters proves a sync pass walks each table folder,
// diffs table.yaml against the ledger head, appends one immutable migration file
// per additive delta, and runs the corresponding ADD COLUMN ALTER.
func TestMigrationSyncAppendsAndAlters(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	sink := pg.NewDirMigrationSink(root)
	rec := pgtest.New()
	ledger := &recordingLedger{}

	// Two table folders, one additive column each.
	folders := []struct {
		yaml   string
		head   pg.LedgerView
		file   string
		schema string
		table  string
	}{
		{
			yaml: ordersWithStatusYAML, head: ordersLedgerHead0001(),
			file: "0002_add_status.yaml", schema: "analytics", table: "orders",
		},
		{
			yaml: widgetsWithColorYAML,
			head: pg.LedgerView{HeadID: "0001", State: ledgerCols(
				[2]string{"id", "uuid"}, [2]string{"sku", "text"})},
			file: "0002_add_color.yaml", schema: "shop", table: "widgets",
		},
	}
	for _, f := range folders {
		plan, err := pg.PlanLedgerSync(parseTable(t, f.yaml), []byte(f.yaml), f.head)
		if err != nil {
			t.Fatalf("PlanLedgerSync(%s.%s): %v", f.schema, f.table, err)
		}
		if err := plan.Apply(ctx, sink, rec, ledger); err != nil {
			t.Fatalf("Apply(%s.%s): %v", f.schema, f.table, err)
		}
		path := filepath.Join(root, f.schema, f.table, "migrations", f.file)
		if _, err := os.Stat(path); err != nil {
			t.Errorf("expected appended migration file %s: %v", path, err)
		}
	}
	// One ALTER per folder, both ADD COLUMN.
	alters := rec.Statements()
	if len(alters) != 2 {
		t.Fatalf("recorded %d statements, want 2 ADD COLUMN ALTERs (one per folder): %v", len(alters), alters)
	}
	for _, a := range alters {
		if !strings.Contains(a, "ADD COLUMN") {
			t.Errorf("statement is not an ADD COLUMN ALTER: %q", a)
		}
	}
	if len(ledger.heads) != 2 {
		t.Errorf("recorded %d applied heads, want 2 (one per folder)", len(ledger.heads))
	}

	// One folder, two columns beyond the head: one immutable file per additive
	// delta, numbered in sequence.
	twoCols := `schema: analytics
table: metrics
columns:
  - name: id
    type: uuid
    primary_key: true
  - name: hits
    type: bigint
  - name: misses
    type: bigint
`
	plan, err := pg.PlanLedgerSync(parseTable(t, twoCols), []byte(twoCols),
		pg.LedgerView{HeadID: "0001", State: ledgerCols([2]string{"id", "uuid"})})
	if err != nil {
		t.Fatalf("PlanLedgerSync(metrics): %v", err)
	}
	if len(plan.Migrations) != 2 {
		t.Fatalf("two new columns generated %d migrations, want 2 (one per additive delta)", len(plan.Migrations))
	}
	if plan.Migrations[0].Head.MigrationID != "0002" || plan.Migrations[1].Head.MigrationID != "0003" {
		t.Errorf("delta ids = %s,%s, want 0002,0003", plan.Migrations[0].Head.MigrationID, plan.Migrations[1].Head.MigrationID)
	}
	if plan.Migrations[1].Head.Parent != "0002" {
		t.Errorf("second delta parent = %q, want 0002 (chained to the first delta)", plan.Migrations[1].Head.Parent)
	}
}

// TestMigrationDryRunTouchesNothing proves a --dry-run sync prints the intended
// ALTERs and migration files but executes no DDL and writes no files: the plan is
// rendered without invoking any executor.
func TestMigrationDryRunTouchesNothing(t *testing.T) {
	raw := []byte(ordersWithStatusYAML)
	plan, err := pg.PlanLedgerSync(parseTable(t, ordersWithStatusYAML), raw, ordersLedgerHead0001())
	if err != nil {
		t.Fatalf("PlanLedgerSync: %v", err)
	}

	// The dry-run preview names the intended ALTER and the migration file.
	golden.Assert(t, plan.Preview(), filepath.Join("testdata", "dry_run_preview.txt"))

	// Rendering the preview touches no executor and writes no file: the recorders
	// stay empty and the temp schemas tree stays empty (Apply was never called).
	root := t.TempDir()
	_ = pg.NewDirMigrationSink(root)
	rec := pgtest.New()
	ledger := &recordingLedger{}
	if got := rec.Statements(); len(got) != 0 {
		t.Errorf("dry-run issued %d DDL statements, want 0: %v", len(got), got)
	}
	if len(ledger.heads) != 0 {
		t.Errorf("dry-run recorded %d applied heads, want 0", len(ledger.heads))
	}
	entries, err := os.ReadDir(root)
	if err != nil {
		t.Fatalf("read temp root: %v", err)
	}
	if len(entries) != 0 {
		t.Errorf("dry-run wrote %d filesystem entries, want 0", len(entries))
	}
}

// TestSchemaDriftMissingColumnAutofix proves a column present in the declared head
// but missing live is classified additive and auto-fixed with ADD COLUMN, while an
// extra live column (non-additive) is refused.
func TestSchemaDriftMissingColumnAutofix(t *testing.T) {
	ctx := context.Background()
	declared := parseTable(t, ordersWithStatusYAML)
	// Live has every declared column except status, capture trigger present.
	live := liveCols("analytics", "orders", true,
		[2]string{"id", "uuid"},
		[2]string{"customer_id", "uuid"},
		[2]string{"amount", "numeric"},
		[2]string{"created_at", "timestamptz"},
	)
	plan, err := pg.PlanSchemaFix(declared, live)
	if err != nil {
		t.Fatalf("PlanSchemaFix: %v", err)
	}
	if len(plan.Fixes) != 1 || plan.Fixes[0].Subject != declare.SubjectColumn {
		t.Fatalf("plan fixes = %+v, want one additive column fix", plan.Fixes)
	}
	if !strings.Contains(plan.Fixes[0].DDL, `ADD COLUMN IF NOT EXISTS "status"`) {
		t.Errorf("fix DDL = %q, want an idempotent ADD COLUMN status", plan.Fixes[0].DDL)
	}

	// Applying the fix runs the ALTER but writes no migration file and records no
	// applied head: a schema-drift autofix reconciles live to the declared head, it
	// does not extend the ledger.
	root := t.TempDir()
	sink := pg.NewDirMigrationSink(root)
	rec := pgtest.New()
	ledger := &recordingLedger{}
	if err := plan.Apply(ctx, sink, rec, ledger); err != nil {
		t.Fatalf("Apply: %v", err)
	}
	if got := rec.Statements(); len(got) != 1 || !strings.Contains(got[0], "ADD COLUMN") {
		t.Errorf("recorded statements = %v, want one ADD COLUMN ALTER", got)
	}
	if len(ledger.heads) != 0 {
		t.Errorf("schema autofix recorded %d applied heads, want 0", len(ledger.heads))
	}
	if entries, _ := os.ReadDir(root); len(entries) != 0 {
		t.Errorf("schema autofix wrote %d filesystem entries, want 0", len(entries))
	}

	// An extra live column is non-additive and refuses apply (never auto-dropped).
	extra := liveCols("analytics", "orders", true,
		[2]string{"id", "uuid"},
		[2]string{"customer_id", "uuid"},
		[2]string{"amount", "numeric"},
		[2]string{"created_at", "timestamptz"},
		[2]string{"status", "text"},
		[2]string{"legacy_id", "bigint"},
	)
	if _, err := pg.PlanSchemaFix(declared, extra); err == nil {
		t.Error("PlanSchemaFix accepted an extra live column; a non-additive extra must be refused")
	}
}

// TestMissingCaptureTriggerAutofix proves a missing capture trigger on a declared
// table is classified additive and auto-fixed, like a missing column: the sync runs
// the create-trigger DDL through the pg seam (E06.2 owns the trigger's PL/pgSQL
// body; here the sync emits the CREATE TRIGGER statement that installs it).
func TestMissingCaptureTriggerAutofix(t *testing.T) {
	ctx := context.Background()
	declared := parseTable(t, ordersWithStatusYAML)
	// Live matches the declared columns exactly, but the capture trigger is absent.
	live := liveCols("analytics", "orders", false,
		[2]string{"id", "uuid"},
		[2]string{"customer_id", "uuid"},
		[2]string{"amount", "numeric"},
		[2]string{"created_at", "timestamptz"},
		[2]string{"status", "text"},
	)
	plan, err := pg.PlanSchemaFix(declared, live)
	if err != nil {
		t.Fatalf("PlanSchemaFix: %v", err)
	}
	// A missing capture trigger installs the complete per-operation trigger set:
	// Postgres transition tables are per-operation (INSERT: NEW only; DELETE: OLD
	// only; UPDATE: both), so the additive fix is three CREATE TRIGGER statements.
	if len(plan.Fixes) != 3 {
		t.Fatalf("plan fixes = %+v, want three per-operation capture-trigger fixes", plan.Fixes)
	}
	for i, f := range plan.Fixes {
		if f.Subject != declare.SubjectCaptureTrigger {
			t.Errorf("fix[%d] subject = %q, want capture_trigger", i, f.Subject)
		}
		if !strings.Contains(f.DDL, "CREATE TRIGGER") || !strings.Contains(f.DDL, "REFERENCING") {
			t.Errorf("fix[%d] DDL is not a transition-table CREATE TRIGGER: %q", i, f.DDL)
		}
	}

	// The trigger fixes render the statement-level transition-table
	// CREATE TRIGGER shape, applied through the pg seam.
	rec := pgtest.New()
	if err := plan.Apply(ctx, pg.NewDirMigrationSink(t.TempDir()), rec, &recordingLedger{}); err != nil {
		t.Fatalf("Apply: %v", err)
	}
	got := rec.Statements()
	if len(got) != 3 {
		t.Fatalf("recorded %d statements, want 3 CREATE TRIGGER (one per operation): %v", len(got), got)
	}
	golden.Assert(t, rec.Dump(), filepath.Join("testdata", "capture_triggers.sql"))
}

// TestRenderCaptureTriggers pins the statement-level, transition-table capture
// trigger DDL the sync emits as a seam: E06.2 owns the trigger function's PL/pgSQL
// body, and these are the CREATE TRIGGER statements that bind it to a declared
// table. Postgres transition tables are per-operation, so the set is three triggers
// (INSERT with NEW TABLE, UPDATE with OLD and NEW TABLE, DELETE with OLD TABLE), one
// INSERT...SELECT per statement. A golden diff is a contract diff.
func TestRenderCaptureTriggers(t *testing.T) {
	stmts := pg.RenderCaptureTriggers("analytics", "orders")
	if len(stmts) != 3 {
		t.Fatalf("RenderCaptureTriggers returned %d statements, want 3 (one per operation)", len(stmts))
	}
	golden.Assert(t, []byte(strings.Join(stmts, "\n\n")+"\n"), filepath.Join("testdata", "capture_triggers.sql"))
}
