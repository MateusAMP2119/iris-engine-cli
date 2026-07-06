package store_test

import (
	"context"
	"errors"
	"testing"

	"github.com/MateusAMP2119/iris-engine-cli/internal/store"
)

// recordingWriteConn is a store.MetaWriteConn that records every write statement,
// standing in for the leader's single meta connection with no live Postgres.
type recordingWriteConn struct {
	stmts []string
	err   error
}

func (c *recordingWriteConn) Exec(_ context.Context, sql string, _ ...any) error {
	c.stmts = append(c.stmts, sql)
	return c.err
}

// TestWriterEnsureSchema proves the single-writer surface issues the meta schema
// re-check (the leader's create-if-missing DDL) through its one connection: the
// Writer is the sole meta-write path, and EnsureSchema is the leader-only write it
// performs at election.
//
// spec: S04/only-leader-writes-meta
func TestWriterEnsureSchema(t *testing.T) {
	t.Run("S04/only-leader-writes-meta", func(t *testing.T) {
		conn := &recordingWriteConn{}
		w := store.NewWriter(conn)

		if err := w.EnsureSchema(context.Background()); err != nil {
			t.Fatalf("EnsureSchema: %v", err)
		}
		want := store.MetaSchema().DDL()
		if len(conn.stmts) != len(want) {
			t.Fatalf("EnsureSchema issued %d statements, want %d", len(conn.stmts), len(want))
		}
		for i := range want {
			if conn.stmts[i] != want[i] {
				t.Errorf("statement %d = %q, want %q", i, conn.stmts[i], want[i])
			}
		}
	})

	t.Run("a write error propagates from the single writer", func(t *testing.T) {
		boom := errors.New("meta connection lost")
		w := store.NewWriter(&recordingWriteConn{err: boom})
		if err := w.EnsureSchema(context.Background()); !errors.Is(err, boom) {
			t.Errorf("EnsureSchema error = %v, want it to wrap %v", err, boom)
		}
	})
}
