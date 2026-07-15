package store_test

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/MateusAMP2119/iris-lakehouse/internal/store"
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
func TestWriterEnsureSchema(t *testing.T) {
	t.Run("only-leader-writes-meta", func(t *testing.T) {
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

// TestWriterDeadLetterRunAtomic proves the single writer dead-letters a run
// atomically: the state transition to dead_lettered and the dead_letters worklist
// row are ONE statement, never two. Two separate statements have an orphan window --
// a failure between them (a shutdown ctx cancel, a transient error) would leave a
// dead_lettered run with no worklist row, and reconciliation (which scans only
// running/queued runs) would never repair it, so the run would be lost from the
// worklist forever. One CTE closes that window.
func TestWriterDeadLetterRunAtomic(t *testing.T) {
	t.Run("inflight-runs-deadlettered", func(t *testing.T) {
		conn := &recordingWriteConn{}
		w := store.NewWriter(conn)

		// detail is a free string; its exact wording ("daemon terminated ...") is
		// asserted by the reconciliation tests, not here.
		if err := w.DeadLetterRun(context.Background(), "run-7", store.ReasonStopped, "daemon terminated while run was in flight"); err != nil {
			t.Fatalf("DeadLetterRun: %v", err)
		}
		if len(conn.stmts) != 1 {
			t.Fatalf("DeadLetterRun issued %d statements, want exactly 1 (atomic: no orphan window between the state change and the worklist row)", len(conn.stmts))
		}
		stmt := conn.stmts[0]
		if !strings.Contains(stmt, "UPDATE runs SET state") {
			t.Errorf("the atomic dead-letter statement does not transition the run state: %q", stmt)
		}
		if !strings.Contains(stmt, "INSERT INTO dead_letters") {
			t.Errorf("the atomic dead-letter statement does not record the dead_letters worklist row: %q", stmt)
		}
	})
}
