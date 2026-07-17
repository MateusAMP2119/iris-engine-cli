package dispatch

import (
	"context"
	"errors"
	"sort"
	"sync"
)

// This file is the lane model and walk: the pure construction that turns the
// persisted lanes roster plus the set of registered pipelines into the per-lane
// runnable walk, and the lane runner that walks one lane serially while distinct
// lanes run in parallel.
//
// Composer order is pure sequencing. A lane's walk carries an order and nothing
// else: no data link between members, no failure propagation, no eligibility
// gate. Those are the depends_on gate's job (a separate mechanism) and the
// dispatcher's, not the lane's. The lane runner here starts each member in turn
// and never gates the walk on a member's outcome.

// LaneRow is one persisted lanes-table row: a (lane, pipeline) placement at a walk
// position. It mirrors the lanes read seam -- lanes holds pipeline names, never
// foreign keys, so a row may name a folder that is not (or no longer) a registered
// pipeline; the walk skips such names.
type LaneRow struct {
	// Lane is the lane the row places its pipeline in.
	Lane string
	// Pipeline is the placed pipeline's name (a name, never an FK).
	Pipeline string
	// Pos is the row's position in its lane's walk (ORDER BY pos).
	Pos int64
}

// QueueLane names the shared serial queue lane for unplaced pipelines; no folder can be empty-named, so it never collides with a declared lane.
const QueueLane = ""

// Lane is one lane's runnable walk: its name and the registered pipelines to
// start within it, in composer (pos) order. A named lane comes from the lanes
// roster; the QueueLane holds every pipeline no lane places.
type Lane struct {
	// Name is the lane's name (its own name, or the pipeline's for an anonymous lane).
	Name string
	// Pipelines are the registered members to start, in walk order.
	Pipelines []string
}

// BuildWalk constructs the per-lane runnable walk from the persisted lanes rows
// and the set of registered pipelines. It is pure: no I/O, and its result is a
// function of its inputs alone.
//
// Within each lane the members are ordered by pos, and any name with no registered
// pipeline is skipped; a lane left with no registered member contributes nothing and
// is omitted. Pipelines no lane row names share the one QueueLane, walked serially
// in name order: only a declared lane buys a walk goroutine of its own.
//
// registered maps each registered pipeline name to true. The returned lanes are
// sorted by name for a stable result; that order is not an execution order --
// lanes run in parallel with no cross-lane sequencing.
func BuildWalk(rows []LaneRow, registered map[string]bool) []Lane {
	// Group rows by lane, preserving each row so the lane can be ordered by pos.
	byLane := map[string][]LaneRow{}
	laneOrder := []string{}
	placed := map[string]bool{} // pipelines named by some lane row (registered or not)
	for _, r := range rows {
		if _, seen := byLane[r.Lane]; !seen {
			laneOrder = append(laneOrder, r.Lane)
		}
		byLane[r.Lane] = append(byLane[r.Lane], r)
		placed[r.Pipeline] = true
	}

	var lanes []Lane
	for _, name := range laneOrder {
		members := byLane[name]
		// ORDER BY pos, stable so equal-pos rows (excluded by UNIQUE(lane, pos)
		// in practice) keep input order.
		sort.SliceStable(members, func(i, j int) bool { return members[i].Pos < members[j].Pos })
		var pipelines []string
		for _, m := range members {
			if registered[m.Pipeline] { // skip names with no registered pipeline
				pipelines = append(pipelines, m.Pipeline)
			}
		}
		if len(pipelines) == 0 {
			continue // a lane of only unregistered names contributes no walk
		}
		lanes = append(lanes, Lane{Name: name, Pipelines: pipelines})
	}

	// Registered pipelines no lane row names share one serial queue lane.
	var loose []string
	for pipeline := range registered {
		if registered[pipeline] && !placed[pipeline] {
			loose = append(loose, pipeline)
		}
	}
	if len(loose) > 0 {
		sort.Strings(loose)
		lanes = append(lanes, Lane{Name: QueueLane, Pipelines: loose})
	}

	sort.Slice(lanes, func(i, j int) bool { return lanes[i].Name < lanes[j].Name })
	return lanes
}

// RunOutcome is the terminal disposition of a single run, as reported to the lane
// runner. The runner never gates its walk on it: composer order sequences members, it
// does not propagate failure. Recording the outcome and any depends_on propagation
// belong to the dispatcher in later epics.
type RunOutcome int

// The terminal run dispositions the lane runner distinguishes.
const (
	// RunSucceeded is a run that reached a successful terminal state.
	RunSucceeded RunOutcome = iota
	// RunDeadLettered is a run that failed to a dead-lettered terminal state.
	// The lane still proceeds to its next member: order never gates.
	RunDeadLettered
)

// RunStarter starts one pipeline run and blocks until it reaches a terminal
// state, returning that terminal disposition. It is the lane runner's seam onto
// run execution: the runner owns sequencing, RunStarter owns the run.
//
// A returned error means the run could not be carried out at all -- for example ctx
// was cancelled -- and stops the lane's walk. A run that executes and then
// dead-letters is not an error: it returns (RunDeadLettered, nil), and the lane
// proceeds to its next member, because composer order never gates.
type RunStarter interface {
	StartRun(ctx context.Context, pipeline string) (RunOutcome, error)
}

// LaneRunner walks one lane's members in composer order, serially. It is the
// one-goroutine-per-lane unit: RunPass performs a single ordered pass; the
// perpetual repetition and the idle watermark layer on top in the lane loop
// (pass.go's Loop over events.go's Events).
type LaneRunner struct {
	lane    Lane
	starter RunStarter
}

// NewLaneRunner builds a runner for one lane over the given run-start seam.
func NewLaneRunner(lane Lane, starter RunStarter) *LaneRunner {
	return &LaneRunner{lane: lane, starter: starter}
}

// RunPass walks the lane's members once, in composer order, serially: it starts
// member N+1 only after member N reaches a terminal state. It never gates on a
// member's outcome -- a dead-lettered member does not stop the walk -- so a run
// that merely fails is not an error here. RunPass returns a non-nil error only
// when a run could not be carried out (ctx cancellation), stopping the pass so
// the runner is reusable rather than left mid-lane.
func (r *LaneRunner) RunPass(ctx context.Context) error {
	for _, pipeline := range r.lane.Pipelines {
		if err := ctx.Err(); err != nil {
			return err
		}
		// The outcome is deliberately discarded: composer order sequences, it
		// does not gate. Only an operational error (a run that could not run)
		// stops the walk.
		if _, err := r.starter.StartRun(ctx, pipeline); err != nil {
			return err
		}
	}
	return nil
}

// RunLanes runs every lane's pass concurrently: one goroutine per lane, all launched
// before any is awaited, so lanes run in parallel with no engine cap and no
// cross-lane serialization. It returns when every lane's pass has finished, joining
// any per-lane errors so one lane's failure to run neither hides another's nor leaks
// a goroutine.
func RunLanes(ctx context.Context, lanes []Lane, starter RunStarter) error {
	errs := make([]error, len(lanes))
	var wg sync.WaitGroup
	for i := range lanes {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			errs[i] = NewLaneRunner(lanes[i], starter).RunPass(ctx)
		}(i)
	}
	wg.Wait()
	return errors.Join(errs...)
}
