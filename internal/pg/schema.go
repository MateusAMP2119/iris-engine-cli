package pg

import (
	"context"
	"fmt"
	"strings"
)

// This file holds the embedded data-journal schema: public.data_journal, the
// always-on write-capture table in the data database.
// pg owns the data database, so it owns this DDL, just as store owns the meta
// tables. pg keeps its own small schema model rather than importing store's: the
// two are peer database clients that never import each other (two clients, two
// databases, never crossed). data_journal is the single table pg models, so the
// model stays minimal.

// JournalSchema is the schema hosting the data journal: public, the readable
// surface every role may SELECT and none may write.
const JournalSchema = "public"

// JournalName is the data-journal table name.
const JournalName = "data_journal"

// Column is one column of the data-journal table's schema model. Identity marks
// the monotonic bigint identity ordering key (never a clock); Nullable drops the
// NOT NULL a required column otherwise carries.
type Column struct {
	// Name is the column identifier.
	Name string
	// Type is the rendered Postgres type.
	Type string
	// Identity marks a GENERATED ALWAYS AS IDENTITY monotonic bigint ordering key.
	Identity bool
	// Nullable drops the NOT NULL a required column otherwise carries.
	Nullable bool
}

// Check is a CHECK constraint pinning a column to a closed value set.
type Check struct {
	// Column is the constrained column.
	Column string
	// Values is the closed set of admissible values.
	Values []string
}

// Index is a secondary (non-primary-key) index over one or more columns.
type Index struct {
	// Name is the index identifier.
	Name string
	// Columns are the indexed columns, in order.
	Columns []string
}

// Table is the data-journal table's schema model.
type Table struct {
	// Schema is the hosting schema (public).
	Schema string
	// Name is the table identifier.
	Name string
	// Columns are the table's columns, in declaration order.
	Columns []Column
	// PrimaryKey is the ordered primary-key column set.
	PrimaryKey []string
	// Checks are the table's value-set CHECK constraints.
	Checks []Check
	// Indexes are the table's secondary indexes.
	Indexes []Index
	// Partition names the range partition key: the table is declared PARTITION BY
	// RANGE (<Partition>).
	Partition string
}

// Qualified returns the schema-qualified table name, e.g. "public.data_journal".
func (t Table) Qualified() string { return t.Schema + "." + t.Name }

// reservedIdents are column identifiers that must be double-quoted in DDL because
// they collide with SQL keywords (data_journal names schema and table columns).
var reservedIdents = map[string]bool{
	"schema": true,
	"table":  true,
}

// quoteIdent double-quotes an identifier that collides with a SQL keyword.
func quoteIdent(name string) string {
	if reservedIdents[name] {
		return `"` + name + `"`
	}
	return name
}

// quoteIdents quotes each identifier in a comma-joined list.
func quoteIdents(names []string) string {
	out := make([]string, len(names))
	for i, n := range names {
		out[i] = quoteIdent(n)
	}
	return strings.Join(out, ", ")
}

// quoteValues renders a CHECK value set as single-quoted SQL string literals.
func quoteValues(values []string) string {
	out := make([]string, len(values))
	for i, v := range values {
		out[i] = "'" + v + "'"
	}
	return strings.Join(out, ", ")
}

// renderColumn renders one column definition; a primary-key or identity column is
// implicitly NOT NULL, so the explicit NOT NULL rides only required columns.
func renderColumn(c Column, inPK bool) string {
	var b strings.Builder
	b.WriteString(quoteIdent(c.Name))
	b.WriteString(" ")
	b.WriteString(c.Type)
	if c.Identity {
		b.WriteString(" GENERATED ALWAYS AS IDENTITY")
	}
	if !c.Nullable && !inPK && !c.Identity {
		b.WriteString(" NOT NULL")
	}
	return b.String()
}

// CreateTableDDL renders the journal as a single partitioned CREATE TABLE IF NOT
// EXISTS statement. The journal carries no foreign keys: run_id links to runs
// logically only, never FK-enforced across the meta/data database boundary.
func (t Table) CreateTableDDL() string {
	pk := map[string]bool{}
	for _, c := range t.PrimaryKey {
		pk[c] = true
	}
	var lines []string
	for _, c := range t.Columns {
		lines = append(lines, "    "+renderColumn(c, pk[c.Name]))
	}
	if len(t.PrimaryKey) > 0 {
		lines = append(lines, "    PRIMARY KEY ("+quoteIdents(t.PrimaryKey)+")")
	}
	for _, ck := range t.Checks {
		lines = append(lines, fmt.Sprintf("    CHECK (%s IN (%s))", quoteIdent(ck.Column), quoteValues(ck.Values)))
	}
	suffix := ")"
	if t.Partition != "" {
		suffix = ") PARTITION BY RANGE (" + quoteIdent(t.Partition) + ")"
	}
	return fmt.Sprintf("CREATE TABLE IF NOT EXISTS %s (\n%s\n%s;", t.Qualified(), strings.Join(lines, ",\n"), suffix)
}

// IndexDDL renders the journal's secondary indexes as create-if-missing CREATE
// INDEX statements, in declaration order.
func (t Table) IndexDDL() []string {
	var out []string
	for _, idx := range t.Indexes {
		out = append(out, fmt.Sprintf("CREATE INDEX IF NOT EXISTS %s ON %s (%s);",
			idx.Name, t.Qualified(), quoteIdents(idx.Columns)))
	}
	return out
}

// DDL renders the journal's create-if-missing statements: the partitioned CREATE
// TABLE followed by its secondary indexes.
func (t Table) DDL() []string {
	return append([]string{t.CreateTableDDL()}, t.IndexDDL()...)
}

// JournalTable returns the data_journal table model: the always-on write-capture
// table in the data database's public schema,
// partitioned by id range with exactly two indexes (its primary key on the bigint
// identity id and the (schema, table, row_pk, run_id) provenance key). id is the
// monotonic ordering key; recorded_at is an opaque non-ordering text audit
// string; there is no post-image.
func JournalTable() Table {
	return Table{
		Schema: JournalSchema,
		Name:   JournalName,
		Columns: []Column{
			{Name: "id", Type: "bigint", Identity: true},
			{Name: "pg_role", Type: "text"},
			{Name: "run_id", Type: "bigint"},
			{Name: "schema", Type: "text"},
			{Name: "table", Type: "text"},
			{Name: "row_pk", Type: "text"},
			{Name: "op", Type: "text"},
			{Name: "pre_image", Type: "json", Nullable: true},
			{Name: "undo", Type: "text"},
			{Name: "recorded_at", Type: "text"},
		},
		PrimaryKey: []string{"id"},
		Checks: []Check{
			{Column: "op", Values: []string{"insert", "update", "delete"}},
			{Column: "undo", Values: []string{"open", "promoted", "wiped", "skipped"}},
		},
		Indexes: []Index{
			{Name: "data_journal_provenance_idx", Columns: []string{"schema", "table", "row_pk", "run_id"}},
		},
		Partition: "id",
	}
}

// JournalSelectGrantDDL renders the journal's read grant: GRANT SELECT on
// public.data_journal TO PUBLIC. public is the readable
// surface, so every engine role -- present and future -- may SELECT the journal,
// and no write privilege is ever granted here: writes reach the journal only
// through the capture triggers, which run as the table owner. Granting to PUBLIC
// rather than role-by-role is what makes "every role may SELECT, none may write" a
// standing property rather than a per-role reconcile. It is idempotent.
func JournalSelectGrantDDL() string {
	return "GRANT SELECT ON " + JournalTable().Qualified() + " TO PUBLIC;"
}

// JournalTeardownDDL renders the statements that drop the data journal in full:
// the engine uninstall teardown of the data database. It is a single cascading
// DROP TABLE, so the journal's partitions and any
// triggers or rules that depend on it go with it rather than being orphaned; the
// per-table capture triggers E03/E04 add to user tables extend this teardown when
// they land. IF EXISTS keeps the teardown idempotent when the journal was already
// dropped or never provisioned.
func JournalTeardownDDL() []string {
	return []string{"DROP TABLE IF EXISTS " + JournalTable().Qualified() + " CASCADE;"}
}

// EnsureJournal issues the embedded data_journal DDL create-if-missing through db,
// ensuring a fully writable and readable journal in one call: the partitioned
// CREATE TABLE IF NOT EXISTS public.data_journal and its provenance index, then its
// initial open tail partition (so the partitioned journal can accept writes -- a
// partitioned table with no partition rejects every insert), then the SELECT grant
// to PUBLIC (so every engine role may read it). Both provisioning paths -- engine
// install and declare apply -- end by calling this, so the journal a partition and
// grant would otherwise be a latent half-provisioned, unwritable table is never
// left behind. Like the meta schema it is applied at provisioning and re-checkable,
// idempotent (every statement is create-if-missing or an idempotent grant), with no
// ALTER or migration ledger.
func EnsureJournal(ctx context.Context, db DB) error {
	stmts := append(JournalTable().DDL(), InitialPartition().CreateDDL(), JournalSelectGrantDDL())
	for _, stmt := range stmts {
		if err := db.Exec(ctx, stmt); err != nil {
			return fmt.Errorf("pg: apply data_journal DDL: %w", err)
		}
	}
	return nil
}
