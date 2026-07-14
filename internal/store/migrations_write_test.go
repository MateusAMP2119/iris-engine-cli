package store_test

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/MateusAMP2119/iris-lakehouse/internal/store"
)

// recordingMigrationConn is a store.MetaWriteConn that records every write
// statement and its bound arguments, standing in for the leader's single meta
// connection with no live Postgres.
type recordingMigrationConn struct {
	stmts []string
	args  [][]any
	err   error
}

func (c *recordingMigrationConn) Exec(_ context.Context, sql string, args ...any) error {
	c.stmts = append(c.stmts, sql)
	c.args = append(c.args, args)
	return c.err
}

// TestMigrationsLedgerShape proves the migrations ledger keys its rows by
// (schema, table, migration_id) and carries the parent, checksum, and applied_seq
// columns: the applied-migration ledger. The applied_seq is a monotonic bigint
// identity assigned by meta, never a clock. The write path
// (RecordMigrationHead) binds exactly the three key columns plus parent and
// checksum, leaving applied_seq to meta.
func TestMigrationsLedgerShape(t *testing.T) {
	t.Run("migrations-ledger-shape", func(t *testing.T) {
		tbl := migrationsTable(t)

		// Keyed by (schema, table, migration_id).
		wantPK := []string{"schema", "table", "migration_id"}
		if len(tbl.PrimaryKey) != len(wantPK) {
			t.Fatalf("migrations primary key = %v, want %v", tbl.PrimaryKey, wantPK)
		}
		for i, col := range wantPK {
			if tbl.PrimaryKey[i] != col {
				t.Errorf("migrations primary key[%d] = %q, want %q", i, tbl.PrimaryKey[i], col)
			}
		}

		// Carries parent, checksum, and an identity applied_seq.
		cols := map[string]store.Column{}
		for _, c := range tbl.Columns {
			cols[c.Name] = c
		}
		for _, name := range []string{"schema", "table", "migration_id", "parent", "checksum", "applied_seq"} {
			if _, ok := cols[name]; !ok {
				t.Errorf("migrations is missing the %q column", name)
			}
		}
		if seq := cols["applied_seq"]; !seq.Identity || seq.Type != "bigint" {
			t.Errorf("migrations.applied_seq = {identity:%v type:%q}, want a monotonic bigint identity", seq.Identity, seq.Type)
		}
		// parent is nullable: the create head (0001_create) has no parent.
		if !cols["parent"].Nullable {
			t.Error("migrations.parent must be nullable (the create head has no parent)")
		}

		// The write path binds exactly the five key/ledger columns; applied_seq is
		// left to meta's identity generator.
		conn := &recordingMigrationConn{}
		w := store.NewWriter(conn)
		head := store.MigrationHead{
			Schema: "analytics", Table: "orders", MigrationID: "0002",
			Parent: "0001", Checksum: "c0bb62",
		}
		if err := w.RecordMigrationHead(context.Background(), head); err != nil {
			t.Fatalf("RecordMigrationHead: %v", err)
		}
		if len(conn.stmts) != 1 {
			t.Fatalf("RecordMigrationHead issued %d statements, want exactly 1", len(conn.stmts))
		}
		stmt := conn.stmts[0]
		for _, frag := range []string{"INSERT INTO migrations", `"schema"`, `"table"`, "migration_id", "parent", "checksum"} {
			if !strings.Contains(stmt, frag) {
				t.Errorf("insert statement missing %q:\n%s", frag, stmt)
			}
		}
		if strings.Contains(stmt, "applied_seq") {
			t.Errorf("insert must not bind applied_seq (meta's identity generator owns it):\n%s", stmt)
		}
		if got := conn.args[0]; len(got) != 5 {
			t.Errorf("bound %d args, want 5 (schema, table, migration_id, parent, checksum): %v", len(got), got)
		}
	})
}

// TestRecordMigrationHead proves applying a table migration inserts one
// migrations row so the table's applied head is durably recorded for
// ledger-versus-disk drift detection. The insert is a single statement (atomic),
// binds the head's key and ledger columns in order, and represents the create
// head (empty parent) as SQL NULL rather than an empty string.
func TestRecordMigrationHead(t *testing.T) {
	t.Run("migrations-record-applied-head", func(t *testing.T) {
		conn := &recordingMigrationConn{}
		w := store.NewWriter(conn)

		head := store.MigrationHead{
			Schema: "analytics", Table: "orders", MigrationID: "0002",
			Parent: "0001", Checksum: "c0bb62137b875d4d76dc820e5f98cf9afe740831eb1476cf3072e1d1885dd353",
		}
		if err := w.RecordMigrationHead(context.Background(), head); err != nil {
			t.Fatalf("RecordMigrationHead: %v", err)
		}
		if len(conn.stmts) != 1 {
			t.Fatalf("RecordMigrationHead issued %d statements, want exactly 1 (one atomic insert of the applied head)", len(conn.stmts))
		}
		args := conn.args[0]
		want := []any{"analytics", "orders", "0002", "0001", head.Checksum}
		if len(args) != len(want) {
			t.Fatalf("bound %d args, want %d: %v", len(args), len(want), args)
		}
		for i := range want {
			if args[i] != want[i] {
				t.Errorf("arg[%d] = %v, want %v", i, args[i], want[i])
			}
		}
	})

	t.Run("the create head records a NULL parent", func(t *testing.T) {
		conn := &recordingMigrationConn{}
		w := store.NewWriter(conn)
		head := store.MigrationHead{Schema: "analytics", Table: "orders", MigrationID: "0001", Checksum: "abc"}
		if err := w.RecordMigrationHead(context.Background(), head); err != nil {
			t.Fatalf("RecordMigrationHead: %v", err)
		}
		if parent := conn.args[0][3]; parent != nil {
			t.Errorf("create-head parent arg = %v (%T), want SQL NULL (nil)", parent, parent)
		}
	})

	t.Run("a write error propagates from the single writer", func(t *testing.T) {
		boom := errors.New("meta connection lost")
		w := store.NewWriter(&recordingMigrationConn{err: boom})
		head := store.MigrationHead{Schema: "s", Table: "t", MigrationID: "0002", Parent: "0001", Checksum: "x"}
		if err := w.RecordMigrationHead(context.Background(), head); !errors.Is(err, boom) {
			t.Errorf("RecordMigrationHead error = %v, want it to wrap %v", err, boom)
		}
	})
}

// migrationsTable returns the migrations table model from the meta schema, failing
// the test if it is absent.
func migrationsTable(t *testing.T) store.Table {
	t.Helper()
	for _, tbl := range store.MetaSchema().Tables {
		if tbl.Name == "migrations" {
			return tbl
		}
	}
	t.Fatal("meta schema has no migrations table")
	return store.Table{}
}
