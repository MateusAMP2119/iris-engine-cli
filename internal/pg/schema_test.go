package pg_test

import (
	"context"
	"path/filepath"
	"strings"
	"testing"

	"github.com/MateusAMP2119/iris-engine-cli/internal/golden"
	"github.com/MateusAMP2119/iris-engine-cli/internal/pg"
	"github.com/MateusAMP2119/iris-engine-cli/internal/pg/pgtest"
)

// journalColumn returns the named column of the journal table, failing the test
// when it is absent.
func journalColumn(t *testing.T, name string) pg.Column {
	t.Helper()
	for _, c := range pg.JournalTable().Columns {
		if c.Name == name {
			return c
		}
	}
	t.Fatalf("data_journal has no column %q", name)
	return pg.Column{}
}

// TestDataJournalShape proves data_journal is created in the data database's
// public schema as a partitioned capture table with exactly two indexes -- its
// primary key and the (schema, table, row_pk, run_id) provenance key.
//
// spec: S04/state-split-meta-vs-data
func TestDataJournalShape(t *testing.T) {
	ctx := context.Background()
	jt := pg.JournalTable()

	if jt.Schema != "public" {
		t.Errorf("data_journal schema = %q, want public (the readable surface)", jt.Schema)
	}
	if jt.Name != "data_journal" {
		t.Errorf("data table name = %q, want data_journal", jt.Name)
	}
	if jt.Partition != "id" {
		t.Errorf("data_journal partition key = %q, want id (PARTITION BY RANGE (id))", jt.Partition)
	}

	// Exactly two indexes: the primary key on id plus the provenance key.
	if len(jt.PrimaryKey) != 1 || jt.PrimaryKey[0] != "id" {
		t.Errorf("data_journal primary key = %v, want [id]", jt.PrimaryKey)
	}
	if len(jt.Indexes) != 1 {
		t.Fatalf("data_journal has %d secondary indexes, want exactly 1 (PK + provenance = two total)", len(jt.Indexes))
	}
	if want := []string{"schema", "table", "row_pk", "run_id"}; !equalStrings(jt.Indexes[0].Columns, want) {
		t.Errorf("data_journal provenance index = %v, want %v", jt.Indexes[0].Columns, want)
	}

	// The rendered DDL is a partitioned create-if-missing statement in public.
	rec := pgtest.New()
	if err := pg.EnsureJournal(ctx, rec); err != nil {
		t.Fatalf("EnsureJournal: %v", err)
	}
	create := rec.Statements()[0]
	for _, want := range []string{
		"CREATE TABLE IF NOT EXISTS public.data_journal",
		"PARTITION BY RANGE (id)",
	} {
		if !strings.Contains(create, want) {
			t.Errorf("data_journal DDL missing %q:\n%s", want, create)
		}
	}

	golden.Assert(t, rec.Dump(), filepath.Join("testdata", "data_journal.sql"))
}

// TestDataJournalOrderingIdentity proves the journal ordering key is a monotonic
// bigint identity column, recorded_at is an opaque text audit string, and the
// table carries a nullable pre_image but no post-image or clock column.
//
// spec: S04/ordering-identity-never-clock
func TestDataJournalOrderingIdentity(t *testing.T) {
	jt := pg.JournalTable()

	id := journalColumn(t, "id")
	if !id.Identity || id.Type != "bigint" {
		t.Errorf("data_journal.id = {identity:%v type:%q}, want a monotonic bigint identity", id.Identity, id.Type)
	}

	recordedAt := journalColumn(t, "recorded_at")
	if recordedAt.Type != "text" {
		t.Errorf("data_journal.recorded_at type = %q, want text (opaque non-ordering audit string)", recordedAt.Type)
	}

	// pre_image is a nullable prior-row image; there is no post-image (provenance
	// returns lineage, never images).
	pre := journalColumn(t, "pre_image")
	if !pre.Nullable {
		t.Error("data_journal.pre_image must be nullable (null on inserts and entries born promoted)")
	}
	for _, c := range jt.Columns {
		if c.Name == "post_image" {
			t.Error("data_journal has a post_image column; the journal keeps no post-image")
		}
		if strings.Contains(c.Type, "timestamp") {
			t.Errorf("data_journal.%s is a clock type %q; ordering is bigint identity, never a clock", c.Name, c.Type)
		}
	}
}

// TestDataJournalNoForeignKeys proves data_journal carries no foreign keys: its
// run_id links to runs logically only, never FK-enforced across the meta/data
// database boundary.
//
// spec: S04/fk-graph-matches-spec
func TestDataJournalNoForeignKeys(t *testing.T) {
	ctx := context.Background()

	// run_id is a plain bigint attribution column, not a foreign key.
	runID := journalColumn(t, "run_id")
	if runID.Type != "bigint" {
		t.Errorf("data_journal.run_id type = %q, want bigint (logical link to runs)", runID.Type)
	}

	rec := pgtest.New()
	if err := pg.EnsureJournal(ctx, rec); err != nil {
		t.Fatalf("EnsureJournal: %v", err)
	}
	for _, s := range rec.Statements() {
		up := strings.ToUpper(s)
		if strings.Contains(up, "FOREIGN KEY") || strings.Contains(up, "REFERENCES") {
			t.Errorf("data_journal DDL declares a foreign key; the journal carries none:\n%s", s)
		}
	}
}

// equalStrings reports whether a and b hold the same strings in the same order.
func equalStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
