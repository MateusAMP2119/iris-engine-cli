package dispatch

import (
	"context"
	"fmt"

	"github.com/MateusAMP2119/iris-engine-cli/internal/store"
)

// This file is the dispatcher's snapshot-pin stamping. The pin is three values naming
// a run's inputs: at dispatch, the data database's LSN (snapshot_lsn) and journal
// high id (journal_floor); at the terminal transition, the journal high id again
// (journal_ceiling). Reading the LSN and the journal high id are data-database reads,
// not meta reads, so they enter through their own seams (LSNReader,
// JournalHighWatermark), fake-backed in tests; the stamped values then ride the
// single-writer run-record path.

// LSNReader reads the data database's current write LSN, the value pinned into a
// run's snapshot_lsn at dispatch. It is a data-database read seam, never a meta
// write; a real data-database-backed implementation and a fake both satisfy it.
type LSNReader interface {
	// CurrentLSN returns the data database's current LSN as its text form.
	CurrentLSN(ctx context.Context) (string, error)
}

// JournalHighWatermark reads the data journal's current high id, stamped into a run's
// journal_floor at dispatch and journal_ceiling at the terminal transition. It is a
// data-database read seam, never a meta write.
type JournalHighWatermark interface {
	// JournalHighID returns the data journal's current high id.
	JournalHighID(ctx context.Context) (int64, error)
}

// RunRecordWriter is the run-record write surface the pin stamping drives: minting the
// run with its dispatch-time pin values, and closing the journal window at the terminal
// transition. The store's single Writer satisfies it.
type RunRecordWriter interface {
	// CreateRun mints a run row and its consumption ledger in one atomic transaction.
	CreateRun(ctx context.Context, rec store.RunRecord) error
	// StampJournalCeiling records a run's terminal journal high id.
	StampJournalCeiling(ctx context.Context, runID string, ceiling int64) error
}

// StampDispatch pins a run's input state at dispatch and mints the run record. It
// reads the data database's LSN into rec.SnapshotLSN and the journal high id into
// rec.JournalFloor -- the two dispatch-time reads of the snapshot pin -- then hands
// the completed record to the run-record write path, so the created runs row carries
// the pin. Either data-database read failing aborts before any meta write, so a run
// is never minted with a half-formed pin.
func StampDispatch(ctx context.Context, w RunRecordWriter, lsn LSNReader, journal JournalHighWatermark, rec store.RunRecord) error {
	snapshotLSN, err := lsn.CurrentLSN(ctx)
	if err != nil {
		return fmt.Errorf("dispatch: read snapshot lsn at dispatch: %w", err)
	}
	floor, err := journal.JournalHighID(ctx)
	if err != nil {
		return fmt.Errorf("dispatch: read journal floor at dispatch: %w", err)
	}
	rec.SnapshotLSN = snapshotLSN
	rec.JournalFloor = floor
	if err := w.CreateRun(ctx, rec); err != nil {
		return fmt.Errorf("dispatch: create run record: %w", err)
	}
	return nil
}

// StampTerminal closes a run's journal window at its terminal transition. It reads
// the journal high id again -- the third and final read of the snapshot pin -- and
// stamps it as the run's journal_ceiling, delimiting the window that opened at
// dispatch. The ceiling read happens here, at the terminal transition, not at
// dispatch, so the window spans exactly the run's execution.
func StampTerminal(ctx context.Context, w RunRecordWriter, journal JournalHighWatermark, runID string) error {
	ceiling, err := journal.JournalHighID(ctx)
	if err != nil {
		return fmt.Errorf("dispatch: read journal ceiling at terminal transition: %w", err)
	}
	if err := w.StampJournalCeiling(ctx, runID, ceiling); err != nil {
		return fmt.Errorf("dispatch: stamp journal ceiling: %w", err)
	}
	return nil
}
