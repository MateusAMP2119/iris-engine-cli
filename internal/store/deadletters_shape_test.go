package store_test

import (
	"reflect"
	"strings"
	"testing"

	"github.com/MateusAMP2119/iris-lakehouse/internal/store"
)

// TestDeadLettersWorklistShape proves the dead_letters worklist table shape: one
// row per outstanding dead-lettered run awaiting disposition, keyed by run_id as
// a primary-key foreign key to runs; reason drawn from the closed set (failed,
// stopped, upstream_dead_lettered); a nullable human error; and a nullable
// failed_upstream foreign key to pipelines (the immediate upstream whose
// dead-lettered run propagated, else null). The DDL lives in the bootstrap meta
// schema (MetaSchema, schema.go); this test locks the worklist's shape as the
// contract replay and drain depend on.
func TestDeadLettersWorklistShape(t *testing.T) {
	s := store.MetaSchema()
	dl := tableByName(t, s, "dead_letters")

	// One row per outstanding dead-lettered run: run_id is the whole primary key.
	if !reflect.DeepEqual(dl.PrimaryKey, []string{"run_id"}) {
		t.Errorf("dead_letters primary key = %v, want [run_id] (one row per dead-lettered run)", dl.PrimaryKey)
	}

	// run_id is a bigint, non-nullable (it is the PK): the parked run.
	runID := columnByName(t, dl, "run_id")
	if runID.Type != "bigint" {
		t.Errorf("dead_letters.run_id type = %q, want bigint", runID.Type)
	}
	if runID.Nullable {
		t.Error("dead_letters.run_id is nullable; the primary key must be NOT NULL")
	}

	// error is a nullable human detail; failed_upstream is nullable (null for a
	// direct failure, set for a propagated entry).
	if errCol := columnByName(t, dl, "error"); !errCol.Nullable {
		t.Error("dead_letters.error must be nullable (a direct failure may carry none)")
	}
	if fu := columnByName(t, dl, "failed_upstream"); !fu.Nullable {
		t.Error("dead_letters.failed_upstream must be nullable (null unless propagated)")
	}

	// reason is the closed enum the CHECK pins.
	var reasonCheck *store.Check
	for i := range dl.Checks {
		if dl.Checks[i].Column == "reason" {
			reasonCheck = &dl.Checks[i]
		}
	}
	if reasonCheck == nil {
		t.Fatal("dead_letters has no CHECK on reason")
	}
	if !reflect.DeepEqual(reasonCheck.Values, []string{"failed", "stopped", "upstream_dead_lettered"}) {
		t.Errorf("dead_letters.reason values = %v, want [failed stopped upstream_dead_lettered]", reasonCheck.Values)
	}

	// run_id -> runs.id and failed_upstream -> pipelines.name are the two foreign keys.
	fks := make(map[string]store.ForeignKey, len(dl.ForeignKeys))
	for _, fk := range dl.ForeignKeys {
		fks[fk.Column] = fk
	}
	if got, ok := fks["run_id"]; !ok || got.RefTable != "runs" || got.RefColumn != "id" {
		t.Errorf("dead_letters.run_id FK = %+v, want -> runs.id", got)
	}
	if got, ok := fks["failed_upstream"]; !ok || got.RefTable != "pipelines" || got.RefColumn != "name" {
		t.Errorf("dead_letters.failed_upstream FK = %+v, want -> pipelines.name", got)
	}

	// The rendered DDL carries the reason CHECK and the two foreign keys: the model
	// meets Postgres exactly as asserted above.
	ddl := dl.CreateTableDDL()
	for _, want := range []string{
		"CHECK (reason IN ('failed', 'stopped', 'upstream_dead_lettered'))",
		"FOREIGN KEY (run_id) REFERENCES runs (id)",
		"FOREIGN KEY (failed_upstream) REFERENCES pipelines (name)",
		"PRIMARY KEY (run_id)",
	} {
		if !strings.Contains(ddl, want) {
			t.Errorf("dead_letters DDL is missing %q:\n%s", want, ddl)
		}
	}
}
