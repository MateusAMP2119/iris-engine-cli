package daemon

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"time"

	"github.com/MateusAMP2119/iris-engine-cli/internal/archive"
	"github.com/MateusAMP2119/iris-engine-cli/internal/dispatch"
	"github.com/MateusAMP2119/iris-engine-cli/internal/store"
)

// This file is the leader-side journal seal step (specification section 14): the
// opportunistic dispatcher action that runs after a terminal run and, when the
// resident partition has crossed the journal_partition_rows threshold with no
// in-flight run left writing into it, seals that partition -- compact, checkpoint,
// archive. It replaces the E13.5 presence stub (a seal that fired on every terminal
// run with a placeholder digest, a fixed key, and a hardcoded id range) with the
// real, spec-backed flow over the E07.3 seal primitives and the E07.4 checkpoint
// chain and engine key.
//
// Seal timing is the spec's seal condition (S14/seal-condition): a partition seals
// only when it is past the row threshold, every in-flight run writing into it has
// finished, and it holds zero open entries. The threshold is journal_partition_rows,
// a threshold and not an exact cap -- a run larger than the threshold, or a long dev
// loop, delays its own seal rather than splitting a run's journal window. The
// resident partition is sealed whole (its rows share one partition by construction,
// since a seal waits for in-flight runs), so the in-flight guard is: seal only when
// no run is currently running. The just-finished run that triggers this step has
// already been recorded terminal before the step runs, so it never counts itself.

// sealDue is the pure seal condition (specification section 14,
// S14/seal-condition): a partition seals only when it is past the row threshold,
// every in-flight run writing into it has finished (no run is running), and it holds
// rows at all. A non-positive threshold disables sealing (the resident tail grows
// unbounded, a deliberate opt-out); a below-threshold partition, an empty partition,
// or any in-flight run all defer the seal.
func sealDue(residentRows, threshold, runningRuns int64) bool {
	if threshold <= 0 {
		return false
	}
	if residentRows <= 0 {
		return false
	}
	if residentRows < threshold {
		return false
	}
	if runningRuns > 0 {
		return false
	}
	return true
}

// sealDataStore is the data-database side the seal reads and mutates: the resident
// partition's stats, the range compaction, the compacted-row read for the digest,
// and the detach-and-drop. *pg.Client satisfies it; a fake stands in at integration
// tier.
type sealDataStore interface {
	// ResidentJournalStats reports the resident partition's row count and inclusive
	// id span (all zero when empty).
	ResidentJournalStats(ctx context.Context) (count, minID, maxID int64, err error)
	// ResidentRunIDs returns the distinct run ids that wrote entries into the resident
	// partition (a no-op run that wrote nothing never appears).
	ResidentRunIDs(ctx context.Context) ([]int64, error)
	// CompactJournalRange nulls released pre-images and folds duplicate stamps over
	// the half-open id range [from, to).
	CompactJournalRange(ctx context.Context, from, to int64) error
	// QueryCompactedRows returns the compacted rows in [from, to) in id order, the
	// canonical serialization the checkpoint digest is computed over.
	QueryCompactedRows(ctx context.Context, from, to int64) ([][]byte, error)
	// DropPartitionForRange detaches and drops the sealed partition and recreates an
	// empty tail so the journal stays writable.
	DropPartitionForRange(ctx context.Context, seq int64) error
}

// journalSealer is the leader-side seal step. It composes the data store, the meta
// read seam (chain head, in-flight count, engine key), the single-writer submitter
// (checkpoint insert, archive flip, and the create-once engine-key mint), and the
// object store (partition export). The checkpoint is signed with the engine key the
// seal loads from the engine_key meta table (minted on first need). A nil sealer, a
// non-positive threshold, or a nil data/meta seam makes the step a no-op, so the
// shape tests that wire no sealer never seal.
type journalSealer struct {
	threshold int64
	data      sealDataStore
	meta      store.JournalSealReader
	submitter dispatch.Submitter
	objects   *store.ObjectStore
	logger    *slog.Logger
}

// newJournalSealer builds the seal step. The engine key the seal signs the
// checkpoint with is loaded from the engine_key meta table through meta, minted
// create-once on first need through the submitter. A nil logger discards.
func newJournalSealer(threshold int64, data sealDataStore, meta store.JournalSealReader, submitter dispatch.Submitter, objects *store.ObjectStore, logger *slog.Logger) *journalSealer {
	if logger == nil {
		logger = slog.New(slog.NewTextHandler(io.Discard, nil))
	}
	return &journalSealer{
		threshold: threshold,
		data:      data,
		meta:      meta,
		submitter: submitter,
		objects:   objects,
		logger:    logger,
	}
}

// loadOrMintEngineKey reads the engine key from the engine_key meta table and, when
// the table holds no key yet, mints one create-once: it generates a fresh ed25519
// key, inserts it through the single writer (INSERT ... ON CONFLICT (id) DO NOTHING,
// so a concurrent minter never forks the key), then reads back whichever key won.
// Install normally mints the key, so on a healthy engine the first read finds it;
// the mint-on-first-need path covers an engine installed before the key existed. A
// read-back that still finds no key after minting is a hard error, so the seal
// defers rather than signing with an unstored key.
func (s *journalSealer) loadOrMintEngineKey(ctx context.Context) (EngineKey, error) {
	raw, err := s.meta.ReadEngineKey(ctx)
	if err != nil {
		return EngineKey{}, err
	}
	if raw == nil {
		key, err := MintEngineKey()
		if err != nil {
			return EngineKey{}, err
		}
		if err := s.submitter.Submit(ctx, func(w *store.Writer) error {
			return w.InsertEngineKey(ctx, key.privateBytes(), time.Now().UTC().Format(time.RFC3339Nano))
		}); err != nil {
			return EngineKey{}, err
		}
		raw, err = s.meta.ReadEngineKey(ctx)
		if err != nil {
			return EngineKey{}, err
		}
		if raw == nil {
			return EngineKey{}, fmt.Errorf("daemon: engine key absent after mint")
		}
	}
	return DecodeEngineKeyBytes(raw)
}

// sealAfterPass is the opportunistic dispatcher step (specification section 14,
// S14/seal-dispatcher-step): after a run reaches terminal, seal the resident
// partition if it is due -- compact, checkpoint, archive. Every step is
// best-effort: a read error, a not-due partition, or a missing engine key leaves the
// journal untouched and the tail resident (still fully answerable by provenance),
// and the next terminal run retries. Errors are logged, never propagated to the run:
// a run's success or failure never hinges on whether its post-pass seal fired.
func (s *journalSealer) sealAfterPass(ctx context.Context) {
	if s == nil || s.data == nil || s.meta == nil || s.submitter == nil || s.objects == nil {
		return
	}
	if s.threshold <= 0 {
		return
	}

	count, minID, maxID, err := s.data.ResidentJournalStats(ctx)
	if err != nil {
		s.logger.Warn("seal: read resident journal stats", "err", err)
		return
	}
	// Only runs that actually wrote into the resident partition can split a window, so
	// the in-flight guard counts running runs among the partition's writers -- an
	// idle-lane no-op pass wrote nothing and never defers a seal (specification section
	// 14: the unit is the run, never its lane).
	writers, err := s.data.ResidentRunIDs(ctx)
	if err != nil {
		s.logger.Warn("seal: read resident run ids", "err", err)
		return
	}
	running, err := s.meta.RunningAmong(ctx, writers)
	if err != nil {
		s.logger.Warn("seal: read in-flight writer count", "err", err)
		return
	}
	if !sealDue(count, s.threshold, running) {
		return
	}
	// A due partition has rows; guard the id span defensively.
	if minID <= 0 || maxID < minID {
		return
	}

	// The engine key signs the checkpoint digest; without it a seal cannot produce a
	// verifiable checkpoint, so it defers (rows stay resident) rather than writing an
	// unsigned or placeholder one. The key is loaded from the engine_key meta table
	// (minted create-once on first need), superuser-free in external mode and shared
	// across daemon processes via the meta database standbys already read.
	key, err := s.loadOrMintEngineKey(ctx)
	if err != nil {
		s.logger.Warn("seal: load engine key", "err", err)
		return
	}

	upper := maxID + 1 // half-open upper bound over the inclusive [minID, maxID] span

	// 1. Compact: null released pre-images, fold duplicate stamps to the latest op.
	if err := s.data.CompactJournalRange(ctx, minID, upper); err != nil {
		s.logger.Warn("seal: compact range", "from", minID, "to", upper, "err", err)
		return
	}
	rows, err := s.data.QueryCompactedRows(ctx, minID, upper)
	if err != nil {
		s.logger.Warn("seal: read compacted rows", "err", err)
		return
	}

	// 2. Checkpoint: digest over the compacted rows in id order, chained to the
	// current head, signed with the engine key. The id range is the sealed
	// partition's actual span, not a placeholder.
	prev, err := s.meta.LatestCheckpoint(ctx)
	if err != nil {
		s.logger.Warn("seal: read chain head", "err", err)
		return
	}
	cp := store.CheckpointForSealed(minID, maxID, rows, prev)
	sig, err := key.SignDigest(cp.Digest)
	if err != nil {
		s.logger.Warn("seal: sign checkpoint digest", "err", err)
		return
	}
	cp.Signature = sig
	cp.Location = "resident"
	cp.RecordedAt = time.Now().UTC().Format(time.RFC3339Nano)
	if err := s.submitter.Submit(ctx, func(w *store.Writer) error { return w.InsertCheckpoint(ctx, cp) }); err != nil {
		s.logger.Warn("seal: insert checkpoint", "err", err)
		return
	}

	// 3. Archive: export the partition under its checkpoint digest key, re-validate,
	// detach and drop it, and flip the checkpoint to archived. On any archive failure
	// the checkpoint stays resident and the rows stay in Postgres, so sealed history
	// is always answerable -- from the object store once archived, from the resident
	// tail until then.
	digestKey := fmt.Sprintf("%x", cp.Digest)
	header := archive.Header{IDFrom: minID, IDTo: maxID, Digest: cp.Digest, Signature: sig}
	dropper := sealPartitionDropper{data: s.data}
	flipper := sealCheckpointFlipper{submitter: s.submitter}
	if err := archive.Export(s.objects, dropper, flipper, digestKey, header, rows); err != nil {
		s.logger.Warn("seal: export partition; checkpoint remains resident", "digest", digestKey, "err", err)
		return
	}
}

// sealPartitionDropper adapts the data store's DropPartitionForRange to the archive
// export flow's PartitionDropper seam.
type sealPartitionDropper struct {
	data sealDataStore
}

// Drop detaches and drops the sealed partition (the resident tail, whose id range
// the caller passes for the archive flow's bookkeeping) and recreates an empty tail
// so the journal stays writable. The data store's drop is keyed on the single
// resident partition, so it needs no id range of its own.
func (d sealPartitionDropper) Drop(_, _ int64) error {
	return d.data.DropPartitionForRange(context.Background(), 0)
}

// sealCheckpointFlipper adapts the single-writer submitter to the archive export
// flow's MetaFlipper seam, flipping the checkpoint location to archived through the
// one meta writer.
type sealCheckpointFlipper struct {
	submitter dispatch.Submitter
}

// Archive flips the checkpoint identified by its digest to archived through the
// single writer.
func (f sealCheckpointFlipper) Archive(digest []byte) error {
	return f.submitter.Submit(context.Background(), func(w *store.Writer) error {
		return w.ArchiveCheckpoint(context.Background(), digest)
	})
}
