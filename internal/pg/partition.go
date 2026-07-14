package pg

import (
	"fmt"
	"sort"
	"strconv"
)

// This file models the data journal's id-range partitioning. data_journal is
// declared PARTITION BY RANGE (id); its rows are carved into partitions by a
// count-based threshold, journal_partition_rows
// (default 10M, configurable). The threshold is not an exact cap: a partition
// seals only once it holds at least the threshold's worth of rows AND every
// in-flight run writing into it has finished AND it holds zero open entries.
//
// Two invariants follow and are modeled here. First, a run's journal window never
// splits across partitions: because a seal waits for in-flight runs to finish, a
// partition boundary always lands at or beyond every open run's ceiling, so a
// run's stamps share one partition (else per-run compaction would break). Second,
// wipe and promote touch only unsealed partitions: sealed history is immutable by
// construction.
//
// The DDL rendered here (PARTITION OF ... FOR VALUES FROM ... TO ...) is
// deterministic, so a golden diff is a contract diff. Sealing, compaction, and
// checkpointing the sealed partitions is downstream (E07); this file owns only the
// partition shape and the boundary-placement rule.

// Partition is one id-range partition of public.data_journal: the half-open id
// window it holds and whether it has sealed. Journal ids are a monotonic bigint
// identity starting at 1, so 0 is never a valid id and marks an unbounded end:
// From == 0 renders MINVALUE (the very first partition's lower bound) and To == 0
// renders MAXVALUE (the open, unsealed tail new stamps land in). A sealed
// partition is immutable by construction.
type Partition struct {
	// Seq is the partition's ordinal in id order (0 is the first partition).
	Seq int64
	// From is the inclusive lower id bound; 0 renders MINVALUE.
	From int64
	// To is the exclusive upper id bound; 0 renders MAXVALUE (the open tail).
	To int64
	// Sealed marks a partition past sealing: immutable by construction. Wipe and
	// promote never touch a sealed partition.
	Sealed bool
}

// Name is the partition's table name, derived from the journal name and its
// sequence, e.g. "data_journal_p0". Engine-owned and recognizable, it never
// collides with a user table.
func (p Partition) Name() string {
	return fmt.Sprintf("%s_p%d", JournalName, p.Seq)
}

// Qualified returns the schema-qualified partition name, e.g.
// "public.data_journal_p0".
func (p Partition) Qualified() string {
	return JournalSchema + "." + p.Name()
}

// Mutable reports whether wipe and promote may touch this partition: true for an
// unsealed partition, false for a sealed one (immutable by construction).
func (p Partition) Mutable() bool { return !p.Sealed }

// CreateDDL renders the partition as a create-if-missing PARTITION OF the journal
// over its id range: CREATE TABLE IF NOT EXISTS <partition> PARTITION OF
// public.data_journal FOR VALUES FROM (<from>) TO (<to>). An unbounded end renders
// MINVALUE or MAXVALUE. Like the parent journal DDL it is idempotent, applied at
// provisioning and re-checkable, with no ALTER or migration ledger.
func (p Partition) CreateDDL() string {
	return fmt.Sprintf("CREATE TABLE IF NOT EXISTS %s PARTITION OF %s FOR VALUES FROM (%s) TO (%s);",
		p.Qualified(), JournalTable().Qualified(), boundLiteral(p.From, "MINVALUE"), boundLiteral(p.To, "MAXVALUE"))
}

// boundLiteral renders a partition bound: the id as a decimal literal, or the
// unbounded sentinel (0) as the given MINVALUE/MAXVALUE keyword.
func boundLiteral(id int64, unbounded string) string {
	if id == 0 {
		return unbounded
	}
	return strconv.FormatInt(id, 10)
}

// InitialPartition is the journal's bootstrap partition: the single open, unsealed
// tail spanning the whole id space (MINVALUE..MAXVALUE), created alongside the
// parent so the partitioned journal is writable at provisioning. Sealing (E07)
// later cuts threshold-sized partitions off its head under the
// journal_partition_rows rule.
func InitialPartition() Partition {
	return Partition{Seq: 0, From: 0, To: 0, Sealed: false}
}

// MutablePartitions returns the partitions wipe and promote may touch: the
// unsealed ones, in input order. Sealed partitions are immutable by construction,
// so neither wipe (E06.5) nor promote (E06.6) ever includes one. Both operations
// run over exactly this set.
func MutablePartitions(parts []Partition) []Partition {
	var out []Partition
	for _, p := range parts {
		if p.Mutable() {
			out = append(out, p)
		}
	}
	return out
}

// RunWindow is a run's journal window: the inclusive id range [Floor, Ceiling] its
// stamps occupy (runs.journal_floor at dispatch, runs.journal_ceiling at terminal
// transition). Stamps of concurrently in-flight runs interleave within a window;
// the partition invariant is that the whole window stays within one partition.
type RunWindow struct {
	// RunID is the run this window belongs to (runs.id).
	RunID int64
	// Floor is the journal high id at dispatch: the window's low id.
	Floor int64
	// Ceiling is the journal high id at the run's terminal transition: the
	// window's high id.
	Ceiling int64
}

// Rows is the number of ids the window spans (inclusive of both ends).
func (w RunWindow) Rows() int64 { return w.Ceiling - w.Floor + 1 }

// PartitionPlan sizes journal partitions by id range under a row-count threshold.
// It is the pure model behind sealing: it decides where a partition boundary may
// fall so that each sealed partition holds at least the threshold's worth of rows
// AND no run's journal window ever straddles a boundary.
type PartitionPlan struct {
	// Threshold is journal_partition_rows: the number of rows a partition must
	// hold before it may seal. A threshold, never an exact cap -- a partition may
	// hold more (a run larger than the threshold, or a long dev loop, delays its
	// own seal); it never holds a split run window.
	Threshold int64
}

// Boundaries returns the exclusive id boundaries at which the plan cuts sealed
// partitions off the journal head, given every run that has written to the
// journal, in any order. A boundary b seals the ids below it into one partition
// and leaves the rest to the tail; PartitionOf maps an id to its partition index
// against the returned boundaries.
//
// Each boundary lies past the threshold's worth of rows AND at or beyond every
// in-flight run's ceiling, so no run window is ever split (this is the seal rule:
// a partition seals only once every in-flight run writing into it has finished).
// Journal ids in the resident tail are contiguous, so a boundary's id offset from
// the partition start counts its rows. A threshold of zero or no runs yields no
// boundaries.
func (pp PartitionPlan) Boundaries(runs []RunWindow) []int64 {
	if pp.Threshold <= 0 || len(runs) == 0 {
		return nil
	}
	ordered := make([]RunWindow, len(runs))
	copy(ordered, runs)
	sort.Slice(ordered, func(i, j int) bool { return ordered[i].Floor < ordered[j].Floor })

	start := ordered[0].Floor
	var maxCeiling int64
	for _, r := range ordered {
		if r.Ceiling > maxCeiling {
			maxCeiling = r.Ceiling
		}
	}

	var boundaries []int64
	for {
		b := safeBoundary(start+pp.Threshold, ordered)
		// A boundary past the last written id is the open tail, not a cut.
		if b > maxCeiling {
			break
		}
		boundaries = append(boundaries, b)
		start = b
	}
	return boundaries
}

// safeBoundary returns the smallest boundary at or above target that splits no run
// window. A boundary straddling an in-flight run is pushed past that run's ceiling
// and re-checked, modeling the seal rule that a partition seals only once every
// in-flight run writing into it has finished. The result rises monotonically and
// is bounded by the highest ceiling plus one, so the loop always terminates.
func safeBoundary(target int64, ordered []RunWindow) int64 {
	b := target
	for {
		next := b
		for _, r := range ordered {
			if splitsWindow(b, r) && r.Ceiling+1 > next {
				next = r.Ceiling + 1
			}
		}
		if next == b {
			return b
		}
		b = next
	}
}

// splitsWindow reports whether an exclusive boundary b cuts window w: some of w's
// ids fall below b and some at or above it, i.e. Floor < b <= Ceiling.
func splitsWindow(b int64, w RunWindow) bool { return w.Floor < b && b <= w.Ceiling }

// PartitionOf returns the zero-based index of the partition holding id, given the
// ascending exclusive boundaries a PartitionPlan produced: the count of boundaries
// at or below id. Two ids share a partition exactly when PartitionOf returns the
// same index for both, so a run window stays in one partition iff PartitionOf of
// its floor equals PartitionOf of its ceiling.
func PartitionOf(boundaries []int64, id int64) int {
	// boundaries are ascending, so the partition index is the count of boundaries
	// at or below id: the first boundary strictly greater than id.
	return sort.Search(len(boundaries), func(i int) bool { return id < boundaries[i] })
}

// CanSeal reports whether a journal partition is sealable under the seal
// condition: it must have crossed the row threshold, hold zero open (undo=open)
// entries, and have no in-flight runs still writing into it (every run that wrote
// into the partition range has finished).
func CanSeal(threshold, rows int64, openEntries, overlappingInflight int) bool {
	if threshold <= 0 || rows < threshold {
		return false
	}
	if openEntries != 0 {
		return false
	}
	if overlappingInflight != 0 {
		return false
	}
	return true
}
