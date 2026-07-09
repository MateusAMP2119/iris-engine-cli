package store

import (
	"context"
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/binary"
	"fmt"
	"sort"
)

// This file is the stats rollup composition (specification sections 11 and 14):
// the engine-wide, per-lane, and per-pipeline read-only rollups `iris engine
// stats` and GET /stats serve identically. It defines the StatsSource read seam
// -- the plain-MVCC meta reads the rollup draws from -- and BuildStats, the pure
// composition over one snapshot of those reads plus the leader-held per-lane
// pass counts (a runtime count handed in by the daemon, never read from meta).
//
// Clock-free by construction: every rollup value is a current count over the
// snapshot or a last-value picked by ordering identity (the run and checkpoint
// identity keys, never recorded_at). Nothing here reads a clock, keeps history,
// or derives a rate.
//
// The journal-side counters (capture counter, wipe-eligible slice, journal
// size, hot rows) belong to the data journal, whose lifecycle machinery is
// E07's; JournalStats is the seam its readers will fill. The checkpoint chain
// head models meta's journal_checkpoints table (schema.go): the head is the
// highest-seq row, explicitly absent (nil) while no partition has ever sealed.

// DeadLetterEntry is one outstanding dead-letter worklist row as the stats
// rollup consumes it: the run and the closed-set reason.
type DeadLetterEntry struct {
	// RunID is the dead-lettered run's id.
	RunID string
	// Reason is the worklist entry's reason (the dead_letters.reason set).
	Reason DeadLetterReason
}

// LaneMember is one persisted composer row as the stats rollup consumes it: a
// pipeline's lane membership.
type LaneMember struct {
	// Lane is the lane's name.
	Lane string
	// Pipeline is the member pipeline's name.
	Pipeline string
}

// JournalStats are the data journal's current counters: the capture counter,
// the wipe-eligible slice, total size, and the hot slice -- all row counts
// (count-based like the partition threshold, specification section 14), never
// bytes-per-anything. E07's journal readers fill it; until then only fakes do.
type JournalStats struct {
	// CapturedWrites is the capture counter: journal entries recorded.
	CapturedWrites int64
	// WipeEligibleRows is the wipe-eligible slice: un-promoted disposable rows
	// a workload wipe would revert.
	WipeEligibleRows int64
	// TotalRows is the journal's total size as a row count.
	TotalRows int64
	// HotRows is the hot slice: rows in unsealed partitions.
	HotRows int64
}

// Checkpoint is one journal_checkpoints row as the stats rollup consumes it:
// the insert-order identity, the chained digest, and the sealed partition's
// location (resident or archived).
type Checkpoint struct {
	// Seq is the checkpoint's insert-order identity (journal_checkpoints.seq).
	Seq int64
	// Digest is the chained digest, hex-encoded.
	Digest string
	// Location is the sealed partition's location: resident or archived.
	Location string
}

// StatsSource is the read seam the stats rollup draws from: one plain-MVCC
// snapshot of runs, the dead-letter worklist, the persisted composer, the
// registry, the journal counters, and the checkpoint chain. A meta-backed
// implementation and the storetest fake both satisfy it; reads are never
// serialized through the writer and never retried.
type StatsSource interface {
	// Runs returns the runs matching filter, in ordering-identity order.
	Runs(ctx context.Context, filter RunFilter) ([]Run, error)
	// DeadLetters returns the outstanding dead-letter worklist entries.
	DeadLetters(ctx context.Context) ([]DeadLetterEntry, error)
	// LaneMembers returns the persisted composer rows (lane membership).
	LaneMembers(ctx context.Context) ([]LaneMember, error)
	// PipelineNames returns every registered pipeline's name.
	PipelineNames(ctx context.Context) ([]string, error)
	// Journal returns the data journal's current counters.
	Journal(ctx context.Context) (JournalStats, error)
	// Checkpoints returns the checkpoint chain's rows.
	Checkpoints(ctx context.Context) ([]Checkpoint, error)
}

// EngineRollup is the engine-wide stats rollup (specification sections 11 and
// 14): the dead-letter worklist depth and per-reason counts, running runs, the
// capture counters, the wipe-eligible slice, total journal size, and the
// lifecycle readout (hot rows, sealed and archived partition counts, checkpoint
// chain head).
type EngineRollup struct {
	// DeadLetterDepth is the outstanding worklist depth.
	DeadLetterDepth int64
	// DeadLettersByReason counts outstanding entries per reason; never nil.
	DeadLettersByReason map[string]int64
	// RunningRuns counts runs currently running.
	RunningRuns int64
	// CapturedWrites is the capture counter.
	CapturedWrites int64
	// WipeEligibleRows is the wipe-eligible slice.
	WipeEligibleRows int64
	// JournalRows is the journal's total row count.
	JournalRows int64
	// HotRows is the unsealed (hot) row count.
	HotRows int64
	// SealedPartitions counts sealed partitions (checkpoint rows).
	SealedPartitions int64
	// ArchivedPartitions counts sealed partitions exported and dropped.
	ArchivedPartitions int64
	// ChainHead is the checkpoint chain's head (highest seq), nil while no
	// partition has ever sealed -- explicit absence, never a zero row.
	ChainHead *Checkpoint
}

// LaneRollup is one lane's stats rollup: pipeline count, queued/running count,
// and loop passes completed since daemon start (the leader-held count).
type LaneRollup struct {
	// Lane is the lane's name.
	Lane string
	// Pipelines counts the lane's registered member pipelines.
	Pipelines int64
	// Queued counts queued runs across the lane's members.
	Queued int64
	// Running counts running runs across the lane's members.
	Running int64
	// Passes is the lane's loop passes completed since daemon start.
	Passes int64
}

// PipelineRollup is one pipeline's stats rollup: latest run state, run counts
// by state, last exit code, and last run id.
type PipelineRollup struct {
	// Pipeline is the registered pipeline's name.
	Pipeline string
	// LatestRunState is the most recent run's state, "" with no runs.
	LatestRunState string
	// RunsByState counts the pipeline's runs per state; never nil.
	RunsByState map[string]int64
	// LastExitCode is the most recent recorded exit code, nil while none.
	LastExitCode *int
	// LastRunID is the most recent run's id, "" with no runs.
	LastRunID string
}

// StatsRollup is the full stats document BuildStats composes: the engine-wide
// rollup plus the per-lane and per-pipeline rollups in name order.
type StatsRollup struct {
	// Engine is the engine-wide rollup.
	Engine EngineRollup
	// Lanes are the per-lane rollups, in lane-name order.
	Lanes []LaneRollup
	// Pipelines are the per-pipeline rollups, in pipeline-name order.
	Pipelines []PipelineRollup
}

// BuildStats composes the stats rollup from one snapshot of the source reads
// plus the leader-held per-lane pass counts (specification section 11). passes
// maps lane name to loop passes completed since daemon start; a lane absent
// from the map (or a nil map, on a standby that never dispatched) reads zero.
// The composition is pure over its inputs: no clock, no history, no rate --
// counts over the snapshot and last-values by ordering identity only.
func BuildStats(ctx context.Context, src StatsSource, passes map[string]int64) (StatsRollup, error) {
	runs, err := src.Runs(ctx, RunFilter{})
	if err != nil {
		return StatsRollup{}, fmt.Errorf("store: stats runs read: %w", err)
	}
	deadLetters, err := src.DeadLetters(ctx)
	if err != nil {
		return StatsRollup{}, fmt.Errorf("store: stats dead-letter read: %w", err)
	}
	members, err := src.LaneMembers(ctx)
	if err != nil {
		return StatsRollup{}, fmt.Errorf("store: stats lane read: %w", err)
	}
	pipelines, err := src.PipelineNames(ctx)
	if err != nil {
		return StatsRollup{}, fmt.Errorf("store: stats pipeline read: %w", err)
	}
	journal, err := src.Journal(ctx)
	if err != nil {
		return StatsRollup{}, fmt.Errorf("store: stats journal read: %w", err)
	}
	checkpoints, err := src.Checkpoints(ctx)
	if err != nil {
		return StatsRollup{}, fmt.Errorf("store: stats checkpoint read: %w", err)
	}

	return StatsRollup{
		Engine:    engineRollup(runs, deadLetters, journal, checkpoints),
		Lanes:     laneRollups(runs, members, passes),
		Pipelines: pipelineRollups(runs, pipelines),
	}, nil
}

// engineRollup composes the engine-wide rollup: current counts over the
// snapshot, plus the chain head picked by checkpoint identity (highest seq).
func engineRollup(runs []Run, deadLetters []DeadLetterEntry, journal JournalStats, checkpoints []Checkpoint) EngineRollup {
	byReason := make(map[string]int64)
	for _, dl := range deadLetters {
		byReason[string(dl.Reason)]++
	}
	var running int64
	for _, r := range runs {
		if r.State == RunRunning {
			running++
		}
	}
	var archived int64
	var head *Checkpoint
	for _, cp := range checkpoints {
		if cp.Location == "archived" {
			archived++
		}
		if head == nil || cp.Seq > head.Seq {
			c := cp
			head = &c
		}
	}
	return EngineRollup{
		DeadLetterDepth:     int64(len(deadLetters)),
		DeadLettersByReason: byReason,
		RunningRuns:         running,
		CapturedWrites:      journal.CapturedWrites,
		WipeEligibleRows:    journal.WipeEligibleRows,
		JournalRows:         journal.TotalRows,
		HotRows:             journal.HotRows,
		SealedPartitions:    int64(len(checkpoints)),
		ArchivedPartitions:  archived,
		ChainHead:           head,
	}
}

// laneRollups composes the per-lane rollups: membership counts from the
// persisted composer, queued/running counts joined pipeline->lane over the run
// snapshot, and the leader-held pass count (zero when absent).
func laneRollups(runs []Run, members []LaneMember, passes map[string]int64) []LaneRollup {
	laneOf := make(map[string]string, len(members))
	memberCount := make(map[string]int64)
	for _, m := range members {
		laneOf[m.Pipeline] = m.Lane
		memberCount[m.Lane]++
	}
	queued := make(map[string]int64)
	running := make(map[string]int64)
	for _, r := range runs {
		lane, ok := laneOf[r.Pipeline]
		if !ok {
			continue // a run of a pipeline no longer in any lane rolls into no lane
		}
		switch r.State {
		case RunQueued:
			queued[lane]++
		case RunRunning:
			running[lane]++
		}
	}

	lanes := make([]string, 0, len(memberCount))
	for lane := range memberCount {
		lanes = append(lanes, lane)
	}
	sort.Strings(lanes)

	out := make([]LaneRollup, 0, len(lanes))
	for _, lane := range lanes {
		out = append(out, LaneRollup{
			Lane:      lane,
			Pipelines: memberCount[lane],
			Queued:    queued[lane],
			Running:   running[lane],
			Passes:    passes[lane],
		})
	}
	return out
}

// pipelineRollups composes the per-pipeline rollups over the run snapshot:
// counts by state, and the last-values (latest state, last run id, last exit
// code) picked by the runs' ordering identity, never a clock. Every registered
// pipeline appears, including ones that never ran.
func pipelineRollups(runs []Run, pipelines []string) []PipelineRollup {
	sorted := append([]string(nil), pipelines...)
	sort.Strings(sorted)

	byPipeline := make(map[string][]Run)
	for _, r := range runs {
		byPipeline[r.Pipeline] = append(byPipeline[r.Pipeline], r)
	}

	out := make([]PipelineRollup, 0, len(sorted))
	for _, name := range sorted {
		roll := PipelineRollup{Pipeline: name, RunsByState: make(map[string]int64)}
		var latest *Run
		var lastExit *int
		var lastExitSeq int64
		for i := range byPipeline[name] {
			r := &byPipeline[name][i]
			roll.RunsByState[string(r.State)]++
			if latest == nil || r.Seq > latest.Seq {
				latest = r
			}
			if r.ExitCode != nil && (lastExit == nil || r.Seq > lastExitSeq) {
				code := *r.ExitCode
				lastExit = &code
				lastExitSeq = r.Seq
			}
		}
		if latest != nil {
			roll.LatestRunState = string(latest.State)
			roll.LastRunID = latest.ID
		}
		roll.LastExitCode = lastExit
		out = append(out, roll)
	}
	return out
}

// CheckpointRow is the full row model for journal_checkpoints used by chain
// logic, sealing, and insert (specification sections 4 and 14). The slim
// Checkpoint is the stats projection only.
type CheckpointRow struct {
	Seq          int64
	IDFrom       int64
	IDTo         int64
	Digest       []byte
	ParentDigest []byte
	Signature    []byte
	Location     string
	RecordedAt   string
}

// ComputeDigest is the pure unit hash over compacted rows (or their byte
// representations) in id order. Uses stdlib sha256. This is the digest stored
// in a checkpoint (S14/checkpoint-digest-chain, S04/checkpoint-per-sealed-partition).
func ComputeDigest(compacted [][]byte) []byte {
	h := sha256.New()
	for _, p := range compacted {
		var lbuf [8]byte
		binary.BigEndian.PutUint64(lbuf[:], uint64(len(p)))
		h.Write(lbuf[:])
		h.Write(p)
	}
	return h.Sum(nil)
}

// ParentFor returns the parent_digest value to use for a new checkpoint
// following prev (nil for the first checkpoint in the chain).
func ParentFor(prev *CheckpointRow) []byte {
	if prev == nil || len(prev.Digest) == 0 {
		return nil
	}
	d := make([]byte, len(prev.Digest))
	copy(d, prev.Digest)
	return d
}

// ValidateChain checks the parent_digest links (and signatures if pub != nil).
// A break (tamper, loss, bad sig) returns error visibly. Pure unit logic
// (S14/chain-detects-tamper, S04/checkpoint-parent-chain).
func ValidateChain(cps []CheckpointRow, pub ed25519.PublicKey) error {
	for i := 1; i < len(cps); i++ {
		if !bytesEqual(cps[i].ParentDigest, cps[i-1].Digest) {
			return fmt.Errorf("checkpoint parent chain broken at seq %d (parent %x != prev %x)", cps[i].Seq, cps[i].ParentDigest, cps[i-1].Digest)
		}
	}
	if len(pub) == ed25519.PublicKeySize && len(cps) > 0 {
		for _, cp := range cps {
			if len(cp.Signature) > 0 && !ed25519.Verify(pub, cp.Digest, cp.Signature) {
				return fmt.Errorf("checkpoint signature invalid at seq %d", cp.Seq)
			}
		}
	}
	return nil
}

// CheckpointForSealed constructs exactly one journal_checkpoints row for a sealed
// partition: id_from/id_to, digest = ComputeDigest over the compacted rows in id
// order, parent_digest chained via ParentFor. Signature is left for the engine
// key signer. Initial location "resident". Pure unit logic.
// (S04/checkpoint-per-sealed-partition, S14/checkpoint-digest-chain)
func CheckpointForSealed(idFrom, idTo int64, compacted [][]byte, prev *CheckpointRow) CheckpointRow {
	return CheckpointRow{
		IDFrom:       idFrom,
		IDTo:         idTo,
		Digest:       ComputeDigest(compacted),
		ParentDigest: ParentFor(prev),
		Location:     "resident",
	}
}

func bytesEqual(a, b []byte) bool {
	if len(a) == 0 && len(b) == 0 {
		return true
	}
	return string(a) == string(b)
}
