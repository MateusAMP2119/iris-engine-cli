package store

import (
	"context"
	"fmt"
)

// This file is the single-writer meta path: the one type through which every meta
// write flows (specification sections 2 and 6.1). Only the leader writes meta, and
// it does so through exactly one Writer, driven serially by the one dispatcher
// goroutine (internal/dispatch). The construction is deliberately narrow: a Writer
// is built only from a MetaWriteConn -- the leader's live meta connection -- and
// the sole constructor (NewWriter) is called only by the dispatcher (enforced by a
// static architecture check), so no other component can mint a meta writer and
// open a second write path.

// MetaWriteConn is the leader's live meta write connection: the one connection meta
// mutations are issued on. The pgx-backed meta client supplies the production
// implementation (the leader's session connection); a recording fake stands in for
// tests. It is the raw seam a Writer wraps.
type MetaWriteConn interface {
	// Exec issues one write statement (DDL or DML) against meta on the leader's
	// connection.
	Exec(ctx context.Context, sql string, args ...any) error
}

// Writer is the single meta-write surface. Every meta write flows through one
// Writer, held by the one dispatcher goroutine, so writes are serialized onto one
// connection by one owner. It is constructed only by NewWriter, which the
// dispatcher alone calls; the architecture gate proves no other package does, so
// the single-writer invariant cannot be bypassed by minting a second writer.
type Writer struct {
	conn MetaWriteConn
}

// NewWriter builds the single meta writer over the leader's write connection. It is
// exported so the dispatcher (a different package) can construct the Writer it
// owns, but a static architecture check restricts the call site to internal/dispatch:
// no other package may construct a meta writer, so meta has exactly one write path.
func NewWriter(conn MetaWriteConn) *Writer {
	return &Writer{conn: conn}
}

// EnsureSchema issues the meta control-table DDL create-if-missing on the leader's
// connection: the schema re-check the leader performs at election (specification
// section 4, re-checked at each leader election). It is a leader-only meta write,
// so it runs through the single Writer -- not from a candidate that has not won the
// lock, and not on any connection but the leader's.
func (w *Writer) EnsureSchema(ctx context.Context) error {
	for _, stmt := range MetaSchema().DDL() {
		if err := w.conn.Exec(ctx, stmt); err != nil {
			return fmt.Errorf("store: writer ensure meta schema: %w", err)
		}
	}
	return nil
}

// The run-record write statements crash reconciliation submits through the single
// writer. Both are guarded on the run's source state so they can only ever act on a
// run that is actually in that state (never one that has since progressed).
//
// deadLetterRunSQL is a single CTE, not two statements: the state transition and the
// dead_letters worklist insert are one atomic Exec, so there is no window in which a
// failure could leave a dead_lettered run with no worklist row -- an orphan
// reconciliation would never repair, since it scans only running and queued runs.
// The INSERT feeds off the UPDATE's RETURNING, so it runs if and only if the guarded
// transition took effect.
const (
	deadLetterRunSQL = `WITH updated AS (
    UPDATE runs SET state = $1 WHERE id = $2 AND state = $3 RETURNING id
)
INSERT INTO dead_letters (run_id, reason, error)
SELECT id, $4, $5 FROM updated`
	deleteQueuedRunSQL = "DELETE FROM runs WHERE id = $1 AND state = $2"
)

// DeadLetterRun dead-letters a leftover run: in one atomic statement it transitions
// the run from running to the dead_lettered terminal state and records its
// dead_letters worklist row with the given reason and human error detail. Crash
// reconciliation calls it for a run left running when the daemon died (reason
// ReasonStopped, detail "daemon terminated while run was in flight" -- specification
// section 2 crash recovery). The state transition and worklist insert are a single
// CTE, so they commit together or not at all: a dead_lettered run can never be left
// without its worklist row. It is a leader-only meta write, riding the single Writer.
func (w *Writer) DeadLetterRun(ctx context.Context, id string, reason DeadLetterReason, detail string) error {
	if err := w.conn.Exec(ctx, deadLetterRunSQL, RunDeadLettered, id, RunRunning, reason, detail); err != nil {
		return fmt.Errorf("store: writer dead-letter run %s: %w", id, err)
	}
	return nil
}

// DeleteQueuedRun deletes a queued never-started run so the next dispatch pass
// recreates it (specification section 2 crash recovery: queued runs consumed
// nothing, so they are deleted, not dead-lettered). The DELETE is guarded on the
// queued state: it can never remove a run that has since started. It is a
// leader-only meta write, riding the single Writer.
func (w *Writer) DeleteQueuedRun(ctx context.Context, id string) error {
	if err := w.conn.Exec(ctx, deleteQueuedRunSQL, id, RunQueued); err != nil {
		return fmt.Errorf("store: writer delete queued run %s: %w", id, err)
	}
	return nil
}
