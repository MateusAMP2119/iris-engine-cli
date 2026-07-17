package dispatch_test

import (
	"context"
	"reflect"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/MateusAMP2119/iris-lakehouse/internal/dispatch"
	"github.com/MateusAMP2119/iris-lakehouse/internal/store"
)

// TestLanesTableShape pins the persisted lanes-table shape the walk reads from: rows
// keyed by (lane, pipeline), UNIQUE(pipeline) and UNIQUE(lane, pos), and pipeline
// referenced by name with no foreign key so an order may name unregistered folders.
func TestLanesTableShape(t *testing.T) {
	t.Run("lanes-table-shape", func(t *testing.T) {
		schema := store.MetaSchema()
		var lanes *store.Table
		for i := range schema.Tables {
			if schema.Tables[i].Name == "lanes" {
				lanes = &schema.Tables[i]
			}
		}
		if lanes == nil {
			t.Fatal("meta schema has no lanes table")
		}

		// Keyed by (lane, pipeline).
		if want := []string{"lane", "pipeline"}; !reflect.DeepEqual(lanes.PrimaryKey, want) {
			t.Errorf("lanes primary key = %v, want %v", lanes.PrimaryKey, want)
		}

		// UNIQUE(pipeline) and UNIQUE(lane, pos), in either declaration order.
		gotUniques := map[string]bool{}
		for _, u := range lanes.Uniques {
			gotUniques[strings.Join(u.Columns, ",")] = true
		}
		for _, want := range []string{"pipeline", "lane,pos"} {
			if !gotUniques[want] {
				t.Errorf("lanes missing UNIQUE(%s); have %v", want, gotUniques)
			}
		}

		// pipeline is a name, never a foreign key: an order may name an
		// unregistered folder, so lanes carries no FK edge at all.
		if len(lanes.ForeignKeys) != 0 {
			t.Errorf("lanes must have no foreign keys (pipeline is a name); got %v", lanes.ForeignKeys)
		}

		// The columns the walk keys and orders on.
		gotCols := map[string]bool{}
		for _, c := range lanes.Columns {
			gotCols[c.Name] = true
		}
		for _, want := range []string{"lane", "pipeline", "pos"} {
			if !gotCols[want] {
				t.Errorf("lanes missing column %q; have %v", want, gotCols)
			}
		}
	})
}

// TestBuildWalkSkipsUnregistered proves the walk of a lane orders its rows by pos and
// drops any name with no registered pipeline.
func TestBuildWalkSkipsUnregistered(t *testing.T) {
	t.Run("lane-walk-skips-unregistered", func(t *testing.T) {
		// Rows deliberately out of pos order to prove ORDER BY pos, with an
		// unregistered "ghost" between two registered members.
		rows := []dispatch.LaneRow{
			{Lane: "etl", Pipeline: "load", Pos: 2},
			{Lane: "etl", Pipeline: "extract", Pos: 0},
			{Lane: "etl", Pipeline: "ghost", Pos: 1},
		}
		registered := map[string]bool{"extract": true, "load": true}

		got := dispatch.BuildWalk(rows, registered)
		want := []dispatch.Lane{{Name: "etl", Pipelines: []string{"extract", "load"}}}
		if !reflect.DeepEqual(got, want) {
			t.Fatalf("BuildWalk = %v, want %v (ghost skipped, ordered by pos)", got, want)
		}
	})
}

// TestBuildWalkRunnerSkipsUnregisteredNames proves lane-walk construction drops
// lane-order names that have no registered pipeline, so a lane whose members are all
// unregistered contributes nothing to run.
func TestBuildWalkRunnerSkipsUnregisteredNames(t *testing.T) {
	t.Run("runner-skips-unregistered-names", func(t *testing.T) {
		rows := []dispatch.LaneRow{
			{Lane: "dead", Pipeline: "ghostA", Pos: 0},
			{Lane: "dead", Pipeline: "ghostB", Pos: 1},
		}
		// No registered pipelines: every ordered name is unregistered.
		got := dispatch.BuildWalk(rows, map[string]bool{})
		if len(got) != 0 {
			t.Fatalf("a lane of only unregistered names yields no walk; got %v", got)
		}
	})
}

// TestBuildWalkAbsentPipelineQueueLane proves a registered pipeline named in no
// lane is scheduled in the shared queue lane, not a lane of its own.
func TestBuildWalkAbsentPipelineQueueLane(t *testing.T) {
	t.Run("absent-pipeline-queue-lane", func(t *testing.T) {
		rows := []dispatch.LaneRow{
			{Lane: "etl", Pipeline: "a", Pos: 0},
			{Lane: "etl", Pipeline: "b", Pos: 1},
		}
		// solo is registered but appears in no lane row.
		registered := map[string]bool{"a": true, "b": true, "solo": true}

		got := dispatch.BuildWalk(rows, registered)
		want := []dispatch.Lane{
			{Name: dispatch.QueueLane, Pipelines: []string{"solo"}},
			{Name: "etl", Pipelines: []string{"a", "b"}},
		}
		if !reflect.DeepEqual(got, want) {
			t.Fatalf("BuildWalk = %v, want %v (solo in the shared queue lane)", got, want)
		}
	})
}

// TestBuildWalkLoosePipelinesShareQueueLane proves composer-unordered pipelines all
// land in the one shared queue lane, walked serially in name order, so a declared
// lane is the only thing that buys a walk goroutine of its own.
func TestBuildWalkLoosePipelinesShareQueueLane(t *testing.T) {
	t.Run("loose-pipelines-share-queue-lane", func(t *testing.T) {
		// Three registered pipelines, none placed by a composer (no lane rows).
		registered := map[string]bool{"gamma": true, "alpha": true, "beta": true}

		got := dispatch.BuildWalk(nil, registered)
		want := []dispatch.Lane{
			{Name: dispatch.QueueLane, Pipelines: []string{"alpha", "beta", "gamma"}},
		}
		if !reflect.DeepEqual(got, want) {
			t.Fatalf("BuildWalk = %v, want %v (all loose pipelines in one queue lane, name order)", got, want)
		}
	})
}

// TestLaneRunnerSerialWithinLane proves composer order sequences members within
// a lane serially -- member N+1 starts only after member N reaches a terminal
// state -- with no data threaded between them (the RunStarter seam carries only
// a pipeline name, no data link).
func TestLaneRunnerSerialWithinLane(t *testing.T) {
	t.Run("composer-order-no-data-link", func(t *testing.T) {
		f := &serialStarter{
			entered: make(chan string, 8),
			release: map[string]chan struct{}{
				"a": make(chan struct{}),
				"b": make(chan struct{}),
			},
		}
		close(f.release["b"]) // b, once started, returns at once

		lane := dispatch.Lane{Name: "etl", Pipelines: []string{"a", "b"}}
		done := make(chan error, 1)
		go func() { done <- dispatch.NewLaneRunner(lane, f).RunPass(context.Background()) }()

		// a is now inside StartRun, blocked until released.
		if got := <-f.entered; got != "a" {
			t.Fatalf("first started = %q, want a", got)
		}
		// While a is in flight, b must not have started: serial within the lane.
		f.mu.Lock()
		started := append([]string(nil), f.started...)
		f.mu.Unlock()
		if !reflect.DeepEqual(started, []string{"a"}) {
			t.Fatalf("while a runs, started = %v, want [a] (b must wait for a to finish)", started)
		}

		// Let a finish; the lane proceeds to b.
		close(f.release["a"])
		if got := <-f.entered; got != "b" {
			t.Fatalf("second started = %q, want b", got)
		}
		if err := <-done; err != nil {
			t.Fatalf("RunPass returned %v, want nil", err)
		}
		if want := []string{"a", "b"}; !reflect.DeepEqual(f.started, want) {
			t.Fatalf("start order = %v, want %v", f.started, want)
		}
	})
}

// TestLaneRunnerOrderNeverGates proves composer order affects ordering only: a
// composer-ordered pipeline with no depends_on edge still runs when an earlier lane
// member's run dead-letters.
func TestLaneRunnerOrderNeverGates(t *testing.T) {
	t.Run("order-never-gates", func(t *testing.T) {
		f := &outcomeStarter{outcomes: map[string]dispatch.RunOutcome{
			"a": dispatch.RunDeadLettered,
			"b": dispatch.RunSucceeded,
		}}
		lane := dispatch.Lane{Name: "etl", Pipelines: []string{"a", "b"}}

		if err := dispatch.NewLaneRunner(lane, f).RunPass(context.Background()); err != nil {
			t.Fatalf("RunPass returned %v, want nil", err)
		}
		if want := []string{"a", "b"}; !reflect.DeepEqual(f.started, want) {
			t.Fatalf("started = %v, want %v (b runs even though a dead-lettered)", f.started, want)
		}
	})
}

// TestRunLanesParallelNoCap proves distinct lanes run concurrently with no engine cap
// and no cross-lane serialization: N lanes must all be in flight at once, which
// composer-unordered pipelines require.
func TestRunLanesParallelNoCap(t *testing.T) {
	t.Run("no-declaration-order-tiebreak", func(t *testing.T) {
		const n = 5
		var lanes []dispatch.Lane
		for i := 0; i < n; i++ {
			name := "p" + strconv.Itoa(i)
			lanes = append(lanes, dispatch.Lane{Name: name, Pipelines: []string{name}})
		}
		// The barrier only releases once all n runs are simultaneously in
		// flight; if the runner serialized lanes it would deadlock, and the
		// context timeout would surface it as an error rather than a hang.
		b := &barrierStarter{n: n, allHere: make(chan struct{})}
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()

		if err := dispatch.RunLanes(ctx, lanes, b); err != nil {
			t.Fatalf("all %d lanes must run concurrently (no cap); got %v", n, err)
		}
		if got := b.count(); got != n {
			t.Fatalf("started %d lanes, want %d", got, n)
		}
	})
}

// serialStarter is a RunStarter that records start order and blocks each run
// until the test releases it, so serialization within a lane is observable with
// no fixed sleep.
type serialStarter struct {
	mu      sync.Mutex
	started []string
	entered chan string
	release map[string]chan struct{}
}

func (s *serialStarter) StartRun(ctx context.Context, pipeline string) (dispatch.RunOutcome, error) {
	s.mu.Lock()
	s.started = append(s.started, pipeline)
	rel := s.release[pipeline]
	s.mu.Unlock()
	s.entered <- pipeline
	select {
	case <-rel:
	case <-ctx.Done():
		return dispatch.RunSucceeded, ctx.Err()
	}
	return dispatch.RunSucceeded, nil
}

// outcomeStarter is a RunStarter that records start order and returns a
// per-pipeline outcome, so "a failed run does not gate the next member" is
// observable. It is used single-lane, so it needs no synchronization.
type outcomeStarter struct {
	started  []string
	outcomes map[string]dispatch.RunOutcome
}

func (o *outcomeStarter) StartRun(_ context.Context, pipeline string) (dispatch.RunOutcome, error) {
	o.started = append(o.started, pipeline)
	return o.outcomes[pipeline], nil
}

// barrierStarter is a RunStarter that blocks every run until n of them are
// simultaneously in flight, so cross-lane concurrency (and the absence of an
// engine cap) is provable without a fixed sleep: only a runner that starts one
// goroutine per lane can gather all n at the barrier.
type barrierStarter struct {
	n       int
	mu      sync.Mutex
	arrived int
	allHere chan struct{}
}

func (b *barrierStarter) StartRun(ctx context.Context, _ string) (dispatch.RunOutcome, error) {
	b.mu.Lock()
	b.arrived++
	if b.arrived == b.n {
		close(b.allHere)
	}
	b.mu.Unlock()
	select {
	case <-b.allHere:
		return dispatch.RunSucceeded, nil
	case <-ctx.Done():
		return dispatch.RunSucceeded, ctx.Err()
	}
}

func (b *barrierStarter) count() int {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.arrived
}
