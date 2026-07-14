package pg

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/MateusAMP2119/iris-engine-cli/internal/declare"
)

// This file is the additive-only migration sync engine: the execution half of the
// migration machinery whose classification lives in the
// declare leaf. It diffs a declared table.yaml against its migration-ledger head
// and its live-Postgres head, and renders an additive-only plan -- the immutable
// migration files to append, the ADD COLUMN and capture-trigger DDL to run, and the
// migrations-ledger heads to record. Planning is pure: every artifact is rendered
// as data and nothing is touched, so --dry-run prints the plan and never calls
// Apply. Apply performs the I/O through three seams: a filesystem sink for the
// migration files, the pg.DB data-database seam for the DDL, and a LedgerRecorder
// for the applied heads.
//
// The engine never imports store: the applied-head record is written through the
// LedgerRecorder seam (store.Writer.RecordMigrationHead satisfies it, wired by the
// leader's apply pipeline), keeping the two database clients uncrossed (store owns
// meta, pg owns data).

// migrationsDirName is the engine-written migration ledger directory under a table
// folder (schemas/<schema>/<table>/migrations/).
const migrationsDirName = "migrations"

// LedgerView is the migration-ledger head of one declared table the sync engine
// diffs table.yaml against: the head migration id (the greatest applied
// migration_id, e.g. "0001") that seeds the next migration's sequence and parent,
// and the reconstructed ledger columns the additive diff runs over. The sync engine
// is pure over it -- it reads no file and no database to build one. The seam has no
// production supplier: provisioning (provision.go) reconstructs the same two facts
// from the migrations/ files and the meta migrations table, but into its own
// TableLedger view, and it is provisioning that the daemon's apply path drives; a
// LedgerView is assembled only by the sync engine's own tests.
type LedgerView struct {
	// HeadID is the current ledger head's zero-padded migration id (e.g. "0001").
	// An empty head is treated as sequence zero, so the first generated migration is
	// 0001; in practice provisioning records a 0001_create head before any sync.
	HeadID string
	// State is the reconstructed ledger-head column set.
	State declare.LedgerState
}

// MigrationHead is one applied-migration ledger row the sync engine records through
// the LedgerRecorder seam: the (schema, table, migration_id) key, the parent id,
// and the checksum of table.yaml at that revision. It
// mirrors store.MigrationHead field-for-field without importing store (two clients,
// two databases, never crossed); the apply pipeline adapts one to the other.
type MigrationHead struct {
	// Schema is the declared table's schema.
	Schema string
	// Table is the declared table's name.
	Table string
	// MigrationID is the zero-padded id of the migration recorded as applied.
	MigrationID string
	// Parent is the predecessor migration id (the prior head).
	Parent string
	// Checksum is the checksum of table.yaml at this revision.
	Checksum string
}

// PlannedMigration is one additive ledger delta the sync engine generates: the
// immutable migration file to append (Filename, Contents), the ADD COLUMN ALTER to
// run (Alter), and the applied head to record (Head). All three are rendered as
// data; Apply performs the I/O.
type PlannedMigration struct {
	// Head is the applied-migration ledger row this delta records.
	Head MigrationHead
	// Filename is the migration file's name (e.g. "0002_add_status.yaml").
	Filename string
	// Contents is the immutable migration file's canonical YAML bytes.
	Contents []byte
	// Alter is the ADD COLUMN ALTER that applies the delta to the data database.
	Alter string
}

// SchemaFix is one additive schema-drift autofix: a rendered ADD COLUMN (a declared
// column missing from live Postgres) or CREATE TRIGGER (a missing capture trigger),
// tagged with the subject it repairs. Unlike a PlannedMigration it writes no
// migration file and records no head: it reconciles live Postgres to the
// already-declared head additively, it does not extend the ledger.
type SchemaFix struct {
	// Subject is the drift subject repaired (column or capture_trigger).
	Subject declare.DriftSubject
	// DDL is the additive statement that repairs it.
	DDL string
}

// Plan is the additive-only sync plan for one declared table: the ledger migrations
// to append and apply (Migrations) and the schema-drift autofixes to run (Fixes).
// An empty plan means the table is already at its ledger head and live Postgres
// matches the declared head -- nothing to do.
type Plan struct {
	// Schema is the declared table's schema.
	Schema string
	// Table is the declared table's name.
	Table string
	// Migrations are the additive ledger deltas, in ledger order (each parent is the
	// prior migration's id).
	Migrations []PlannedMigration
	// Fixes are the additive schema-drift autofixes, in classification order.
	Fixes []SchemaFix
}

// LedgerRecorder is the meta-write seam the sync engine records applied migration
// heads through. store.Writer.RecordMigrationHead satisfies it; the sync engine
// never constructs it and never imports store, so the single-writer meta path is
// preserved -- the leader's apply pipeline wires the real writer.
type LedgerRecorder interface {
	// RecordMigrationHead durably records a table's applied migration head.
	RecordMigrationHead(ctx context.Context, head MigrationHead) error
}

// MigrationSink is the filesystem seam the sync engine appends immutable migration
// files through. Implementations must never overwrite an existing file (the ledger
// is immutable): a name collision is an error, not a silent rewrite.
type MigrationSink interface {
	// AppendMigration writes data as the immutable migration file named filename in
	// the migrations/ directory of the (schema, table) folder, never overwriting an
	// existing file.
	AppendMigration(schema, table, filename string, data []byte) error
}

// PlanLedgerSync diffs a declared table.yaml (the desired head) against its
// migration-ledger head and returns the additive-only migration plan.
// Each column present in the declared head but beyond the ledger head is
// an additive delta: the next numbered migration file, its ADD COLUMN ALTER, and the
// applied head that advances the ledger. Deltas are numbered in declared-column
// order from HeadID, each chained to the prior as its parent. A non-additive ledger
// change -- a column removed from table.yaml, or a retype -- refuses apply and
// returns an error, never a migration that drops or rewrites a column. raw is the
// exact table.yaml bytes at this revision; its checksum pins every generated
// migration to the revision it was cut from.
func PlanLedgerSync(declared *declare.Table, raw []byte, ledger LedgerView) (Plan, error) {
	report, err := declare.ClassifyLedgerDrift(declared, ledger.State)
	if err != nil {
		return Plan{}, fmt.Errorf("pg: ledger sync: %w", err)
	}
	if report.Refused() {
		return Plan{}, fmt.Errorf("pg: ledger sync %s.%s refused (non-additive): %s",
			declared.Schema, declared.Table, refusedDetail(report))
	}

	qualified := declared.Schema + "." + declared.Table
	// The additive columns are exactly the ledger-domain column drifts the classifier
	// marks autofix (declared columns beyond the ledger head); the classifier is the
	// single source of the additive/refuse decision.
	additive := make(map[string]bool)
	for _, d := range report.Autofixes() {
		if d.Domain == declare.DomainLedger && d.Subject == declare.SubjectColumn {
			additive[strings.TrimPrefix(d.Name, qualified+".")] = true
		}
	}

	seq, err := parseSeq(ledger.HeadID)
	if err != nil {
		return Plan{}, err
	}
	checksum := declare.ChecksumTableYAML(raw)
	parent := ledger.HeadID

	plan := Plan{Schema: declared.Schema, Table: declared.Table}
	for _, col := range declared.Columns {
		if !additive[col.Name] {
			continue
		}
		seq++
		id := formatSeq(seq)
		mcol := declare.MigrationColumn{Name: col.Name, Type: col.Type, Default: col.Default}
		alter, err := RenderAddColumn(declared.Schema, declared.Table, mcol)
		if err != nil {
			return Plan{}, fmt.Errorf("pg: ledger sync %s: %w", qualified, err)
		}
		contents, err := declare.MarshalMigration(declare.MigrationFile{
			ID: id, Parent: parent, Op: "add_column", Column: mcol, Checksum: checksum,
		})
		if err != nil {
			return Plan{}, fmt.Errorf("pg: ledger sync %s: %w", qualified, err)
		}
		plan.Migrations = append(plan.Migrations, PlannedMigration{
			Head: MigrationHead{
				Schema: declared.Schema, Table: declared.Table,
				MigrationID: id, Parent: parent, Checksum: checksum,
			},
			Filename: migrationFilename(id, col.Name),
			Contents: contents,
			Alter:    alter,
		})
		parent = id
	}
	return plan, nil
}

// PlanSchemaFix diffs a declared table.yaml against its live-Postgres head and
// returns the additive-only schema-drift autofix plan. A
// declared column missing from live is auto-fixed with ADD COLUMN; a missing capture
// trigger is auto-fixed with CREATE TRIGGER, additively, like a missing column.
// Every other discrepancy -- an extra, renamed, or retyped column -- is non-additive
// and refuses apply, returning an error rather than dropping or rewriting the
// object. The plan carries no migrations and records no heads: a schema-drift
// autofix reconciles live Postgres to the already-declared head, it does not extend
// the ledger.
func PlanSchemaFix(declared *declare.Table, live declare.LiveTable) (Plan, error) {
	report, err := declare.ClassifySchemaDrift(declared, live)
	if err != nil {
		return Plan{}, fmt.Errorf("pg: schema fix: %w", err)
	}
	if report.Refused() {
		return Plan{}, fmt.Errorf("pg: schema fix %s.%s refused (non-additive): %s",
			declared.Schema, declared.Table, refusedDetail(report))
	}

	qualified := declared.Schema + "." + declared.Table
	declaredByName := make(map[string]declare.Column, len(declared.Columns))
	for _, c := range declared.Columns {
		declaredByName[c.Name] = c
	}

	plan := Plan{Schema: declared.Schema, Table: declared.Table}
	for _, d := range report.Autofixes() {
		if d.Domain != declare.DomainSchema {
			continue
		}
		switch d.Subject {
		case declare.SubjectColumn:
			name := strings.TrimPrefix(d.Name, qualified+".")
			col, ok := declaredByName[name]
			if !ok {
				return Plan{}, fmt.Errorf("pg: schema fix %s: additive column %q is not in the declared head", qualified, name)
			}
			ddl, err := RenderAddColumn(declared.Schema, declared.Table,
				declare.MigrationColumn{Name: col.Name, Type: col.Type, Default: col.Default})
			if err != nil {
				return Plan{}, fmt.Errorf("pg: schema fix %s: %w", qualified, err)
			}
			plan.Fixes = append(plan.Fixes, SchemaFix{Subject: declare.SubjectColumn, DDL: ddl})
		case declare.SubjectCaptureTrigger:
			// A missing capture trigger installs the complete per-operation trigger
			// set: Postgres transition tables are per-operation, so one classified
			// missing-trigger drift maps to three additive CREATE TRIGGER fixes
			// (insert/update/delete), each a separate statement.
			for _, ddl := range RenderCaptureTriggers(declared.Schema, declared.Table) {
				plan.Fixes = append(plan.Fixes, SchemaFix{Subject: declare.SubjectCaptureTrigger, DDL: ddl})
			}
		}
	}
	return plan, nil
}

// Apply executes the plan through its three seams: it appends each immutable
// migration file (sink), runs its ADD COLUMN ALTER (db), and records its applied
// head (rec), then runs each schema-drift autofix (db). Per migration the order is
// file, ALTER, head: the file is the durable disk ledger and the recorded head is
// the applied head, so a failure after the file is written leaves the file with the
// applied head lagging -- the ledger-versus-disk drift provisioning reconciles --
// rather than a silent gap. Every step's error is wrapped and returned immediately;
// no error is swallowed, and a partial apply leaves no in-memory state to detonate
// later (the plan is immutable and re-appliable once the fault is cleared).
func (p Plan) Apply(ctx context.Context, sink MigrationSink, db DB, rec LedgerRecorder) error {
	for _, m := range p.Migrations {
		if err := sink.AppendMigration(p.Schema, p.Table, m.Filename, m.Contents); err != nil {
			return fmt.Errorf("pg: apply migration %s.%s %s: %w", p.Schema, p.Table, m.Filename, err)
		}
		if err := db.Exec(ctx, m.Alter); err != nil {
			return fmt.Errorf("pg: apply migration ALTER %s.%s %s: %w", p.Schema, p.Table, m.Head.MigrationID, err)
		}
		if err := rec.RecordMigrationHead(ctx, m.Head); err != nil {
			return fmt.Errorf("pg: record migration head %s.%s %s: %w", p.Schema, p.Table, m.Head.MigrationID, err)
		}
	}
	for _, f := range p.Fixes {
		if err := db.Exec(ctx, f.DDL); err != nil {
			return fmt.Errorf("pg: apply schema autofix (%s) %s.%s: %w", f.Subject, p.Schema, p.Table, err)
		}
	}
	return nil
}

// Preview renders the plan as --dry-run text: the
// intended ALTERs and migration files, and the schema-drift autofix DDL. It invokes
// no executor and is pure over the plan, so --dry-run is exactly Preview with Apply
// never called. The output is deterministic, so a golden diff is a contract diff.
func (p Plan) Preview() []byte {
	var b strings.Builder
	for _, m := range p.Migrations {
		path := migrationDisplayPath(p.Schema, p.Table, m.Filename)
		fmt.Fprintf(&b, "-- migration: %s\n", path)
		b.WriteString(m.Alter)
		b.WriteString("\n\n")
		fmt.Fprintf(&b, "# %s\n", path)
		b.Write(m.Contents)
		b.WriteByte('\n')
	}
	for _, f := range p.Fixes {
		fmt.Fprintf(&b, "-- autofix (%s):\n", f.Subject)
		b.WriteString(f.DDL)
		b.WriteString("\n\n")
	}
	return []byte(b.String())
}

// DirMigrationSink is the filesystem MigrationSink rooted at a schemas/ directory:
// it appends migration files under <root>/<schema>/<table>/migrations/. Appends are
// atomic and create-once (temp-write then link), so a migration file appears with
// complete contents and an existing ledger file is never rewritten.
type DirMigrationSink struct {
	root string
}

// NewDirMigrationSink returns a filesystem migration sink rooted at schemasRoot (the
// schemas/ directory whose table folders hold the migration ledgers).
func NewDirMigrationSink(schemasRoot string) *DirMigrationSink {
	return &DirMigrationSink{root: schemasRoot}
}

// Root returns the schemas/ directory the sink writes under.
func (s *DirMigrationSink) Root() string { return s.root }

// AppendMigration writes data as the immutable migration file filename under the
// (schema, table) folder's migrations/ directory. It writes a hidden temp file in
// the destination directory, gives it 0644, then hard-links it into place: the link
// makes the final name appear atomically with complete contents and fails with an
// error when the target already exists, so the ledger is never rewritten (create-
// once, with no TOCTOU stat). The migrations/ directory is created 0755 if absent;
// the temp file is always removed.
func (s *DirMigrationSink) AppendMigration(schema, table, filename string, data []byte) error {
	dir := filepath.Join(s.root, schema, table, migrationsDirName)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return fmt.Errorf("pg: create migrations dir %s: %w", dir, err)
	}
	tmp, err := os.CreateTemp(dir, "."+filename+".tmp-*")
	if err != nil {
		return fmt.Errorf("pg: create temp migration in %s: %w", dir, err)
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName) // no-op after a successful link removes it below.

	if _, err := tmp.Write(data); err != nil {
		return errors.Join(fmt.Errorf("pg: write temp migration %s: %w", tmpName, err), tmp.Close())
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("pg: close temp migration %s: %w", tmpName, err)
	}
	if err := os.Chmod(tmpName, 0o644); err != nil {
		return fmt.Errorf("pg: chmod temp migration %s: %w", tmpName, err)
	}
	target := filepath.Join(dir, filename)
	if err := os.Link(tmpName, target); err != nil {
		// Distinguish the create-once collision (the ledger is immutable) from a
		// genuine I/O failure (e.g. a full disk): mislabeling the latter as a replay
		// would send the caller hunting a nonexistent duplicate migration.
		if errors.Is(err, os.ErrExist) {
			return fmt.Errorf("pg: append migration %s: already exists, the ledger is immutable (create-once): %w", target, err)
		}
		return fmt.Errorf("pg: append migration %s: %w", target, err)
	}
	return nil
}

// migrationFilename is a migration file's name: the zero-padded id, "_add_", the
// added column name, and the .yaml extension (e.g. "0002_add_status.yaml").
func migrationFilename(id, column string) string {
	return id + "_add_" + column + ".yaml"
}

// migrationDisplayPath is a migration file's repo-relative display path for the
// --dry-run preview, always forward-slashed so the golden is stable across
// operating systems.
func migrationDisplayPath(schema, table, filename string) string {
	return fmt.Sprintf("schemas/%s/%s/%s/%s", schema, table, migrationsDirName, filename)
}

// parseSeq parses a zero-padded migration id into its integer sequence; an empty id
// is sequence zero (no ledger head yet). A non-numeric id is a corrupt ledger head
// and returns an error.
func parseSeq(id string) (int, error) {
	id = strings.TrimSpace(id)
	if id == "" {
		return 0, nil
	}
	n, err := strconv.Atoi(id)
	if err != nil {
		return 0, fmt.Errorf("pg: ledger head id %q is not a number: %w", id, err)
	}
	if n < 0 {
		return 0, fmt.Errorf("pg: ledger head id %q is negative (corrupt ledger)", id)
	}
	return n, nil
}

// formatSeq renders an integer sequence as a zero-padded four-digit migration id.
func formatSeq(seq int) string {
	return fmt.Sprintf("%04d", seq)
}

// refusedDetail joins the names of a report's refusing drifts for a refusal error.
func refusedDetail(r declare.DriftReport) string {
	var names []string
	for _, d := range r.Drifts {
		if d.Action == declare.ActionRefuse {
			names = append(names, d.Name)
		}
	}
	return strings.Join(names, ", ")
}
