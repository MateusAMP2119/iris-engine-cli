package pg

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/MateusAMP2119/iris-engine-cli/internal/declare"
)

// This file is the pipeline-independent, idempotent schema provisioner: it walks
// the declared schemas/ tree and renders the
// data-database plan that materializes it. Provisioning is create-if-missing and
// takes exactly one path per table:
//
//   - a table absent from live Postgres is created from its table.yaml head in one
//     CREATE TABLE, and its ledger head is recorded as applied;
//   - a table already present replays its pending additive migrations (the
//     migration files on disk beyond the recorded applied head), never recreated.
//
// Both paths end ensuring capture: the partitioned journal exists once per data
// database and a capture trigger exists on every declared table. The whole thing
// is pipeline-independent -- the only table source is the schemas/ tree, never a
// pipeline's reads or writes -- and idempotent: a re-plan against an
// already-provisioned live view is empty (no statements, no heads).
//
// Planning is pure over three views: the declared schemas tree, the live-Postgres
// state, and the reconstructed migration ledger. Apply performs the I/O through
// the same two seams the E03.7 sync engine uses -- the pg.DB data-database client
// and the LedgerRecorder meta-write seam -- and never writes a migration file
// (provisioning replays the ledger; the sync engine is the sole file writer). The
// emitted DDL stream is deterministic (schemas then tables then capture, each in
// sorted order), so a golden diff is a contract diff.
//
// Head-recording definition: the ledger head recorded when a table is created
// from its head is the highest migration id present in the table's migrations/
// directory (with that migration's own parent and checksum), or "0001" -- the
// implicit create head whose revision is table.yaml itself -- when no migration
// files are present. The reconstruction is linear: the applied head is the single
// greatest recorded migration id, and everything at or below it is applied, so a
// create-from-head materializing the whole head records only that one head, while
// a replay records each pending migration it applies. Both leave the applied head
// at the highest present migration, so a re-plan is empty.

// createHeadID is the implicit create head's migration id: the head recorded for a
// table created from its table.yaml head when no migration files are on disk yet.
const createHeadID = "0001"

// LiveView is the data database's current physical state as provisioning reads it:
// which schemas and tables exist, whether each existing
// table's capture trigger is installed, and whether the partitioned journal
// exists. Provisioning consults it so a re-plan against an already-provisioned
// database is empty. E03.10 supplies the real view from an information_schema and
// pg_trigger read; the planner is pure over it.
type LiveView struct {
	// Schemas is the set of schema names that already exist (keys present => exists).
	Schemas map[string]bool
	// Tables is the set of "schema.table" keys that already exist.
	Tables map[string]bool
	// CaptureTriggers is the set of "schema.table" keys whose capture trigger is
	// already installed.
	CaptureTriggers map[string]bool
	// HasJournal reports whether the partitioned public.data_journal already exists.
	HasJournal bool
}

// hasSchema reports whether the named schema exists live.
func (v LiveView) hasSchema(schema string) bool { return v.Schemas[schema] }

// hasTable reports whether schema.table exists live.
func (v LiveView) hasTable(schema, table string) bool { return v.Tables[schema+"."+table] }

// hasCaptureTrigger reports whether schema.table's capture trigger is installed.
func (v LiveView) hasCaptureTrigger(schema, table string) bool {
	return v.CaptureTriggers[schema+"."+table]
}

// TableLedger is the reconstructed migration-ledger view of one declared table
// that provisioning diffs against live Postgres: the immutable migration files
// present on disk under the table's migrations/ directory and the highest migration
// id already recorded applied in the meta migrations table. E03.10 reconstructs it
// from the migrations/ files (LoadDiskMigrations) and a meta read; the planner is
// pure over it. The create-head checksum for a table with no migrations comes from
// the DECLARED table.yaml bytes (declare.DiscoveredTable.Raw), which are always
// present, not from this diff view, which may be absent for a brand-new table.
type TableLedger struct {
	// DiskMigrations are the migration files present on disk, one per additive
	// migration; order is normalized to ascending migration id by the planner.
	DiskMigrations []declare.MigrationFile
	// AppliedHeadID is the greatest migration id recorded applied in meta, or "" when
	// nothing is recorded yet.
	AppliedHeadID string
}

// TableBranch is the provisioning path taken for one declared table: a closed
// oneof with exactly two implementers, create-from-head XOR pending-migration
// replay. A table's branch is a single value of this type, so it can never hold
// both paths -- the "exactly one path per table" invariant is structural, not a
// runtime check.
type TableBranch interface {
	isTableBranch()
}

// CreateFromHead is the branch for a table absent from live Postgres: it renders
// one CREATE TABLE from the table.yaml head and records one ledger head as
// applied. It never replays a migration -- the CREATE materializes the whole head
// at once.
type CreateFromHead struct {
	// DDL is the CREATE TABLE statement rendered from the table.yaml head.
	DDL string
	// Head is the ledger head recorded as applied (the highest migration id present,
	// or the 0001 create head).
	Head MigrationHead
}

func (CreateFromHead) isTableBranch() {}

// ReplayPending is the branch for a table already present in live Postgres: it
// replays the pending additive migrations (the migration files beyond the recorded
// applied head), running each ADD COLUMN and recording each head. It never
// recreates the table. An empty Migrations slice means the table is already at its
// ledger head -- nothing pending.
type ReplayPending struct {
	// Migrations are the pending additive migrations to replay, in ascending id order.
	Migrations []PendingMigration
}

func (ReplayPending) isTableBranch() {}

// PendingMigration is one additive migration replayed against an existing table:
// the ADD COLUMN ALTER to run and the ledger head it advances to. Unlike an E03.7
// PlannedMigration it writes no migration file -- the file already exists on disk;
// provisioning only reconciles the live table and the applied head to it.
type PendingMigration struct {
	// Alter is the ADD COLUMN ALTER that applies the migration to the data database.
	Alter string
	// Head is the applied head this migration advances the ledger to.
	Head MigrationHead
}

// TableProvision is the provisioning work for one declared table: its identity,
// the one path taken (Branch), and the capture triggers to install (empty when the
// trigger is already present). A TableProvision is included in a plan only when it
// carries work -- a missing table, a pending migration, or an absent trigger -- so
// an already-provisioned table contributes nothing and the plan stays empty on a
// re-run.
type TableProvision struct {
	// Schema is the table's schema.
	Schema string
	// Table is the table's name.
	Table string
	// Branch is the single provisioning path (create-from-head XOR replay).
	Branch TableBranch
	// CaptureTriggers are the CREATE TRIGGER statements installing the table's
	// capture trigger, empty when it is already installed.
	CaptureTriggers []string
}

// ProvisionPlan is the deterministic data-database plan that materializes the
// declared schemas/ tree: the schemas to create, the
// per-table provisioning work, and whether the partitioned journal must be
// ensured. Apply performs the I/O; the plan itself is pure data, so a --dry-run is
// exactly the plan without Apply. An empty plan (Empty) means the database already
// reflects the declared world.
type ProvisionPlan struct {
	// Schemas are the schema names to CREATE SCHEMA IF NOT EXISTS, in sorted order,
	// missing schemas only.
	Schemas []string
	// Tables are the per-table provisioning work, in sorted (schema, table) order.
	Tables []TableProvision
	// EnsureJournal reports whether the partitioned journal must be ensured (it is
	// absent), ensured once per data database.
	EnsureJournal bool
}

// Empty reports whether the plan does nothing: no schemas to create, no journal to
// ensure, and no per-table work. A re-plan against an already-provisioned live view
// is empty, which is exactly the idempotency guarantee.
func (p ProvisionPlan) Empty() bool {
	return len(p.Schemas) == 0 && len(p.Tables) == 0 && !p.EnsureJournal
}

// PlanProvision renders the provisioning plan for the declared schemas/ tree
// against a live-Postgres view and the reconstructed migration ledger. It is
// pipeline-independent: the only table source is
// the schemas argument (the walked schemas/ tree), never a pipeline's reads or
// writes, so every declared table is planned regardless of who references it. It
// is idempotent: a schema, table, capture trigger, or journal already present in
// the live view contributes nothing, so a re-plan against a provisioned database
// is empty.
//
// Per table it takes exactly one path: a table absent from live is created from
// its head (and its ledger head recorded), a table present replays its pending
// additive migrations -- never both. Both paths end ensuring capture. ledgers maps
// each "schema.table" to its reconstructed ledger; a table missing an entry is
// treated as having no migration files and no applied head (its create head is the
// 0001 create head). A declared column or migration whose YAML type is outside the
// closed set returns an error rather than emitting invalid DDL.
func PlanProvision(schemas []declare.DiscoveredTable, live LiveView, ledgers map[string]TableLedger) (ProvisionPlan, error) {
	var plan ProvisionPlan

	// CREATE SCHEMA IF NOT EXISTS for each distinct declared schema not already
	// present, in sorted order. Multiple tables share a schema, so schemas are
	// deduplicated before the liveness filter.
	seen := make(map[string]bool, len(schemas))
	var schemaNames []string
	for _, dt := range schemas {
		if seen[dt.Schema] {
			continue
		}
		seen[dt.Schema] = true
		if !live.hasSchema(dt.Schema) {
			schemaNames = append(schemaNames, dt.Schema)
		}
	}
	sort.Strings(schemaNames)
	plan.Schemas = schemaNames

	// Per-table work, in sorted (schema, table) order for a deterministic plan. A
	// copy is sorted so the caller's slice order does not leak into the plan.
	sorted := append([]declare.DiscoveredTable(nil), schemas...)
	sort.Slice(sorted, func(i, j int) bool {
		if sorted[i].Schema != sorted[j].Schema {
			return sorted[i].Schema < sorted[j].Schema
		}
		return sorted[i].Table < sorted[j].Table
	})

	for _, dt := range sorted {
		if dt.Spec == nil {
			return ProvisionPlan{}, fmt.Errorf("pg: provision %s.%s: nil table spec", dt.Schema, dt.Table)
		}
		key := dt.Schema + "." + dt.Table
		led := ledgers[key]
		exists := live.hasTable(dt.Schema, dt.Table)

		branch, err := SelectTableBranch(exists, dt.Spec, dt.Raw, led)
		if err != nil {
			return ProvisionPlan{}, err
		}

		var triggers []string
		if !live.hasCaptureTrigger(dt.Schema, dt.Table) {
			triggers = RenderCaptureTriggers(dt.Schema, dt.Table)
		}

		// Omit a table that carries no work: an existing table with nothing pending
		// and its trigger already installed. A missing table always carries its
		// CREATE, so it is never omitted.
		if exists && emptyReplay(branch) && len(triggers) == 0 {
			continue
		}
		plan.Tables = append(plan.Tables, TableProvision{
			Schema: dt.Schema, Table: dt.Table, Branch: branch, CaptureTriggers: triggers,
		})
	}

	plan.EnsureJournal = !live.HasJournal
	return plan, nil
}

// emptyReplay reports whether a branch is a replay with no pending migrations (an
// existing table already at its ledger head).
func emptyReplay(b TableBranch) bool {
	r, ok := b.(ReplayPending)
	return ok && len(r.Migrations) == 0
}

// SelectTableBranch chooses a declared table's provisioning path from a single
// predicate -- whether it exists in live Postgres -- and returns exactly one
// branch: create-from-head when absent, pending-migration replay when present.
// The two are mutually exclusive by construction: the
// return is one TableBranch value, so a table can never take both paths. declared
// is the table.yaml head and raw its verbatim bytes (the create head's checksum
// source); led is its reconstructed ledger.
func SelectTableBranch(exists bool, declared *declare.Table, raw []byte, led TableLedger) (TableBranch, error) {
	if declared == nil {
		return nil, errors.New("pg: provision: nil declared table")
	}
	if !exists {
		return createFromHead(declared, raw, led)
	}
	return replayPending(declared, led)
}

// createFromHead renders the create-from-head branch: one CREATE TABLE from the
// declared head and the ledger head to record. The head is the highest migration
// id present on disk (with its own parent and checksum), or the 0001 create head
// when no migration files exist. The 0001 head's checksum is taken from the
// DECLARED table.yaml bytes (raw) -- the authoritative, always-present source --
// so an absent reconstructed ledger never yields a checksum over nil bytes; empty
// raw is an error rather than a permanently wrong head. A malformed migration id
// on disk is a corrupt ledger and surfaces as an error, consistent with the replay
// path, never a silent fallthrough to the 0001 head.
func createFromHead(declared *declare.Table, raw []byte, led TableLedger) (TableBranch, error) {
	ddl, err := RenderCreateTable(declared)
	if err != nil {
		return nil, err
	}
	head := MigrationHead{Schema: declared.Schema, Table: declared.Table}
	top, ok, err := highestMigration(led.DiskMigrations)
	if err != nil {
		return nil, fmt.Errorf("pg: provision %s.%s: %w", declared.Schema, declared.Table, err)
	}
	if ok {
		head.MigrationID = top.ID
		head.Parent = top.Parent
		head.Checksum = top.Checksum
	} else {
		if len(raw) == 0 {
			return nil, fmt.Errorf("pg: provision %s.%s: create head checksum needs the declared table.yaml bytes, got none", declared.Schema, declared.Table)
		}
		head.MigrationID = createHeadID
		head.Parent = ""
		head.Checksum = declare.ChecksumTableYAML(raw)
	}
	return CreateFromHead{DDL: ddl, Head: head}, nil
}

// replayPending renders the pending-migration replay branch: the migration files
// on disk whose id is beyond the recorded applied head, each rendered as its ADD
// COLUMN ALTER and the head it advances to. A non-additive (non add_column)
// migration in the ledger is refused rather than replayed.
func replayPending(declared *declare.Table, led TableLedger) (TableBranch, error) {
	appliedSeq, err := parseSeq(led.AppliedHeadID)
	if err != nil {
		return nil, fmt.Errorf("pg: provision %s.%s: %w", declared.Schema, declared.Table, err)
	}

	migs := sortedMigrations(led.DiskMigrations)
	var pending []PendingMigration
	for _, m := range migs {
		seq, err := parseSeq(m.ID)
		if err != nil {
			return nil, fmt.Errorf("pg: provision %s.%s: %w", declared.Schema, declared.Table, err)
		}
		if seq <= appliedSeq {
			continue // at or below the applied head: already applied.
		}
		if m.Op != "add_column" {
			return nil, fmt.Errorf("pg: provision %s.%s: migration %s op %q is non-additive; provisioning replays only add_column",
				declared.Schema, declared.Table, m.ID, m.Op)
		}
		alter, err := RenderAddColumn(declared.Schema, declared.Table, m.Column)
		if err != nil {
			return nil, err
		}
		pending = append(pending, PendingMigration{
			Alter: alter,
			Head: MigrationHead{
				Schema: declared.Schema, Table: declared.Table,
				MigrationID: m.ID, Parent: m.Parent, Checksum: m.Checksum,
			},
		})
	}
	return ReplayPending{Migrations: pending}, nil
}

// highestMigration returns the migration file with the greatest id, ok=false when
// the slice is empty. It is order-independent: it compares parsed sequences, so an
// unsorted disk list still yields the true head. A malformed (non-numeric) id is a
// corrupt ledger and returns an error rather than being skipped -- the same
// treatment replayPending gives the same input -- so the create path can never
// silently drop a corrupt migration and fall through to the implicit 0001 head.
func highestMigration(migs []declare.MigrationFile) (declare.MigrationFile, bool, error) {
	var top declare.MigrationFile
	var topSeq int
	found := false
	for _, m := range migs {
		seq, err := parseSeq(m.ID)
		if err != nil {
			return declare.MigrationFile{}, false, err
		}
		if !found || seq > topSeq {
			top, topSeq, found = m, seq, true
		}
	}
	return top, found, nil
}

// sortedMigrations returns a copy of migs in ascending id order, so replay runs
// migrations in ledger order regardless of directory-read order. A non-numeric id
// sorts to the front and is surfaced as an error by the caller.
func sortedMigrations(migs []declare.MigrationFile) []declare.MigrationFile {
	out := append([]declare.MigrationFile(nil), migs...)
	sort.SliceStable(out, func(i, j int) bool {
		si, _ := parseSeq(out[i].ID)
		sj, _ := parseSeq(out[j].ID)
		return si < sj
	})
	return out
}

// Apply executes the plan through two seams: the pg.DB data-database client (db)
// for every CREATE / ALTER / trigger statement and the LedgerRecorder meta-write
// seam (rec) for every applied head. It writes no migration file: provisioning
// replays the ledger, it never extends it. The order is deterministic -- schemas,
// then per-table create/replay, then capture (the journal once, then per-table
// triggers) -- so "both paths end ensuring capture" holds and the emitted stream
// is a golden.
//
// Within create-from-head the CREATE TABLE runs before the head is recorded (the
// durable data change first, then the meta record), matching the E03.7 sync
// engine's order; the two databases cannot share a transaction, so a failure
// between them is reconciled by re-provisioning, which is idempotent. Every step's
// error is wrapped and returned immediately -- no error is swallowed -- and the
// plan is immutable, so it is safely re-appliable once a fault is cleared.
func (p ProvisionPlan) Apply(ctx context.Context, db DB, rec LedgerRecorder) error {
	for _, schema := range p.Schemas {
		if err := db.Exec(ctx, createSchemaSQL(schema)); err != nil {
			return fmt.Errorf("pg: provision create schema %s: %w", schema, err)
		}
	}

	for _, tp := range p.Tables {
		switch b := tp.Branch.(type) {
		case CreateFromHead:
			if err := db.Exec(ctx, b.DDL); err != nil {
				return fmt.Errorf("pg: provision create table %s.%s: %w", tp.Schema, tp.Table, err)
			}
			if err := rec.RecordMigrationHead(ctx, b.Head); err != nil {
				return fmt.Errorf("pg: provision record create head %s.%s %s: %w", tp.Schema, tp.Table, b.Head.MigrationID, err)
			}
		case ReplayPending:
			for _, m := range b.Migrations {
				if err := db.Exec(ctx, m.Alter); err != nil {
					return fmt.Errorf("pg: provision replay ALTER %s.%s %s: %w", tp.Schema, tp.Table, m.Head.MigrationID, err)
				}
				if err := rec.RecordMigrationHead(ctx, m.Head); err != nil {
					return fmt.Errorf("pg: provision record replay head %s.%s %s: %w", tp.Schema, tp.Table, m.Head.MigrationID, err)
				}
			}
		default:
			return fmt.Errorf("pg: provision %s.%s: unknown branch %T", tp.Schema, tp.Table, tp.Branch)
		}
	}

	// Capture: the partitioned journal once per data database, then a capture trigger
	// on every declared table lacking one.
	if p.EnsureJournal {
		if err := EnsureJournal(ctx, db); err != nil {
			return fmt.Errorf("pg: provision ensure journal: %w", err)
		}
	}
	for _, tp := range p.Tables {
		for _, trig := range tp.CaptureTriggers {
			if err := db.Exec(ctx, trig); err != nil {
				return fmt.Errorf("pg: provision install capture trigger %s.%s: %w", tp.Schema, tp.Table, err)
			}
		}
	}
	return nil
}

// createSchemaSQL renders the create-if-missing CREATE SCHEMA statement for a
// declared schema folder, its name double-quoted like the rest of pg's DDL so a
// reserved word or a name with a quote is a safe identifier. IF NOT EXISTS keeps
// it idempotent when the plan is applied against a database that already has the
// schema (a partial-provision re-run).
func createSchemaSQL(schema string) string {
	return "CREATE SCHEMA IF NOT EXISTS " + quoteIdentifier(schema) + ";"
}

// LoadDiskMigrations reads a table folder's migrations/ directory and returns its
// migration files in ascending id order: the on-disk half of the ledger
// reconstruction provisioning diffs against the recorded applied head. An absent
// directory yields no migrations and no error (a freshly declared table with no
// migration ledger yet). Hidden entries and non-.yaml files are skipped; a
// malformed migration file is an error.
func LoadDiskMigrations(migrationsDir string) ([]declare.MigrationFile, error) {
	entries, err := os.ReadDir(migrationsDir)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("pg: read migrations dir %s: %w", migrationsDir, err)
	}

	var migs []declare.MigrationFile
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || strings.HasPrefix(name, ".") || filepath.Ext(name) != ".yaml" {
			continue
		}
		data, err := os.ReadFile(filepath.Join(migrationsDir, name)) //nolint:gosec // G304: migrationsDir is an engine-owned workspace path, not user or network input.
		if err != nil {
			return nil, fmt.Errorf("pg: read migration %s: %w", name, err)
		}
		m, err := declare.ParseMigration(data)
		if err != nil {
			return nil, fmt.Errorf("pg: parse migration %s: %w", name, err)
		}
		migs = append(migs, m)
	}
	return sortedMigrations(migs), nil
}
