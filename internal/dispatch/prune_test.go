package dispatch_test

import (
	"reflect"
	"testing"

	"github.com/MateusAMP2119/iris-engine-cli/internal/config"
	"github.com/MateusAMP2119/iris-engine-cli/internal/dispatch"
)

// runs is a small builder: it turns a pipeline name and a set of run ids into
// RetentionRun rows, so a table case reads as "this pipeline has these run ids".
func runs(pipeline string, ids ...int64) []dispatch.RetentionRun {
	out := make([]dispatch.RetentionRun, 0, len(ids))
	for _, id := range ids {
		out = append(out, dispatch.RetentionRun{RunID: id, Pipeline: pipeline})
	}
	return out
}

// TestSelectPrunableCountBased proves retention is count-based and clockless:
// SelectPrunable keeps the newest `retain` runs per pipeline -- ordered by run id,
// meta's monotonic identity, never a clock -- and prunes the rest, independently per
// pipeline. The decision is a function of run ids and the retain count alone:
// SelectPrunable takes no timestamp and no consumer watermark, so a run is prunable
// purely by count, never pinned by whether a downstream consumed it (consumption and
// retention are unlinked).
func TestSelectPrunableCountBased(t *testing.T) {
	tests := []struct {
		name   string
		runs   []dispatch.RetentionRun
		retain int
		want   []int64
	}{
		{
			name:   "within retain prunes nothing",
			runs:   runs("load", 1, 2, 3),
			retain: 3,
			want:   nil,
		},
		{
			name:   "fewer than retain prunes nothing",
			runs:   runs("load", 1, 2),
			retain: 10,
			want:   nil,
		},
		{
			name:   "keeps newest retain, prunes the older tail",
			runs:   runs("load", 1, 2, 3, 4, 5),
			retain: 2,
			want:   []int64{1, 2, 3}, // newest two (4,5) kept; 1,2,3 pruned.
		},
		{
			name:   "retain one keeps only the newest",
			runs:   runs("load", 10, 20, 30),
			retain: 1,
			want:   []int64{10, 20}, // only 30 (newest) survives.
		},
		{
			name:   "retain zero prunes every run",
			runs:   runs("load", 7, 8, 9),
			retain: 0,
			want:   []int64{7, 8, 9},
		},
		{
			name:   "negative retain treated as zero, prunes all",
			runs:   runs("load", 4, 5),
			retain: -3,
			want:   []int64{4, 5},
		},
		{
			// Ordering is by run id, not the order rows are supplied: the same set
			// shuffled yields the same decision -- the proof retention is clockless
			// and identity-ordered, never dependent on arrival order.
			name:   "clockless: order-independent, ordered by id",
			runs:   runs("load", 30, 10, 50, 20, 40),
			retain: 2,
			want:   []int64{10, 20, 30}, // newest two by id (40,50) kept.
		},
		{
			// Retention is per pipeline: each pipeline keeps its own newest `retain`,
			// so a pipeline within retain is untouched while another is trimmed.
			name: "per pipeline independent counts",
			runs: append(append([]dispatch.RetentionRun{},
				runs("extract", 100, 101, 102, 103)...),
				runs("load", 200, 201)...),
			retain: 2,
			want:   []int64{100, 101}, // extract trims 100,101; load (2 runs) untouched.
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := dispatch.SelectPrunable(tt.runs, tt.retain, nil)
			if !reflect.DeepEqual(got, tt.want) {
				t.Fatalf("SelectPrunable(retain=%d) = %v, want %v", tt.retain, got, tt.want)
			}
		})
	}
}

// TestSelectPrunableDefaultRetain proves the documented default retain of 1000: with
// the config-resolved default, a pipeline keeps its newest 1000 runs and prunes only
// what lies beyond, so a pipeline of 1001 runs prunes exactly its oldest one.
func TestSelectPrunableDefaultRetain(t *testing.T) {
	if config.DefaultRetain != 1000 {
		t.Fatalf("config.DefaultRetain = %d, want the documented default 1000", config.DefaultRetain)
	}
	ids := make([]int64, 0, 1001)
	for i := int64(1); i <= 1001; i++ {
		ids = append(ids, i)
	}
	got := dispatch.SelectPrunable(runs("load", ids...), int(config.DefaultRetain), nil)
	if want := []int64{1}; !reflect.DeepEqual(got, want) {
		t.Fatalf("SelectPrunable(default retain 1000) over 1001 runs = %v, want %v (oldest one pruned)", got, want)
	}
}

// TestSelectPrunableSparesOutstandingDeadLetter proves the pruner never removes a
// dead-lettered run while an outstanding dead_letters entry still holds it: post-pass
// pruning spares such a run until replay, supersession, or drain releases it. A run
// beyond retain but named in the outstanding worklist is excluded from the prune set;
// once the entry is released (gone from the outstanding set), the same run becomes
// prunable.
func TestSelectPrunableSparesOutstandingDeadLetter(t *testing.T) {
	// Five runs, retain the newest two: ids 1, 2, 3 are beyond retain.
	pipeline := runs("load", 1, 2, 3, 4, 5)

	// Run 2 is dead-lettered with an outstanding entry: it is spared even though it
	// is beyond retain. Runs 1 and 3 (no entry) are pruned.
	got := dispatch.SelectPrunable(pipeline, 2, []int64{2})
	if want := []int64{1, 3}; !reflect.DeepEqual(got, want) {
		t.Fatalf("with run 2 held by a dead_letters entry, prune set = %v, want %v (2 spared)", got, want)
	}

	// A held run within retain is spared regardless (it would not be pruned anyway),
	// and multiple outstanding entries are all spared.
	got = dispatch.SelectPrunable(pipeline, 2, []int64{1, 2, 3})
	if got != nil {
		t.Fatalf("with runs 1,2,3 all held, prune set = %v, want none (all spared)", got)
	}

	// Once the entry is released (run 2 no longer outstanding), run 2 rejoins the
	// prune set: the spare lasts only as long as the worklist entry.
	got = dispatch.SelectPrunable(pipeline, 2, nil)
	if want := []int64{1, 2, 3}; !reflect.DeepEqual(got, want) {
		t.Fatalf("after release, prune set = %v, want %v (2 now prunable)", got, want)
	}
}
