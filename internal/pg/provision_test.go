package pg_test

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/MateusAMP2119/iris-engine-cli/internal/declare"
	"github.com/MateusAMP2119/iris-engine-cli/internal/fixtures"
	"github.com/MateusAMP2119/iris-engine-cli/internal/golden"
	"github.com/MateusAMP2119/iris-engine-cli/internal/pg"
	"github.com/MateusAMP2119/iris-engine-cli/internal/pg/pgtest"
)

// emptyLive is the data database before any provisioning: no schema, table,
// capture trigger, or journal exists. The maps are non-nil so lookups are safe.
func emptyLive() pg.LiveView {
	return pg.LiveView{
		Schemas:         map[string]bool{},
		Tables:          map[string]bool{},
		CaptureTriggers: map[string]bool{},
	}
}

// discoverGoldenSchemas discovers the golden sample workspace's schemas/ tree
// (analytics.orders and raw.orders_staging), the pipeline-independent input to
// provisioning. Each DiscoveredTable carries its verbatim table.yaml Raw bytes,
// the authoritative source of the create-head checksum.
func discoverGoldenSchemas(t *testing.T) []declare.DiscoveredTable {
	t.Helper()
	ws, err := declare.DiscoverWorkspace(fixtures.WorkspaceGolden())
	if err != nil {
		t.Fatalf("DiscoverWorkspace: %v", err)
	}
	if len(ws.Schemas) == 0 {
		t.Fatalf("golden workspace has no schemas")
	}
	return ws.Schemas
}

// freshLedgers builds a per-table ledger view for freshly declared tables: no
// migration files on disk and no applied head. The create-head checksum comes from
// each DiscoveredTable's Raw bytes, not the ledger, so the map need not carry them.
func freshLedgers(tables []declare.DiscoveredTable) map[string]pg.TableLedger {
	ledgers := make(map[string]pg.TableLedger, len(tables))
	for _, dt := range tables {
		ledgers[dt.Schema+"."+dt.Table] = pg.TableLedger{}
	}
	return ledgers
}

// TestProvisionCreateIfMissing proves provisioning walks schemas/ and emits a
// CREATE SCHEMA IF NOT EXISTS per schema folder plus a CREATE TABLE from the
// table.yaml head for each missing table (and ends by ensuring capture: the
// partitioned journal once and the per-table capture triggers). The emitted DDL
// stream is pinned byte-for-byte; a golden diff is a contract diff.
//
// spec: S05/provision-create-if-missing
func TestProvisionCreateIfMissing(t *testing.T) {
	ctx := context.Background()
	tables := discoverGoldenSchemas(t)
	ledgers := freshLedgers(tables)

	plan, err := pg.PlanProvision(tables, emptyLive(), ledgers)
	if err != nil {
		t.Fatalf("PlanProvision: %v", err)
	}

	// One CREATE SCHEMA per distinct schema folder (analytics, raw), sorted.
	if got := plan.Schemas; len(got) != 2 || got[0] != "analytics" || got[1] != "raw" {
		t.Errorf("plan.Schemas = %v, want [analytics raw]", got)
	}
	// A missing table takes the create-from-head branch, never replay.
	if len(plan.Tables) != 2 {
		t.Fatalf("plan has %d tables, want 2", len(plan.Tables))
	}
	for _, tp := range plan.Tables {
		if _, ok := tp.Branch.(pg.CreateFromHead); !ok {
			t.Errorf("%s.%s branch = %T, want CreateFromHead", tp.Schema, tp.Table, tp.Branch)
		}
	}

	rec := pgtest.New()
	ledger := &recordingLedger{}
	if err := plan.Apply(ctx, rec, ledger); err != nil {
		t.Fatalf("Apply: %v", err)
	}
	golden.Assert(t, rec.Dump(), filepath.Join("testdata", "provision_create_if_missing.sql"))
}

// TestProvisionAppliesPendingMigrations proves that for a table already present
// in live Postgres, provisioning applies its pending additive migrations (the
// migration files on disk beyond the recorded applied head) instead of recreating
// the table: no CREATE TABLE, one ADD COLUMN ALTER, and the replayed head
// recorded.
//
// spec: S05/provision-applies-pending-migrations
func TestProvisionAppliesPendingMigrations(t *testing.T) {
	ctx := context.Background()

	// A table folder whose table.yaml head carries the status column and whose
	// migrations/ ledger holds the 0002 migration that adds it.
	declared := parseTable(t, ordersWithStatusYAML)
	migrationsDir := filepath.Join(t.TempDir(), "migrations")
	if err := os.MkdirAll(migrationsDir, 0o755); err != nil {
		t.Fatalf("mkdir migrations: %v", err)
	}
	m0002 := declare.MigrationFile{
		ID: "0002", Parent: "0001", Op: "add_column",
		Column:   declare.MigrationColumn{Name: "status", Type: "text", Default: "'pending'"},
		Checksum: declare.ChecksumTableYAML([]byte(ordersWithStatusYAML)),
	}
	data, err := declare.MarshalMigration(m0002)
	if err != nil {
		t.Fatalf("MarshalMigration: %v", err)
	}
	if err := os.WriteFile(filepath.Join(migrationsDir, "0002_add_status.yaml"), data, 0o644); err != nil {
		t.Fatalf("write migration: %v", err)
	}

	// Read the ledger back off disk (real local file I/O, no live Postgres).
	disk, err := pg.LoadDiskMigrations(migrationsDir)
	if err != nil {
		t.Fatalf("LoadDiskMigrations: %v", err)
	}
	if len(disk) != 1 || disk[0].ID != "0002" {
		t.Fatalf("LoadDiskMigrations = %+v, want the single 0002 migration", disk)
	}

	tables := []declare.DiscoveredTable{{
		Schema: "analytics", Table: "orders", Dir: filepath.Dir(migrationsDir),
		Spec: declared, HasMigrations: true,
	}}
	// The table already exists, at applied head 0001, with its trigger and the
	// journal in place: only the pending 0002 migration is outstanding.
	live := pg.LiveView{
		Schemas:         map[string]bool{"analytics": true},
		Tables:          map[string]bool{"analytics.orders": true},
		CaptureTriggers: map[string]bool{"analytics.orders": true},
		HasJournal:      true,
	}
	ledgers := map[string]pg.TableLedger{
		"analytics.orders": {
			DiskMigrations: disk,
			AppliedHeadID:  "0001",
		},
	}

	plan, err := pg.PlanProvision(tables, live, ledgers)
	if err != nil {
		t.Fatalf("PlanProvision: %v", err)
	}

	// No schema and no journal statements: both already exist.
	if len(plan.Schemas) != 0 {
		t.Errorf("plan.Schemas = %v, want none (schema exists)", plan.Schemas)
	}
	if plan.EnsureJournal {
		t.Error("plan ensures the journal again; it already exists")
	}
	if len(plan.Tables) != 1 {
		t.Fatalf("plan has %d tables, want 1", len(plan.Tables))
	}
	// Existing table: the replay branch, never create-from-head.
	replay, ok := plan.Tables[0].Branch.(pg.ReplayPending)
	if !ok {
		t.Fatalf("branch = %T, want ReplayPending", plan.Tables[0].Branch)
	}
	if len(replay.Migrations) != 1 || replay.Migrations[0].Head.MigrationID != "0002" {
		t.Fatalf("replay = %+v, want the single pending 0002 migration", replay.Migrations)
	}

	rec := pgtest.New()
	ledger := &recordingLedger{}
	if err := plan.Apply(ctx, rec, ledger); err != nil {
		t.Fatalf("Apply: %v", err)
	}
	stmts := rec.Statements()
	if len(stmts) != 1 {
		t.Fatalf("recorded %d statements, want exactly 1 ADD COLUMN ALTER (no recreate): %v", len(stmts), stmts)
	}
	if strings.Contains(stmts[0], "CREATE TABLE") {
		t.Errorf("provisioning recreated the existing table: %q", stmts[0])
	}
	if !strings.Contains(stmts[0], `ADD COLUMN "status"`) {
		t.Errorf("statement = %q, want an ADD COLUMN status ALTER", stmts[0])
	}
	if len(ledger.heads) != 1 || ledger.heads[0].MigrationID != "0002" || ledger.heads[0].Parent != "0001" {
		t.Errorf("recorded heads = %+v, want the replayed 0002 head chained to 0001", ledger.heads)
	}
}

// TestProvisionOnePathPerTable proves provisioning selects exactly one path per
// table: create-from-head for a missing table XOR pending-migration replay for an
// existing one, never both. The branch is chosen by a single predicate (the table
// exists live or not) and carried as a closed oneof, so a table structurally
// cannot hold both paths.
//
// spec: S05/provision-one-path-per-table
func TestProvisionOnePathPerTable(t *testing.T) {
	declared := parseTable(t, ordersWithStatusYAML)
	raw := []byte(ordersWithStatusYAML)
	// A ledger with a 0002 migration file already on disk, so both branch inputs
	// are non-trivial: the create path would still create-from-head (recording the
	// 0002 head), the replay path would still replay the pending 0002.
	withDisk := pg.TableLedger{
		DiskMigrations: []declare.MigrationFile{{
			ID: "0002", Parent: "0001", Op: "add_column",
			Column:   declare.MigrationColumn{Name: "status", Type: "text", Default: "'pending'"},
			Checksum: declare.ChecksumTableYAML(raw),
		}},
	}
	noDisk := pg.TableLedger{}

	cases := []struct {
		name   string
		exists bool
		led    pg.TableLedger
		want   string // "create" or "replay"
	}{
		{"missing_no_migrations", false, noDisk, "create"},
		{"missing_with_migrations", false, withDisk, "create"},
		{"existing_no_migrations", true, noDisk, "replay"},
		{"existing_with_pending", true, pg.TableLedger{DiskMigrations: withDisk.DiskMigrations, AppliedHeadID: "0001"}, "replay"},
		{"existing_all_applied", true, pg.TableLedger{DiskMigrations: withDisk.DiskMigrations, AppliedHeadID: "0002"}, "replay"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			// raw is the declared table.yaml bytes, the create-head checksum source.
			branch, err := pg.SelectTableBranch(tc.exists, declared, raw, tc.led)
			if err != nil {
				t.Fatalf("SelectTableBranch: %v", err)
			}
			_, isCreate := branch.(pg.CreateFromHead)
			_, isReplay := branch.(pg.ReplayPending)
			// Exactly one branch type: the oneof makes both-at-once impossible, and
			// the predicate makes neither impossible.
			if isCreate == isReplay {
				t.Fatalf("branch %T is neither exclusively create nor replay (create=%v replay=%v)", branch, isCreate, isReplay)
			}
			switch tc.want {
			case "create":
				if !isCreate {
					t.Errorf("missing table took %T, want CreateFromHead", branch)
				}
			case "replay":
				if !isReplay {
					t.Errorf("existing table took %T, want ReplayPending", branch)
				}
			}
		})
	}
}

// TestProvisionHeadCreateRecordsLedger proves that when a table is created from
// its table.yaml head, the ledger head is recorded as applied: the head is the
// highest migration id present in the table's migrations/ directory, or 0001 (the
// implicit create head, whose revision is table.yaml itself) when none are.
//
// spec: S05/provision-head-create-records-ledger
func TestProvisionHeadCreateRecordsLedger(t *testing.T) {
	ctx := context.Background()

	t.Run("no_migrations_records_0001", func(t *testing.T) {
		declared := parseTable(t, ordersWithStatusYAML)
		raw := []byte(ordersWithStatusYAML)
		tables := []declare.DiscoveredTable{{Schema: "analytics", Table: "orders", Spec: declared, Raw: raw}}
		ledgers := map[string]pg.TableLedger{"analytics.orders": {}}

		plan, err := pg.PlanProvision(tables, emptyLive(), ledgers)
		if err != nil {
			t.Fatalf("PlanProvision: %v", err)
		}
		ledger := &recordingLedger{}
		if err := plan.Apply(ctx, pgtest.New(), ledger); err != nil {
			t.Fatalf("Apply: %v", err)
		}
		if len(ledger.heads) != 1 {
			t.Fatalf("recorded %d heads, want 1", len(ledger.heads))
		}
		h := ledger.heads[0]
		if h.Schema != "analytics" || h.Table != "orders" {
			t.Errorf("head key = %s.%s, want analytics.orders", h.Schema, h.Table)
		}
		if h.MigrationID != "0001" || h.Parent != "" {
			t.Errorf("head id/parent = %s/%q, want 0001/\"\" (the create head)", h.MigrationID, h.Parent)
		}
		if h.Checksum != declare.ChecksumTableYAML(raw) {
			t.Errorf("head checksum = %q, want the checksum of table.yaml", h.Checksum)
		}
	})

	t.Run("records_highest_present_migration", func(t *testing.T) {
		declared := parseTable(t, ordersWithStatusYAML)
		raw := []byte(ordersWithStatusYAML)
		// The table folder already carries migrations 0002 and 0003 on disk (a fresh
		// data database catching up on a repo with committed migrations): create-from
		// -head materializes the whole head in one statement and records the head at
		// the highest present id, never replaying the intermediate migrations.
		disk := []declare.MigrationFile{
			{ID: "0002", Parent: "0001", Op: "add_column", Column: declare.MigrationColumn{Name: "status", Type: "text"}, Checksum: "c2"},
			{ID: "0003", Parent: "0002", Op: "add_column", Column: declare.MigrationColumn{Name: "note", Type: "text"}, Checksum: "c3"},
		}
		tables := []declare.DiscoveredTable{{Schema: "analytics", Table: "orders", Spec: declared, Raw: raw, HasMigrations: true}}
		ledgers := map[string]pg.TableLedger{"analytics.orders": {DiskMigrations: disk}}

		plan, err := pg.PlanProvision(tables, emptyLive(), ledgers)
		if err != nil {
			t.Fatalf("PlanProvision: %v", err)
		}
		rec := pgtest.New()
		ledger := &recordingLedger{}
		if err := plan.Apply(ctx, rec, ledger); err != nil {
			t.Fatalf("Apply: %v", err)
		}
		// One CREATE TABLE, no ADD COLUMN replay (create-from-head is a single path).
		for _, s := range rec.Statements() {
			if strings.Contains(s, "ADD COLUMN") {
				t.Errorf("create-from-head replayed a migration ALTER: %q", s)
			}
		}
		if len(ledger.heads) != 1 {
			t.Fatalf("recorded %d heads, want exactly 1 (the ledger head)", len(ledger.heads))
		}
		h := ledger.heads[0]
		if h.MigrationID != "0003" || h.Parent != "0002" || h.Checksum != "c3" {
			t.Errorf("head = %+v, want the highest present migration 0003 (parent 0002, checksum c3)", h)
		}
	})

	t.Run("absent_ledger_entry_checksums_declared_yaml", func(t *testing.T) {
		// The reconstructed-ledger map carries no entry for this table (a brand-new
		// declared table whose ledger has never been read/reconstructed). The create
		// head's checksum must still pin the DECLARED table.yaml bytes, never a zero
		// ledger's nil Raw (whose sha256 of empty bytes is a permanently wrong head).
		declared := parseTable(t, ordersWithStatusYAML)
		raw := []byte(ordersWithStatusYAML)
		tables := []declare.DiscoveredTable{{Schema: "analytics", Table: "orders", Spec: declared, Raw: raw}}
		ledgers := map[string]pg.TableLedger{} // no entry for analytics.orders

		plan, err := pg.PlanProvision(tables, emptyLive(), ledgers)
		if err != nil {
			t.Fatalf("PlanProvision: %v", err)
		}
		ledger := &recordingLedger{}
		if err := plan.Apply(ctx, pgtest.New(), ledger); err != nil {
			t.Fatalf("Apply: %v", err)
		}
		if len(ledger.heads) != 1 {
			t.Fatalf("recorded %d heads, want 1", len(ledger.heads))
		}
		h := ledger.heads[0]
		if h.MigrationID != "0001" || h.Parent != "" {
			t.Errorf("head id/parent = %s/%q, want 0001/\"\" (the create head)", h.MigrationID, h.Parent)
		}
		if want := declare.ChecksumTableYAML(raw); h.Checksum != want {
			t.Errorf("head checksum = %q, want %q (the declared table.yaml, not an absent ledger's nil bytes)", h.Checksum, want)
		}
	})

	t.Run("empty_declared_bytes_error", func(t *testing.T) {
		// A create-from-head with no migration files and no declared table.yaml bytes
		// has no source for the create-head checksum: provisioning must error rather
		// than checksum nil (the sha256 of empty bytes) into a permanently wrong head.
		declared := parseTable(t, ordersWithStatusYAML)
		tables := []declare.DiscoveredTable{{Schema: "analytics", Table: "orders", Spec: declared, Raw: nil}}
		ledgers := map[string]pg.TableLedger{}

		if _, err := pg.PlanProvision(tables, emptyLive(), ledgers); err == nil {
			t.Fatal("PlanProvision accepted a create-from-head with empty declared table.yaml bytes; want an error")
		}
	})

	t.Run("malformed_migration_id_on_create_errors", func(t *testing.T) {
		// The create path's head is the highest migration id on disk. A malformed
		// (non-numeric) id is a corrupt ledger: provisioning must error -- consistent
		// with the replay path, which hard-errors on the same input -- never silently
		// skip it and fall through to the implicit 0001 head.
		declared := parseTable(t, ordersWithStatusYAML)
		raw := []byte(ordersWithStatusYAML)
		disk := []declare.MigrationFile{
			{ID: "bogus", Parent: "0001", Op: "add_column", Column: declare.MigrationColumn{Name: "status", Type: "text"}, Checksum: "cx"},
		}
		tables := []declare.DiscoveredTable{{Schema: "analytics", Table: "orders", Spec: declared, Raw: raw, HasMigrations: true}}
		ledgers := map[string]pg.TableLedger{"analytics.orders": {DiskMigrations: disk}}

		if _, err := pg.PlanProvision(tables, emptyLive(), ledgers); err == nil {
			t.Fatal("PlanProvision silently accepted a malformed migration id on the create path; want an error, never a fallthrough to 0001")
		}
	})
}

// TestProvisionIdempotent proves re-running provisioning against an
// already-provisioned target emits no schema, table, or migration changes: the
// re-planned plan is empty and applying it issues no statements and records no
// heads.
//
// spec: S05/provision-idempotent
func TestProvisionIdempotent(t *testing.T) {
	ctx := context.Background()
	tables := discoverGoldenSchemas(t)
	ledgers := freshLedgers(tables)

	// First provision against a fresh data database does real work.
	first, err := pg.PlanProvision(tables, emptyLive(), ledgers)
	if err != nil {
		t.Fatalf("PlanProvision (first): %v", err)
	}
	if first.Empty() {
		t.Fatal("first provision plan is empty; a fresh data database needs provisioning")
	}
	if err := first.Apply(ctx, pgtest.New(), &recordingLedger{}); err != nil {
		t.Fatalf("Apply (first): %v", err)
	}

	// The data database now reflects the provisioned world: every schema and table
	// exists, every capture trigger is installed, and the journal is present.
	provisioned := pg.LiveView{
		Schemas:         map[string]bool{},
		Tables:          map[string]bool{},
		CaptureTriggers: map[string]bool{},
		HasJournal:      true,
	}
	for _, dt := range tables {
		provisioned.Schemas[dt.Schema] = true
		provisioned.Tables[dt.Schema+"."+dt.Table] = true
		provisioned.CaptureTriggers[dt.Schema+"."+dt.Table] = true
	}

	second, err := pg.PlanProvision(tables, provisioned, ledgers)
	if err != nil {
		t.Fatalf("PlanProvision (second): %v", err)
	}
	if !second.Empty() {
		t.Errorf("re-provision plan is not empty: %+v", second)
	}
	rec := pgtest.New()
	ledger := &recordingLedger{}
	if err := second.Apply(ctx, rec, ledger); err != nil {
		t.Fatalf("Apply (second): %v", err)
	}
	if got := rec.Statements(); len(got) != 0 {
		t.Errorf("re-provision issued %d statements, want 0: %v", len(got), got)
	}
	if len(ledger.heads) != 0 {
		t.Errorf("re-provision recorded %d heads, want 0", len(ledger.heads))
	}
}

// TestProvisionPipelineIndependent proves provisioning is pipeline-independent:
// every table under schemas/ is planned regardless of whether any pipeline
// declares reads or writes on it. PlanProvision's only table source is the
// schemas/ tree -- its signature carries no pipeline, reads, or writes input -- so
// a table that no pipeline references is planned identically to one that many do.
//
// spec: S05/provision-pipeline-independent
func TestProvisionPipelineIndependent(t *testing.T) {
	// An orphan table under schemas/ that no pipeline reads or writes.
	orphanYAML := `schema: analytics
table: orphan
columns:
  - name: id
    type: uuid
    primary_key: true
`
	orphan := parseTable(t, orphanYAML)
	tables := []declare.DiscoveredTable{{Schema: "analytics", Table: "orphan", Spec: orphan, Raw: []byte(orphanYAML)}}
	ledgers := map[string]pg.TableLedger{"analytics.orphan": {}}

	plan, err := pg.PlanProvision(tables, emptyLive(), ledgers)
	if err != nil {
		t.Fatalf("PlanProvision: %v", err)
	}
	if len(plan.Tables) != 1 || plan.Tables[0].Table != "orphan" {
		t.Fatalf("plan tables = %+v, want the orphan table planned despite no pipeline referencing it", plan.Tables)
	}
	if _, ok := plan.Tables[0].Branch.(pg.CreateFromHead); !ok {
		t.Errorf("orphan branch = %T, want CreateFromHead", plan.Tables[0].Branch)
	}
}
