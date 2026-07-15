package store_test

import (
	"testing"

	"github.com/MateusAMP2119/iris-lakehouse/internal/pg"
	"github.com/MateusAMP2119/iris-lakehouse/internal/store"
)

// TestEighteenTableRoster proves the bootstrap DDL creates exactly twenty-one engine
// tables: the twenty meta control tables plus public.data_journal in the data
// database. The eighteenth meta table is engine_key (moving the engine signing key
// from a per-database GUC into an engine-owned single-row meta table); the
// nineteenth is read_pool_credential (persisting the shared read-pool login secret
// create-once, so a restart or HA standby reuses one stable credential); the
// twentieth is leadership (the leader's advertised address a standby reads to name
// the leader). The test name keeps its original count though the roster grew.
func TestEighteenTableRoster(t *testing.T) {
	meta := store.MetaSchema()

	if got := len(meta.Tables); got != len(metaRoster) {
		t.Fatalf("meta schema has %d tables, want %d", got, len(metaRoster))
	}
	// The twenty meta tables are exactly the roster, in order.
	for i, want := range metaRoster {
		if meta.Tables[i].Name != want {
			t.Errorf("meta table %d = %q, want %q", i, meta.Tables[i].Name, want)
		}
	}

	// The twenty-first table is public.data_journal in the data database.
	jt := pg.JournalTable()
	if jt.Name != "data_journal" {
		t.Errorf("data table name = %q, want data_journal", jt.Name)
	}

	total := len(meta.Tables) + 1
	if total != 21 {
		t.Errorf("engine table roster = %d, want exactly 21 (20 meta + data_journal)", total)
	}

	// data_journal is not a meta control table: the twenty-first table lives on the
	// data side, never doubled into meta.
	for _, m := range meta.Tables {
		if m.Name == "data_journal" {
			t.Error("data_journal appears among the meta control tables; it belongs to the data database only")
		}
	}
}

// TestStateSplitMetaVsData proves the engine control tables are created in the
// dedicated meta database while data_journal is created in the data database's
// public schema.
func TestStateSplitMetaVsData(t *testing.T) {
	meta := store.MetaSchema()

	if store.MetaDatabase != "meta" {
		t.Errorf("meta database name = %q, want meta", store.MetaDatabase)
	}
	if meta.Database != store.MetaDatabase {
		t.Errorf("meta schema database = %q, want %q", meta.Database, store.MetaDatabase)
	}
	if len(meta.Tables) != len(metaRoster) {
		t.Errorf("meta side holds %d tables, want the %d control tables", len(meta.Tables), len(metaRoster))
	}

	// data_journal lives in the data database's public schema, apart from meta.
	jt := pg.JournalTable()
	if jt.Schema != "public" {
		t.Errorf("data_journal schema = %q, want public", jt.Schema)
	}
	if got := jt.Qualified(); got != "public.data_journal" {
		t.Errorf("data_journal qualified name = %q, want public.data_journal", got)
	}
	// The names comparison confirms no control table is placed on the data side.
	metaNames := sortedNames(meta)
	for _, n := range metaNames {
		if n == jt.Name {
			t.Errorf("control table %q collides with the data-side journal", n)
		}
	}
}
