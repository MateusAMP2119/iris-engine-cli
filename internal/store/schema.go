package store

import (
	"fmt"
	"strings"
)

// This file holds the embedded meta schema: the seventeen control tables of the
// dedicated meta database (specification section 4), modeled as Go data that
// renders deterministically to create-if-missing DDL and is directly assertable
// for the roster, foreign-key graph, and identity-ordering contracts. The model
// is the single source; the rendered DDL and the extracted FK graph are both
// derived from it, so a golden diff is a contract diff.

// MetaDatabase is the fixed name of the dedicated meta control-plane database
// (specification section 4: "control state in meta"). It is created in the same
// cluster as the data database at bootstrap.
const MetaDatabase = "meta"

// Column is one column in an engine table's schema model. Identity marks a
// monotonic bigint identity ordering key (never a clock); Nullable drops the
// NOT NULL that non-key columns otherwise carry.
type Column struct {
	// Name is the column identifier.
	Name string
	// Type is the rendered Postgres type (e.g. "text", "bigint", "json").
	Type string
	// Identity marks a GENERATED ALWAYS AS IDENTITY monotonic bigint ordering key.
	Identity bool
	// Nullable drops the NOT NULL a required non-key column otherwise carries.
	Nullable bool
}

// ForeignKey is one foreign-key edge from a table column to a referenced table's
// column: the child.Column references RefTable.RefColumn.
type ForeignKey struct {
	// Column is the referencing (child) column.
	Column string
	// RefTable is the referenced (parent) table.
	RefTable string
	// RefColumn is the referenced (parent) column.
	RefColumn string
}

// Check is a CHECK constraint pinning a column to a closed value set.
type Check struct {
	// Column is the constrained column.
	Column string
	// Values is the closed set of admissible values.
	Values []string
}

// Unique is a UNIQUE constraint over one or more columns.
type Unique struct {
	// Columns are the columns the uniqueness spans.
	Columns []string
}

// Index is a secondary (non-primary-key) index over one or more columns.
type Index struct {
	// Name is the index identifier.
	Name string
	// Columns are the indexed columns, in order.
	Columns []string
}

// Table is one engine table's schema model: its columns, primary key, and the
// constraints and indexes the create-if-missing DDL renders.
type Table struct {
	// Name is the table identifier.
	Name string
	// Columns are the table's columns, in declaration order.
	Columns []Column
	// PrimaryKey is the ordered primary-key column set.
	PrimaryKey []string
	// ForeignKeys are the table's foreign-key edges, in declaration order.
	ForeignKeys []ForeignKey
	// Uniques are the table's UNIQUE constraints.
	Uniques []Unique
	// Checks are the table's value-set CHECK constraints.
	Checks []Check
	// RawChecks are free-form CHECK expressions the value-set form cannot express.
	RawChecks []string
	// Indexes are the table's secondary indexes.
	Indexes []Index
	// Partition, when non-empty, names the range partition key: the table is
	// declared PARTITION BY RANGE (<Partition>).
	Partition string
}

// Schema is a database's ordered table set.
type Schema struct {
	// Database is the database the tables live in.
	Database string
	// Tables are the schema's tables in spec roster order (runs precedes
	// artifacts), the order the roster assertion pins. It is NOT a safe emission
	// order: iterating it to issue DDL forward-references artifacts from runs and
	// fails against real Postgres. Call DDL() for FK-safe (topologically ordered)
	// create-if-missing emission.
	Tables []Table
}

// reservedIdents are the column identifiers that must be double-quoted in DDL
// because they collide with SQL keywords (specification section 4 names schema
// and table columns on grants, migrations, and data_journal).
var reservedIdents = map[string]bool{
	"schema": true,
	"table":  true,
}

// quoteIdent double-quotes an identifier that collides with a SQL keyword, and
// leaves ordinary identifiers bare for readable DDL.
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

// quoteValues renders a CHECK value set as a comma-joined list of single-quoted
// SQL string literals.
func quoteValues(values []string) string {
	out := make([]string, len(values))
	for i, v := range values {
		out[i] = "'" + v + "'"
	}
	return strings.Join(out, ", ")
}

// renderColumn renders one column definition. A column in the primary key or one
// declared IDENTITY is implicitly NOT NULL, so the explicit NOT NULL is emitted
// only for required non-key columns.
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

// CreateTableDDL renders t as a single CREATE TABLE IF NOT EXISTS statement: the
// column definitions followed by the primary key, unique, foreign-key, and check
// constraints, with a trailing PARTITION BY RANGE clause when t is partitioned.
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
	for _, u := range t.Uniques {
		lines = append(lines, "    UNIQUE ("+quoteIdents(u.Columns)+")")
	}
	for _, fk := range t.ForeignKeys {
		lines = append(lines, fmt.Sprintf("    FOREIGN KEY (%s) REFERENCES %s (%s)",
			quoteIdent(fk.Column), fk.RefTable, quoteIdent(fk.RefColumn)))
	}
	for _, ck := range t.Checks {
		lines = append(lines, fmt.Sprintf("    CHECK (%s IN (%s))", quoteIdent(ck.Column), quoteValues(ck.Values)))
	}
	for _, expr := range t.RawChecks {
		lines = append(lines, "    CHECK ("+expr+")")
	}

	suffix := ")"
	if t.Partition != "" {
		suffix = ") PARTITION BY RANGE (" + quoteIdent(t.Partition) + ")"
	}
	return fmt.Sprintf("CREATE TABLE IF NOT EXISTS %s (\n%s\n%s;", t.Name, strings.Join(lines, ",\n"), suffix)
}

// IndexDDL renders t's secondary indexes as create-if-missing CREATE INDEX
// statements, in declaration order.
func (t Table) IndexDDL() []string {
	var out []string
	for _, idx := range t.Indexes {
		out = append(out, fmt.Sprintf("CREATE INDEX IF NOT EXISTS %s ON %s (%s);",
			idx.Name, t.Name, quoteIdents(idx.Columns)))
	}
	return out
}

// DDL renders the schema as an ordered create-if-missing statement list: each
// table's CREATE TABLE followed by its secondary indexes. Statements are emitted
// in dependency order -- a referenced table is always created before the table
// whose foreign key references it -- so the sequence applies cleanly against a
// real Postgres in one pass, with no forward references and no deferred ALTER.
// The model's Tables slice keeps the spec roster order; only the emission order
// is topologically sorted.
func (s Schema) DDL() []string {
	var out []string
	for _, t := range s.orderedTables() {
		out = append(out, t.CreateTableDDL())
		out = append(out, t.IndexDDL()...)
	}
	return out
}

// orderedTables returns the schema's tables in a stable topological order:
// every table follows the tables its foreign keys reference (self-references are
// ignored, as a self-FK resolves within the table's own CREATE statement). Among
// tables whose dependencies are all satisfied, the earliest in spec roster order
// wins, so the emission order is deterministic. A foreign-key graph is a DAG, so
// the sort always completes; a defensive fallback emits any residual in roster
// order rather than looping.
func (s Schema) orderedTables() []Table {
	emitted := make(map[string]bool, len(s.Tables))
	remaining := append([]Table(nil), s.Tables...)
	out := make([]Table, 0, len(s.Tables))

	for len(remaining) > 0 {
		progressed := false
		for i, t := range remaining {
			if !dependenciesSatisfied(t, emitted) {
				continue
			}
			out = append(out, t)
			emitted[t.Name] = true
			remaining = append(remaining[:i], remaining[i+1:]...)
			progressed = true
			break // restart the scan so roster order breaks ties.
		}
		if !progressed {
			out = append(out, remaining...)
			break
		}
	}
	return out
}

// dependenciesSatisfied reports whether every table t's foreign keys reference
// (other than itself) has already been emitted.
func dependenciesSatisfied(t Table, emitted map[string]bool) bool {
	for _, fk := range t.ForeignKeys {
		if fk.RefTable == t.Name {
			continue
		}
		if !emitted[fk.RefTable] {
			return false
		}
	}
	return true
}

// MetaSchema returns the meta control-plane schema: the seventeen tables of
// specification section 4, in the spec's own roster order. Roster order is not a
// safe DDL emission order (runs precedes artifacts it references); DDL() emits in
// FK-dependency order instead. Ordering keys are monotonic bigint identity
// columns; recorded_at is an opaque non-ordering text audit string throughout.
func MetaSchema() Schema {
	return Schema{
		Database: MetaDatabase,
		Tables: []Table{
			// pipelines: registry root. name PK; run is the JSON argv.
			{
				Name: "pipelines",
				Columns: []Column{
					{Name: "name", Type: "text"},
					{Name: "folder", Type: "text"},
					{Name: "run", Type: "json"},
					{Name: "artifact", Type: "text"},
					{Name: "data_mode", Type: "text"},
				},
				PrimaryKey: []string{"name"},
				Checks: []Check{
					{Column: "artifact", Values: []string{"source", "built"}},
					{Column: "data_mode", Values: []string{"disposable", "permanent"}},
				},
			},
			// dependencies: the depends_on graph as edge rows, indexed both directions.
			{
				Name: "dependencies",
				Columns: []Column{
					{Name: "from_pipeline", Type: "text"},
					{Name: "to_pipeline", Type: "text"},
				},
				PrimaryKey: []string{"from_pipeline", "to_pipeline"},
				ForeignKeys: []ForeignKey{
					{Column: "from_pipeline", RefTable: "pipelines", RefColumn: "name"},
					{Column: "to_pipeline", RefTable: "pipelines", RefColumn: "name"},
				},
				Indexes: []Index{
					{Name: "dependencies_to_pipeline_idx", Columns: []string{"to_pipeline"}},
				},
			},
			// lanes: persisted composer. pipeline is a name, never an FK.
			{
				Name: "lanes",
				Columns: []Column{
					{Name: "lane", Type: "text"},
					{Name: "pipeline", Type: "text"},
					{Name: "pos", Type: "bigint"},
				},
				PrimaryKey: []string{"lane", "pipeline"},
				Uniques: []Unique{
					{Columns: []string{"pipeline"}},
					{Columns: []string{"lane", "pos"}},
				},
			},
			// runs: history root. id is the monotonic bigint identity ordering key.
			{
				Name: "runs",
				Columns: []Column{
					{Name: "id", Type: "bigint", Identity: true},
					{Name: "pipeline", Type: "text"},
					{Name: "state", Type: "text"},
					{Name: "cause", Type: "text"},
					{Name: "replayed_from", Type: "bigint", Nullable: true},
					{Name: "exit_code", Type: "integer", Nullable: true},
					{Name: "handle", Type: "bigint", Nullable: true},
					{Name: "artifact_hash", Type: "text", Nullable: true},
					{Name: "declaration_checksum", Type: "text"},
					{Name: "log_ref", Type: "text", Nullable: true},
					{Name: "snapshot_lsn", Type: "text", Nullable: true},
					{Name: "journal_floor", Type: "bigint", Nullable: true},
					{Name: "journal_ceiling", Type: "bigint", Nullable: true},
					{Name: "recorded_at", Type: "text"},
				},
				PrimaryKey: []string{"id"},
				ForeignKeys: []ForeignKey{
					{Column: "pipeline", RefTable: "pipelines", RefColumn: "name"},
					{Column: "replayed_from", RefTable: "runs", RefColumn: "id"},
					{Column: "artifact_hash", RefTable: "artifacts", RefColumn: "hash"},
				},
				Checks: []Check{
					{Column: "state", Values: []string{"queued", "running", "succeeded", "dead_lettered"}},
					{Column: "cause", Values: []string{"manual", "loop", "replay", "propagated"}},
				},
				Indexes: []Index{
					{Name: "runs_pipeline_id_idx", Columns: []string{"pipeline", "id"}},
				},
			},
			// run_inputs: consumption ledger. Reverse-indexed on upstream_run_id.
			{
				Name: "run_inputs",
				Columns: []Column{
					{Name: "run_id", Type: "bigint"},
					{Name: "upstream_run_id", Type: "bigint"},
				},
				PrimaryKey: []string{"run_id", "upstream_run_id"},
				ForeignKeys: []ForeignKey{
					{Column: "run_id", RefTable: "runs", RefColumn: "id"},
					{Column: "upstream_run_id", RefTable: "runs", RefColumn: "id"},
				},
				Indexes: []Index{
					{Name: "run_inputs_upstream_run_id_idx", Columns: []string{"upstream_run_id"}},
				},
			},
			// dead_letters: the outstanding worklist. run_id PK FK.
			{
				Name: "dead_letters",
				Columns: []Column{
					{Name: "run_id", Type: "bigint"},
					{Name: "reason", Type: "text"},
					{Name: "error", Type: "text", Nullable: true},
					{Name: "failed_upstream", Type: "text", Nullable: true},
				},
				PrimaryKey: []string{"run_id"},
				ForeignKeys: []ForeignKey{
					{Column: "run_id", RefTable: "runs", RefColumn: "id"},
					{Column: "failed_upstream", RefTable: "pipelines", RefColumn: "name"},
				},
				Checks: []Check{
					{Column: "reason", Values: []string{"failed", "stopped", "upstream_dead_lettered"}},
				},
			},
			// artifacts: content-addressed built binaries. hash PK, row is the index.
			{
				Name: "artifacts",
				Columns: []Column{
					{Name: "hash", Type: "text"},
					{Name: "pipeline", Type: "text"},
					{Name: "size_bytes", Type: "bigint"},
					{Name: "recorded_at", Type: "text"},
				},
				PrimaryKey: []string{"hash"},
				ForeignKeys: []ForeignKey{
					{Column: "pipeline", RefTable: "pipelines", RefColumn: "name"},
				},
			},
			// run_summaries: archival tier. Insert-only, no FKs by design.
			{
				Name: "run_summaries",
				Columns: []Column{
					{Name: "run_id", Type: "bigint"},
					{Name: "pipeline", Type: "text"},
					{Name: "state", Type: "text"},
					{Name: "artifact_hash", Type: "text", Nullable: true},
					{Name: "declaration_checksum", Type: "text"},
					{Name: "consumed_upstream_run_ids", Type: "json"},
					{Name: "snapshot_lsn", Type: "text", Nullable: true},
					{Name: "journal_floor", Type: "bigint", Nullable: true},
					{Name: "journal_ceiling", Type: "bigint", Nullable: true},
					{Name: "recorded_at", Type: "text"},
				},
				PrimaryKey: []string{"run_id"},
			},
			// journal_checkpoints: tamper-evidence chain. seq identity PK, insert-only.
			{
				Name: "journal_checkpoints",
				Columns: []Column{
					{Name: "seq", Type: "bigint", Identity: true},
					{Name: "id_from", Type: "bigint"},
					{Name: "id_to", Type: "bigint"},
					{Name: "digest", Type: "bytea"},
					{Name: "parent_digest", Type: "bytea", Nullable: true},
					{Name: "signature", Type: "bytea"},
					{Name: "location", Type: "text"},
					{Name: "recorded_at", Type: "text"},
				},
				PrimaryKey: []string{"seq"},
				Checks: []Check{
					{Column: "location", Values: []string{"resident", "archived"}},
				},
			},
			// pats: the unified PAT store. id PK (token prefix), argon2id hash.
			{
				Name: "pats",
				Columns: []Column{
					{Name: "id", Type: "text"},
					{Name: "hash", Type: "text"},
					{Name: "label", Type: "text"},
					{Name: "revoked", Type: "boolean"},
				},
				PrimaryKey: []string{"id"},
			},
			// pat_scopes: the scope set, 1NF. Effective authority is the union of rows.
			{
				Name: "pat_scopes",
				Columns: []Column{
					{Name: "pat_id", Type: "text"},
					{Name: "scope", Type: "text"},
				},
				PrimaryKey: []string{"pat_id", "scope"},
				ForeignKeys: []ForeignKey{
					{Column: "pat_id", RefTable: "pats", RefColumn: "id"},
				},
				Checks: []Check{
					{Column: "scope", Values: []string{"control", "read", "data"}},
				},
			},
			// endpoints: persisted read endpoints. name PK, source is schema.table.
			{
				Name: "endpoints",
				Columns: []Column{
					{Name: "name", Type: "text"},
					{Name: "source", Type: "text"},
					{Name: "fields", Type: "json"},
					{Name: "sort", Type: "text"},
				},
				PrimaryKey: []string{"name"},
			},
			// endpoint_filters: per-endpoint filter grammar.
			{
				Name: "endpoint_filters",
				Columns: []Column{
					{Name: "endpoint", Type: "text"},
					{Name: "param", Type: "text"},
					{Name: "op", Type: "text"},
				},
				PrimaryKey: []string{"endpoint", "param"},
				ForeignKeys: []ForeignKey{
					{Column: "endpoint", RefTable: "endpoints", RefColumn: "name"},
				},
				Checks: []Check{
					{Column: "op", Values: []string{"eq", "range"}},
				},
			},
			// roles: the access ledger. Owner is a pipeline or a data PAT, exactly one.
			{
				Name: "roles",
				Columns: []Column{
					{Name: "pg_role", Type: "text"},
					{Name: "pipeline", Type: "text", Nullable: true},
					{Name: "pat", Type: "text", Nullable: true},
				},
				PrimaryKey: []string{"pg_role"},
				ForeignKeys: []ForeignKey{
					{Column: "pipeline", RefTable: "pipelines", RefColumn: "name"},
					{Column: "pat", RefTable: "pats", RefColumn: "id"},
				},
				RawChecks: []string{"(pipeline IS NULL) <> (pat IS NULL)"},
			},
			// grants: field-level access rows, indexed on pg_role (led by the PK).
			{
				Name: "grants",
				Columns: []Column{
					{Name: "pg_role", Type: "text"},
					{Name: "schema", Type: "text"},
					{Name: "table", Type: "text"},
					{Name: "field", Type: "text"},
					{Name: "access", Type: "text"},
				},
				PrimaryKey: []string{"pg_role", "schema", "table", "field", "access"},
				ForeignKeys: []ForeignKey{
					{Column: "pg_role", RefTable: "roles", RefColumn: "pg_role"},
				},
			},
			// credentials: engine-managed secret per login role (pipeline roles only).
			{
				Name: "credentials",
				Columns: []Column{
					{Name: "pg_role", Type: "text"},
					{Name: "secret", Type: "text"},
				},
				PrimaryKey: []string{"pg_role"},
				ForeignKeys: []ForeignKey{
					{Column: "pg_role", RefTable: "roles", RefColumn: "pg_role"},
				},
			},
			// migrations: the applied-migration ledger. applied_seq is the identity key.
			{
				Name: "migrations",
				Columns: []Column{
					{Name: "schema", Type: "text"},
					{Name: "table", Type: "text"},
					{Name: "migration_id", Type: "text"},
					{Name: "parent", Type: "text", Nullable: true},
					{Name: "checksum", Type: "text"},
					{Name: "applied_seq", Type: "bigint", Identity: true},
				},
				PrimaryKey: []string{"schema", "table", "migration_id"},
			},
		},
	}
}
