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
	markRunRunningSQL  = "UPDATE runs SET state = $1, handle = $2 WHERE id = $3 AND state = $4"
)

// MarkRunRunning records a started run: in one guarded statement it transitions the
// run from queued to running and records its subprocess process-group id as
// runs.handle (specification section 1: handle = process-group id, set when the
// subprocess starts). The dispatcher submits it through the single writer the moment
// exec starts a run. The UPDATE is guarded on the queued state, so it can only ever
// act on a run that has not already started -- never one already running or terminal
// -- and it is one atomic Exec, never a read-then-write that could split. It is a
// leader-only meta write, riding the single Writer.
func (w *Writer) MarkRunRunning(ctx context.Context, id string, pgid int) error {
	if err := w.conn.Exec(ctx, markRunRunningSQL, RunRunning, pgid, id, RunQueued); err != nil {
		return fmt.Errorf("store: writer mark run running %s: %w", id, err)
	}
	return nil
}

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

// MigrationHead is one applied-migration ledger row an engine records against the
// meta migrations table (specification section 4): the (schema, table,
// migration_id) key, the parent migration id, and the checksum of table.yaml at
// that revision. The applied_seq identity column is assigned by meta on insert,
// never carried here. It is the durable applied head that ledger-versus-disk drift
// detection compares against the migrations/ files on disk.
type MigrationHead struct {
	// Schema is the declared table's schema.
	Schema string
	// Table is the declared table's name.
	Table string
	// MigrationID is the zero-padded migration id being recorded as applied (e.g.
	// "0002").
	MigrationID string
	// Parent is the predecessor migration id (e.g. "0001"); empty for the create
	// head (0001_create), which is recorded as a SQL NULL parent.
	Parent string
	// Checksum is the checksum of table.yaml at this revision (declare.ChecksumTableYAML).
	Checksum string
}

// recordMigrationHeadSQL inserts one applied-migration ledger row. It is a single
// statement: recording an applied head is one atomic Exec, never a read-then-write
// that could split. applied_seq is omitted so meta's identity generator assigns
// the monotonic ordering key; the reserved column names schema and table are
// double-quoted.
const recordMigrationHeadSQL = `INSERT INTO migrations ("schema", "table", migration_id, parent, checksum)
VALUES ($1, $2, $3, $4, $5)`

// RecordMigrationHead inserts a migrations row recording a table's applied head so
// each table's applied migration is durably recorded for ledger-versus-disk drift
// detection (specification section 4). The insert is one atomic statement; an empty
// parent (the create head) is recorded as SQL NULL rather than an empty string, so
// the create head is distinguishable from a migration that names "" as its parent.
// It is a leader-only meta write, riding the single Writer.
func (w *Writer) RecordMigrationHead(ctx context.Context, head MigrationHead) error {
	var parent any // nil -> SQL NULL for the create head.
	if head.Parent != "" {
		parent = head.Parent
	}
	if err := w.conn.Exec(ctx, recordMigrationHeadSQL, head.Schema, head.Table, head.MigrationID, parent, head.Checksum); err != nil {
		return fmt.Errorf("store: writer record migration head %s.%s %s: %w", head.Schema, head.Table, head.MigrationID, err)
	}
	return nil
}
