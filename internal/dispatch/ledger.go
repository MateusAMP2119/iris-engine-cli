package dispatch

import (
	"context"

	"github.com/MateusAMP2119/iris-engine-cli/internal/pg"
	"github.com/MateusAMP2119/iris-engine-cli/internal/store"
)

// This file bridges the schema provisioner's ledger-recording seam (pg.LedgerRecorder)
// to the single meta writer. Provisioning records each table's applied migration head
// in meta; that is a leader-only meta write, so it must ride the dispatcher rather than
// a second writer. The adapter submits store.Writer.RecordMigrationHead through the
// single-writer path, converting pg's MigrationHead to store's field-for-field (the
// two mirror each other so the peer database clients need not import one another).

// ledgerRecorder adapts a Submitter (the Dispatcher) to pg.LedgerRecorder: every
// applied-head record the provisioner emits is submitted onto the single meta writer.
type ledgerRecorder struct {
	submit Submitter
}

// compile-time proof the adapter satisfies the provisioner's ledger seam.
var _ pg.LedgerRecorder = ledgerRecorder{}

// NewLedgerRecorder returns a pg.LedgerRecorder that records applied migration heads
// through the single meta writer, so provisioning's ledger writes preserve the
// single-writer invariant like every other meta mutation.
func NewLedgerRecorder(submit Submitter) pg.LedgerRecorder {
	return ledgerRecorder{submit: submit}
}

// RecordMigrationHead submits the applied-head write onto the dispatcher goroutine,
// converting the head to store's shape. It blocks until the write completes (or the
// dispatcher stops / ctx is cancelled), like every submitted meta write.
func (r ledgerRecorder) RecordMigrationHead(ctx context.Context, head pg.MigrationHead) error {
	return r.submit.Submit(ctx, func(w *store.Writer) error {
		return w.RecordMigrationHead(ctx, store.MigrationHead{
			Schema:      head.Schema,
			Table:       head.Table,
			MigrationID: head.MigrationID,
			Parent:      head.Parent,
			Checksum:    head.Checksum,
		})
	})
}
