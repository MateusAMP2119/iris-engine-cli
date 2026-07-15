package pg_test

import (
	"context"
	"path/filepath"
	"strings"
	"testing"

	"github.com/MateusAMP2119/iris-lakehouse/internal/golden"
	"github.com/MateusAMP2119/iris-lakehouse/internal/pg"
	"github.com/MateusAMP2119/iris-lakehouse/internal/pg/pgtest"
)

// TestJournalTableShape proves data_journal declares exactly the specified shape:
// a bigint identity primary key, the pg_role / run_id / schema / table / row_pk /
// op / pre_image / undo / recorded_at columns with no post-image, op constrained
// to (insert, update, delete), undo to (open, promoted, wiped, skipped), and
// exactly two indexes -- the primary key on id and the (schema, table, row_pk,
// run_id) provenance key.
func TestJournalTableShape(t *testing.T) {
	ctx := context.Background()
	jt := pg.JournalTable()

	// The exact column set, in declaration order, with each column's type,
	// identity flag, and nullability. There is no post-image column.
	wantCols := []pg.Column{
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
	}
	if len(jt.Columns) != len(wantCols) {
		t.Fatalf("data_journal has %d columns, want %d: %+v", len(jt.Columns), len(wantCols), jt.Columns)
	}
	for i, want := range wantCols {
		if got := jt.Columns[i]; got != want {
			t.Errorf("column[%d] = %+v, want %+v", i, got, want)
		}
	}
	for _, c := range jt.Columns {
		if c.Name == "post_image" {
			t.Error("data_journal declares a post_image column; the journal keeps no post-image")
		}
	}

	// Primary key is the identity id.
	if len(jt.PrimaryKey) != 1 || jt.PrimaryKey[0] != "id" {
		t.Errorf("data_journal primary key = %v, want [id]", jt.PrimaryKey)
	}

	// CHECK constraints: op and undo pinned to their closed value sets.
	wantChecks := map[string][]string{
		"op":   {"insert", "update", "delete"},
		"undo": {"open", "promoted", "wiped", "skipped"},
	}
	if len(jt.Checks) != len(wantChecks) {
		t.Fatalf("data_journal has %d CHECK constraints, want %d: %+v", len(jt.Checks), len(wantChecks), jt.Checks)
	}
	for _, ck := range jt.Checks {
		want, ok := wantChecks[ck.Column]
		if !ok {
			t.Errorf("unexpected CHECK on column %q", ck.Column)
			continue
		}
		if !equalStrings(ck.Values, want) {
			t.Errorf("CHECK (%s) values = %v, want %v", ck.Column, ck.Values, want)
		}
	}

	// Exactly two indexes: the primary key plus one secondary provenance index
	// on (schema, table, row_pk, run_id).
	if len(jt.Indexes) != 1 {
		t.Fatalf("data_journal has %d secondary indexes, want exactly 1 (PK + provenance = two total): %+v", len(jt.Indexes), jt.Indexes)
	}
	if want := []string{"schema", "table", "row_pk", "run_id"}; !equalStrings(jt.Indexes[0].Columns, want) {
		t.Errorf("provenance index columns = %v, want %v", jt.Indexes[0].Columns, want)
	}

	// The rendered DDL matches the golden byte-for-byte (a golden diff is a
	// contract diff).
	rec := pgtest.New()
	if err := pg.EnsureJournal(ctx, rec); err != nil {
		t.Fatalf("EnsureJournal: %v", err)
	}
	golden.Assert(t, rec.Dump(), filepath.Join("testdata", "data_journal.sql"))
}

// TestJournalRunIDNotFK proves data_journal.run_id references runs logically only:
// a plain bigint attribution column, never a foreign key. The journal and runs
// live in different databases, so no FK constraint can span them.
func TestJournalRunIDNotFK(t *testing.T) {
	ctx := context.Background()

	var runID pg.Column
	for _, c := range pg.JournalTable().Columns {
		if c.Name == "run_id" {
			runID = c
		}
	}
	if runID.Name == "" {
		t.Fatal("data_journal has no run_id column")
	}
	if runID.Type != "bigint" {
		t.Errorf("data_journal.run_id type = %q, want bigint (a plain logical link to runs)", runID.Type)
	}

	// The emitted DDL declares no foreign key anywhere: run_id carries no
	// REFERENCES clause and the table has no FOREIGN KEY constraint.
	rec := pgtest.New()
	if err := pg.EnsureJournal(ctx, rec); err != nil {
		t.Fatalf("EnsureJournal: %v", err)
	}
	for _, s := range rec.Statements() {
		up := strings.ToUpper(s)
		if strings.Contains(up, "FOREIGN KEY") || strings.Contains(up, "REFERENCES") {
			t.Errorf("data_journal DDL declares a foreign key; run_id links to runs logically only:\n%s", s)
		}
	}
}

// TestJournalLivesInDataDB proves the capture journal is created as
// public.data_journal in the data database -- the surface capture triggers write
// inside the data transaction -- and never in the meta control database where all
// other engine state lives.
func TestJournalLivesInDataDB(t *testing.T) {
	ctx := context.Background()

	// The journal is public.data_journal, and pg's data database is "data": the
	// journal is hosted by the data database, not meta.
	jt := pg.JournalTable()
	if jt.Schema != "public" {
		t.Errorf("data_journal schema = %q, want public (the readable surface of the data database)", jt.Schema)
	}
	if got := jt.Qualified(); got != "public.data_journal" {
		t.Errorf("journal qualified name = %q, want public.data_journal", got)
	}
	if pg.DataDatabase != "data" {
		t.Errorf("pg.DataDatabase = %q, want data (the database that hosts the journal)", pg.DataDatabase)
	}

	// EnsureJournal issues its DDL through the data-database client seam, and every
	// statement targets public.data_journal -- never the meta control database.
	// Triggers therefore write the journal inside the data transaction.
	rec := pgtest.New()
	if err := pg.EnsureJournal(ctx, rec); err != nil {
		t.Fatalf("EnsureJournal: %v", err)
	}
	stmts := rec.Statements()
	if len(stmts) == 0 {
		t.Fatal("EnsureJournal issued no DDL")
	}
	for _, s := range stmts {
		if !strings.Contains(s, "public.data_journal") {
			t.Errorf("journal DDL statement does not target public.data_journal:\n%s", s)
		}
		// The meta database is a separate database ("meta"); no journal DDL may
		// name it -- the journal lives only in the data database.
		if strings.Contains(strings.ToLower(s), "meta.") || strings.Contains(s, " meta ") {
			t.Errorf("journal DDL references the meta database; the journal lives only in the data database:\n%s", s)
		}
	}
}
