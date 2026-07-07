package pg_test

import (
	"context"
	"path/filepath"
	"strings"
	"testing"

	"github.com/MateusAMP2119/iris-engine-cli/internal/config"
	"github.com/MateusAMP2119/iris-engine-cli/internal/golden"
	"github.com/MateusAMP2119/iris-engine-cli/internal/pg"
	"github.com/MateusAMP2119/iris-engine-cli/internal/pg/pgtest"
)

// windowSplit reports whether the run window [floor, ceiling] straddles any
// partition boundary: its first and last stamp land in different partitions.
func windowSplit(boundaries []int64, w pg.RunWindow) bool {
	return pg.PartitionOf(boundaries, w.Floor) != pg.PartitionOf(boundaries, w.Ceiling)
}

// TestRunStampsOnePartition proves the partition plan never splits a run's
// journal window across a boundary: because a partition seals only once every
// in-flight run writing into it has finished, a boundary always lands at or
// beyond every open run's ceiling, so all of a run's stamps share one partition
// (else per-run compaction would break). A run larger than the threshold, and a
// run interleaved with others in flight, both stay whole -- the threshold is not
// a cut point mid-run.
//
// spec: S14/run-stamps-one-partition
func TestRunStampsOnePartition(t *testing.T) {
	cases := []struct {
		name      string
		threshold int64
		runs      []pg.RunWindow
		// wantBoundaries is the exact set of cut points the plan places; the
		// invariant assertion (no window split) is checked for every case.
		wantBoundaries []int64
	}{
		{
			name:           "empty journal never cuts",
			threshold:      10,
			runs:           nil,
			wantBoundaries: nil,
		},
		{
			name:      "single small runs cut near the threshold",
			threshold: 100,
			runs: []pg.RunWindow{
				{RunID: 1, Floor: 1, Ceiling: 50},
				{RunID: 2, Floor: 51, Ceiling: 120},
				{RunID: 3, Floor: 121, Ceiling: 130},
				{RunID: 4, Floor: 131, Ceiling: 260},
			},
			// start=1, target=101 lands inside run 2 [51,120] -> pushed to 121;
			// next start=121, target=221 lands inside run 4 [131,260] -> pushed to 261,
			// which is past the last ceiling, so it is the open tail, not a cut.
			wantBoundaries: []int64{121},
		},
		{
			name:      "a run larger than the threshold is never split",
			threshold: 100,
			runs: []pg.RunWindow{
				{RunID: 7, Floor: 1, Ceiling: 300}, // 300 rows, 3x the threshold
			},
			// The only run spans the whole space; the threshold cannot cut inside it,
			// so a long dev loop just delays its own seal: one partition, 300 rows.
			wantBoundaries: nil,
		},
		{
			name:      "interleaved in-flight runs cut only past every open ceiling",
			threshold: 10,
			runs: []pg.RunWindow{
				// Two runs in flight together: their windows overlap in id space.
				{RunID: 10, Floor: 1, Ceiling: 25},
				{RunID: 11, Floor: 5, Ceiling: 40},
				{RunID: 12, Floor: 41, Ceiling: 45},
			},
			// target=11 lands inside both open runs; the boundary is pushed past the
			// max ceiling of the interleaved pair (40) to 41. Run 12 [41,45] then
			// completes the tail.
			wantBoundaries: []int64{41},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			plan := pg.PartitionPlan{Threshold: tc.threshold}
			got := plan.Boundaries(tc.runs)

			if !equalInts(got, tc.wantBoundaries) {
				t.Fatalf("Boundaries = %v, want %v", got, tc.wantBoundaries)
			}

			// The core invariant: no run window is ever split by a boundary.
			for _, w := range tc.runs {
				if windowSplit(got, w) {
					t.Errorf("run %d window [%d,%d] split across boundaries %v: floor in partition %d, ceiling in partition %d",
						w.RunID, w.Floor, w.Ceiling, got,
						pg.PartitionOf(got, w.Floor), pg.PartitionOf(got, w.Ceiling))
				}
			}
		})
	}
}

// TestWipePromoteUnsealedOnly proves wipe and promote touch only unsealed
// partitions: a sealed partition is immutable by construction, so the shared
// eligibility classification (MutablePartitions) excludes every sealed partition
// and keeps every unsealed one. Both operations run over exactly this set.
//
// spec: S14/wipe-promote-unsealed-only
func TestWipePromoteUnsealedOnly(t *testing.T) {
	parts := []pg.Partition{
		{Seq: 0, From: 1, To: 101, Sealed: true},                 // sealed history
		{Seq: 1, From: 101, To: 201, Sealed: true},               // sealed history
		{Seq: 2, From: 201, To: 301, Sealed: false},              // unsealed
		{Seq: 3, From: 301, To: 0 /* MAXVALUE */, Sealed: false}, // open tail
	}

	mutable := pg.MutablePartitions(parts)

	// Exactly the two unsealed partitions are eligible, in order.
	if len(mutable) != 2 {
		t.Fatalf("MutablePartitions returned %d partitions, want 2 (the unsealed ones): %+v", len(mutable), mutable)
	}
	for _, p := range mutable {
		if p.Sealed {
			t.Errorf("MutablePartitions included sealed partition seq=%d; sealed history is immutable by construction", p.Seq)
		}
		if !p.Mutable() {
			t.Errorf("partition seq=%d is in the mutable set but reports Mutable()=false", p.Seq)
		}
	}
	if mutable[0].Seq != 2 || mutable[1].Seq != 3 {
		t.Errorf("mutable partitions = seqs %d,%d, want 2,3 (the unsealed tail)", mutable[0].Seq, mutable[1].Seq)
	}

	// Every sealed partition reports itself immutable, so wipe and promote -- both
	// of which filter through MutablePartitions -- can never reach it.
	for _, p := range parts {
		if p.Sealed && p.Mutable() {
			t.Errorf("sealed partition seq=%d reports Mutable()=true; wipe/promote must never touch it", p.Seq)
		}
	}
}

// TestJournalPartitionByIDRange proves data_journal is partitioned by id range
// and that partition size is governed by the configurable journal_partition_rows
// threshold (default 10M), treated as a threshold rather than an exact cap.
//
// spec: S14/partition-by-id-range
func TestJournalPartitionByIDRange(t *testing.T) {
	ctx := context.Background()

	// The parent table is declared PARTITION BY RANGE (id).
	jt := pg.JournalTable()
	if jt.Partition != "id" {
		t.Errorf("data_journal partition key = %q, want id (PARTITION BY RANGE (id))", jt.Partition)
	}
	rec := pgtest.New()
	if err := pg.EnsureJournal(ctx, rec); err != nil {
		t.Fatalf("EnsureJournal: %v", err)
	}
	if create := rec.Statements()[0]; !strings.Contains(create, "PARTITION BY RANGE (id)") {
		t.Errorf("data_journal DDL is not id-range partitioned:\n%s", create)
	}

	t.Run("default threshold is the configured 10M", func(t *testing.T) {
		if config.DefaultJournalPartitionRows != 10_000_000 {
			t.Errorf("default journal_partition_rows = %d, want 10_000_000", config.DefaultJournalPartitionRows)
		}
	})

	t.Run("partition size is governed by the threshold", func(t *testing.T) {
		// Under a threshold of 100 rows and a stream of small runs, partitions
		// seal roughly every threshold rows: the first cut lands at the first
		// safe boundary at or past 100 ids.
		plan := pg.PartitionPlan{Threshold: 100}
		var runs []pg.RunWindow
		for i := int64(1); i <= 250; i++ {
			runs = append(runs, pg.RunWindow{RunID: i, Floor: i, Ceiling: i})
		}
		bs := plan.Boundaries(runs)
		if want := []int64{101, 201}; !equalInts(bs, want) {
			t.Fatalf("threshold=100 boundaries over 250 single-id runs = %v, want %v (~one cut per threshold)", bs, want)
		}
	})

	t.Run("threshold is not an exact cap", func(t *testing.T) {
		// A single run larger than the threshold never splits: the partition holds
		// more than the threshold's worth of rows (a long dev loop delays its seal).
		plan := pg.PartitionPlan{Threshold: 100}
		big := pg.RunWindow{RunID: 1, Floor: 1, Ceiling: 350}
		bs := plan.Boundaries([]pg.RunWindow{big})
		if len(bs) != 0 {
			t.Fatalf("boundaries over one 350-row run under threshold 100 = %v, want none (threshold is not a cap)", bs)
		}
		if windowSplit(bs, big) {
			t.Error("a run larger than the threshold was split; the threshold must not be an exact cap")
		}
	})

	t.Run("partition DDL is an id-range PARTITION OF the journal", func(t *testing.T) {
		// The bootstrap tail partition spans the whole id space (MINVALUE..MAXVALUE),
		// so the journal is writable; a sealed partition carries a concrete
		// threshold-sized id range. Both are golden.
		got := []byte(strings.Join([]string{
			pg.InitialPartition().CreateDDL(),
			pg.Partition{Seq: 1, From: 1, To: config.DefaultJournalPartitionRows + 1, Sealed: true}.CreateDDL(),
		}, "\n\n") + "\n")
		golden.Assert(t, got, filepath.Join("testdata", "data_journal_partition.sql"))
	})
}

// equalInts reports whether a and b hold the same int64s in the same order.
func equalInts(a, b []int64) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
