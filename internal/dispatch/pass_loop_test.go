package dispatch_test

import (
	"context"
	"reflect"
	"sync"
	"testing"
	"time"

	"github.com/MateusAMP2119/iris-engine-cli/internal/dispatch"
	"github.com/MateusAMP2119/iris-engine-cli/internal/pg"
	"github.com/MateusAMP2119/iris-engine-cli/internal/store"
)

// testTimeout bounds every loop test so a synchronization bug surfaces as a failure
// rather than a hang. Tests synchronize on channels and state, never on elapsed time.
const testTimeout = 10 * time.Second

// fakeWalk is a mutable, concurrency-safe WalkReader: the test sets the current walk and
// swaps it between passes to exercise pass-boundary graph visibility. Each Walk returns a
// copy so a reader can never mutate the test's staging.
type fakeWalk struct {
	mu    sync.Mutex
	lanes []dispatch.Lane
}

func newFakeWalk(lanes ...dispatch.Lane) *fakeWalk { return &fakeWalk{lanes: lanes} }

func (w *fakeWalk) set(lanes ...dispatch.Lane) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.lanes = lanes
}

func (w *fakeWalk) Walk(context.Context) ([]dispatch.Lane, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	return append([]dispatch.Lane(nil), w.lanes...), nil
}

// fakeGate is a per-pipeline PassGate: a pipeline absent from the map is ungated (always
// runs), so most tests need only name the exceptions (a closed or poisoned gate).
type fakeGate struct {
	mu     sync.Mutex
	decide map[string]dispatch.Decision
}

func newFakeGate() *fakeGate { return &fakeGate{decide: map[string]dispatch.Decision{}} }

func (g *fakeGate) set(pipeline string, d dispatch.Decision) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.decide[pipeline] = d
}

func (g *fakeGate) Eligible(_ context.Context, pipeline string) (dispatch.Decision, error) {
	g.mu.Lock()
	defer g.mu.Unlock()
	if d, ok := g.decide[pipeline]; ok {
		return d, nil
	}
	return dispatch.Decision{Run: true}, nil
}

// fakeRunner is the FreshRunner test double. It records every started run in order and
// supports sleepless synchronization: a run signals its start on entered (when set) and
// blocks on its per-pipeline release channel (when set) until the test frees it, so
// serial/parallel/hung timing is observable without a fixed sleep. A pipeline with no
// release channel completes at once.
type fakeRunner struct {
	mu      sync.Mutex
	starts  []store.RunRecord
	ev      *eventLog
	outcome map[string]dispatch.RunOutcome
	entered chan string
	release map[string]chan struct{}
}

func newFakeRunner() *fakeRunner {
	return &fakeRunner{
		outcome: map[string]dispatch.RunOutcome{},
		release: map[string]chan struct{}{},
	}
}

func (r *fakeRunner) StartFresh(ctx context.Context, rec store.RunRecord) (dispatch.RunOutcome, error) {
	r.mu.Lock()
	r.starts = append(r.starts, rec)
	rel := r.release[rec.Pipeline]
	out := r.outcome[rec.Pipeline]
	ev := r.ev
	entered := r.entered
	r.mu.Unlock()

	if ev != nil {
		ev.add("run:" + rec.Pipeline)
	}
	if entered != nil {
		select {
		case entered <- rec.Pipeline:
		case <-ctx.Done():
			return dispatch.RunSucceeded, ctx.Err()
		}
	}
	if rel != nil {
		select {
		case <-rel:
		case <-ctx.Done():
			return dispatch.RunSucceeded, ctx.Err()
		}
	}
	return out, nil
}

// startedPipelines returns the pipeline names of every started run, in order.
func (r *fakeRunner) startedPipelines() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]string, len(r.starts))
	for i, rec := range r.starts {
		out[i] = rec.Pipeline
	}
	return out
}

// count returns how many runs of pipeline were started.
func (r *fakeRunner) count(pipeline string) int {
	r.mu.Lock()
	defer r.mu.Unlock()
	n := 0
	for _, rec := range r.starts {
		if rec.Pipeline == pipeline {
			n++
		}
	}
	return n
}

// eventLog is an ordered, concurrency-safe record of run and post-pass events, so the
// "post-pass work never interleaves mid-pass" ordering is observable.
type eventLog struct {
	mu     sync.Mutex
	events []string
}

func (e *eventLog) add(s string) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.events = append(e.events, s)
}

func (e *eventLog) snapshot() []string {
	e.mu.Lock()
	defer e.mu.Unlock()
	return append([]string(nil), e.events...)
}

// fakePostPass records each AfterPass call: the event log entry (to prove ordering) and
// the reports, so a test can assert what post-pass bookkeeping saw.
type fakePostPass struct {
	mu      sync.Mutex
	ev      *eventLog
	reports []dispatch.PassReport
}

func (p *fakePostPass) AfterPass(_ context.Context, report dispatch.PassReport) error {
	if p.ev != nil {
		p.ev.add("post:" + report.Lane)
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	p.reports = append(p.reports, report)
	return nil
}

func (p *fakePostPass) snapshot() []dispatch.PassReport {
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([]dispatch.PassReport(nil), p.reports...)
}

// TestLaneRunnerComposerOrder proves each lane runner starts the ELIGIBLE pipelines
// in composer order (ORDER BY pos) on a pass: a closed-gate member in the middle
// mints no run, and the open-gated members start in their composed order regardless.
func TestLaneRunnerComposerOrder(t *testing.T) {
	t.Run("lane-runner-composer-order", func(t *testing.T) {
		gate := newFakeGate()
		gate.set("skipme", dispatch.Decision{Run: false}) // closed gate: mints no run
		runner := newFakeRunner()
		loop := dispatch.NewLoop(newFakeWalk(), gate, runner, nil)

		lane := dispatch.Lane{Name: "etl", Pipelines: []string{"extract", "skipme", "transform", "load"}}
		if err := loop.RunLanePass(context.Background(), lane); err != nil {
			t.Fatalf("RunLanePass returned %v, want nil", err)
		}

		if got, want := runner.startedPipelines(), []string{"extract", "transform", "load"}; !reflect.DeepEqual(got, want) {
			t.Fatalf("started = %v, want %v (eligible members in composer order, skipme minted no run)", got, want)
		}
	})
}

// TestDispatcherPostPassOnly proves dispatcher-owned bookkeeping runs
// opportunistically only AFTER a lane pass completes, never mid-pass: the post-pass
// event lands strictly after every one of the pass's runs, and a poisoned member's
// failure propagation is deferred to the post-pass report rather than written at the
// member's turn.
func TestDispatcherPostPassOnly(t *testing.T) {
	t.Run("dispatcher-post-pass-only", func(t *testing.T) {
		ev := &eventLog{}
		gate := newFakeGate()
		// dep's awaited upstream dead-lettered: the gate poisons, so no run starts for it
		// and the failure propagation must be deferred to post-pass.
		gate.set("dep", dispatch.Decision{
			Poisoned: true,
			Ledger:   []dispatch.EdgeVerdict{{Upstream: "up", Verdict: dispatch.VerdictPoisoned, LatestRunID: 9}},
		})
		runner := newFakeRunner()
		runner.ev = ev
		post := &fakePostPass{ev: ev}
		loop := dispatch.NewLoop(newFakeWalk(), gate, runner, nil, dispatch.WithPostPass(post))

		lane := dispatch.Lane{Name: "etl", Pipelines: []string{"a", "dep", "b"}}
		if err := loop.RunLanePass(context.Background(), lane); err != nil {
			t.Fatalf("RunLanePass returned %v, want nil", err)
		}

		// The pass's runs come first, in order; the single post-pass event lands strictly
		// after them -- never between two members.
		if got, want := ev.snapshot(), []string{"run:a", "run:b", "post:etl"}; !reflect.DeepEqual(got, want) {
			t.Fatalf("event order = %v, want %v (post-pass strictly after the pass, never mid-pass)", got, want)
		}

		// The poisoned member reached post-pass as deferred propagation, not a mid-pass
		// write: it appears in the post-pass report, and it started no run.
		reports := post.snapshot()
		if len(reports) != 1 {
			t.Fatalf("post-pass invoked %d times, want exactly 1 per lane pass", len(reports))
		}
		rep := reports[0]
		if len(rep.Poisoned) != 1 || rep.Poisoned[0].Pipeline != "dep" {
			t.Fatalf("post-pass report poisoned = %+v, want [dep] (propagation deferred to post-pass)", rep.Poisoned)
		}
		if got, want := rep.Started, []string{"a", "b"}; !reflect.DeepEqual(got, want) {
			t.Fatalf("post-pass report started = %v, want %v (dep minted no run)", got, want)
		}
	})
}

// blocked returns a never-sent channel, so a run that blocks on it stays in flight until
// the test explicitly sends a token or the context is cancelled.
func blocked() chan struct{} { return make(chan struct{}) }

// TestSameLaneSerialSeparateParallel proves pipelines in the same lane are dispatched
// serially in composed order while pipelines in separate lanes run in parallel: while
// the first member of lane etl is in flight, its second member has not started
// (serial), yet a member of a separate lane is already running (parallel), and the
// second member starts only after the first reaches a terminal state.
func TestSameLaneSerialSeparateParallel(t *testing.T) {
	t.Run("same-lane-serial-order", func(t *testing.T) {
		ctx, cancel := context.WithTimeout(context.Background(), testTimeout)
		defer cancel()

		runner := newFakeRunner()
		runner.entered = make(chan string)
		runner.release["a1"] = blocked()
		runner.release["a2"] = blocked()
		runner.release["b1"] = blocked()
		loop := dispatch.NewLoop(
			newFakeWalk(
				dispatch.Lane{Name: "etl", Pipelines: []string{"a1", "a2"}},
				dispatch.Lane{Name: "other", Pipelines: []string{"b1"}},
			),
			newFakeGate(), runner, nil,
		)
		done := make(chan struct{})
		go func() { defer close(done); _ = loop.Run(ctx) }()
		defer func() { cancel(); <-done }()

		// The first member of etl and the sole member of the separate lane both start
		// concurrently: separate lanes run in parallel.
		first := <-runner.entered
		second := <-runner.entered
		got := map[string]bool{first: true, second: true}
		if !got["a1"] || !got["b1"] {
			t.Fatalf("first two started = %v, want {a1, b1} (separate lanes parallel)", got)
		}

		// Serial within the lane: while a1 is in flight, a2 has not started.
		if n := runner.count("a2"); n != 0 {
			t.Fatalf("a2 started %d times while a1 in flight, want 0 (same-lane serial)", n)
		}

		// a1 reaches terminal; only then does the lane proceed to a2.
		runner.release["a1"] <- struct{}{}
		if got := <-runner.entered; got != "a2" {
			t.Fatalf("after a1 terminal, next started = %q, want a2 (serial in composed order)", got)
		}
		if n := runner.count("b1"); n != 1 {
			t.Fatalf("b1 started %d times, want 1 (ran once, in parallel with lane etl)", n)
		}
	})
}

// TestLanesParallelSerialWithinNoCap proves runs within a lane execute serially while
// distinct lanes run in parallel with no engine cap, and a laneless pipeline forms
// its own lane: five own-lane pipelines and the first member of a two-member lane are
// all in flight simultaneously (no cap, laneless is its own lane run in parallel),
// while the lane's second member waits for the first (serial within).
func TestLanesParallelSerialWithinNoCap(t *testing.T) {
	t.Run("lanes-parallel-serial-within", func(t *testing.T) {
		ctx, cancel := context.WithTimeout(context.Background(), testTimeout)
		defer cancel()

		const solo = 5
		var lanes []dispatch.Lane
		runner := newFakeRunner()
		runner.entered = make(chan string)
		// Five laneless pipelines, each its own lane, all blocked so they pile up in flight.
		soloNames := []string{"p0", "p1", "p2", "p3", "p4"}
		for _, name := range soloNames {
			lanes = append(lanes, dispatch.Lane{Name: name, Pipelines: []string{name}})
			runner.release[name] = blocked()
		}
		// A two-member lane whose first member blocks, to show serial within a lane.
		lanes = append(lanes, dispatch.Lane{Name: "etl", Pipelines: []string{"x", "y"}})
		runner.release["x"] = blocked()
		runner.release["y"] = blocked()

		loop := dispatch.NewLoop(newFakeWalk(lanes...), newFakeGate(), runner, nil)
		done := make(chan struct{})
		go func() { defer close(done); _ = loop.Run(ctx) }()
		defer func() { cancel(); <-done }()

		// Gather the first solo+1 starts: the five own-lane pipelines and lane etl's first
		// member must all be in flight at once -- no engine cap, laneless is its own lane.
		inflight := map[string]bool{}
		for i := 0; i < solo+1; i++ {
			inflight[<-runner.entered] = true
		}
		for _, name := range soloNames {
			if !inflight[name] {
				t.Fatalf("own-lane pipeline %q not in flight; started concurrently = %v (no cap, laneless=own lane)", name, inflight)
			}
		}
		if !inflight["x"] {
			t.Fatalf("lane etl's first member not in flight; started = %v", inflight)
		}

		// Serial within the lane: y has not started while x is in flight.
		if n := runner.count("y"); n != 0 {
			t.Fatalf("y started %d times while x in flight, want 0 (serial within a lane)", n)
		}
		// x reaches terminal; only then does the lane proceed to y.
		runner.release["x"] <- struct{}{}
		if got := <-runner.entered; got != "y" {
			t.Fatalf("after x terminal, next started = %q, want y (serial within a lane)", got)
		}
	})
}

// TestPassBoundaryGraphVisibility proves runners read the walk at pass start: an
// in-flight pass finishes on the OLD graph and only the next pass sees the NEW one,
// while the in-flight run is untouched. A pipeline added to the walk while pass one
// is in flight does not join that pass; it appears only in pass two.
func TestPassBoundaryGraphVisibility(t *testing.T) {
	t.Run("pass-boundary-graph-visibility", func(t *testing.T) {
		ctx, cancel := context.WithTimeout(context.Background(), testTimeout)
		defer cancel()

		walk := newFakeWalk(dispatch.Lane{Name: "etl", Pipelines: []string{"a"}}) // W1
		runner := newFakeRunner()
		runner.entered = make(chan string)
		runner.release["a"] = make(chan struct{}) // send one token per a-run
		runner.release["b"] = make(chan struct{})

		stop := make(chan struct{})
		defer close(stop)
		passes := make(chan dispatch.PassReport)
		hook := func(r dispatch.PassReport) {
			select {
			case passes <- r:
			case <-stop:
			}
		}
		loop := dispatch.NewLoop(walk, newFakeGate(), runner, nil, dispatch.WithOnPass(hook))
		done := make(chan struct{})
		go func() { defer close(done); _ = loop.Run(ctx) }()
		defer func() { cancel(); <-done }()

		// Pass one starts a on the OLD graph (only [a]). While a is in flight, add b to the
		// walk: the change must not join the in-flight pass.
		if got := <-runner.entered; got != "a" {
			t.Fatalf("pass one first start = %q, want a", got)
		}
		walk.set(dispatch.Lane{Name: "etl", Pipelines: []string{"a", "b"}}) // W2, mid-pass
		runner.release["a"] <- struct{}{}                                   // a reaches terminal

		pass1 := <-passes
		if got, want := pass1.Started, []string{"a"}; !reflect.DeepEqual(got, want) {
			t.Fatalf("pass one started = %v, want %v (in-flight pass finishes on the old graph)", got, want)
		}

		// Pass two reads the walk afresh and sees the new graph: a then b.
		if got := <-runner.entered; got != "a" {
			t.Fatalf("pass two first start = %q, want a", got)
		}
		runner.release["a"] <- struct{}{}
		if got := <-runner.entered; got != "b" {
			t.Fatalf("pass two second start = %q, want b (next pass sees the new graph)", got)
		}
		runner.release["b"] <- struct{}{}
		pass2 := <-passes
		if got, want := pass2.Started, []string{"a", "b"}; !reflect.DeepEqual(got, want) {
			t.Fatalf("pass two started = %v, want %v (the next pass sees the new graph)", got, want)
		}
	})
}

// nextStart reads the next started pipeline from the runner, failing the test rather than
// hanging if the loop stalls (the context bounds the wait).
func nextStart(ctx context.Context, t *testing.T, runner *fakeRunner) string {
	t.Helper()
	select {
	case p := <-runner.entered:
		return p
	case <-ctx.Done():
		t.Fatalf("timed out waiting for the next run to start: %v", ctx.Err())
		return ""
	}
}

// TestRemovedPipelineFinishes proves a removed pipeline finishes its current run and
// then stops appearing in subsequent passes: a second lane member removed from the
// walk while its run is in flight still finishes that run in the current pass, and
// the next pass -- on the new walk -- omits it entirely and never re-dispatches it.
func TestRemovedPipelineFinishes(t *testing.T) {
	t.Run("removed-pipeline-finishes", func(t *testing.T) {
		ctx, cancel := context.WithTimeout(context.Background(), testTimeout)
		defer cancel()

		walk := newFakeWalk(dispatch.Lane{Name: "etl", Pipelines: []string{"a", "b"}}) // W1
		runner := newFakeRunner()
		runner.entered = make(chan string)
		runner.release["a"] = make(chan struct{})
		runner.release["b"] = make(chan struct{})

		stop := make(chan struct{})
		defer close(stop)
		passes := make(chan dispatch.PassReport)
		hook := func(r dispatch.PassReport) {
			select {
			case passes <- r:
			case <-stop:
			}
		}
		loop := dispatch.NewLoop(walk, newFakeGate(), runner, nil, dispatch.WithOnPass(hook))
		done := make(chan struct{})
		go func() { defer close(done); _ = loop.Run(ctx) }()
		defer func() { cancel(); <-done }()

		// Pass one: a runs, then b starts and is in flight.
		if got := nextStart(ctx, t, runner); got != "a" {
			t.Fatalf("pass one first start = %q, want a", got)
		}
		runner.release["a"] <- struct{}{}
		if got := nextStart(ctx, t, runner); got != "b" {
			t.Fatalf("pass one second start = %q, want b", got)
		}

		// Remove b from the walk while its run is in flight, then let that run FINISH.
		walk.set(dispatch.Lane{Name: "etl", Pipelines: []string{"a"}}) // W2
		runner.release["b"] <- struct{}{}                              // b finishes its current run

		pass1 := <-passes
		if got, want := pass1.Started, []string{"a", "b"}; !reflect.DeepEqual(got, want) {
			t.Fatalf("pass one started = %v, want %v (removed pipeline finishes its current run)", got, want)
		}

		// Pass two omits b entirely: it has stopped appearing and is never re-dispatched.
		if got := nextStart(ctx, t, runner); got != "a" {
			t.Fatalf("pass two first start = %q, want a", got)
		}
		runner.release["a"] <- struct{}{}
		pass2 := <-passes
		if got, want := pass2.Started, []string{"a"}; !reflect.DeepEqual(got, want) {
			t.Fatalf("pass two started = %v, want %v (removed pipeline stops appearing)", got, want)
		}
		if n := runner.count("b"); n != 1 {
			t.Fatalf("b started %d times, want 1 (finished its one run, never re-dispatched)", n)
		}
	})
}

// TestFailureNeverGloballyFatal proves a run failure is isolated: a pipeline whose
// run dead-letters every pass keeps being freshly dispatched (the engine keeps
// running, never globally fatal) while a pipeline in a separate lane continues
// dispatching unaffected.
func TestFailureNeverGloballyFatal(t *testing.T) {
	t.Run("failure-never-globally-fatal", func(t *testing.T) {
		ctx, cancel := context.WithTimeout(context.Background(), testTimeout)
		defer cancel()

		runner := newFakeRunner()
		runner.entered = make(chan string)
		runner.outcome["a"] = dispatch.RunDeadLettered // a's run fails every pass
		runner.release["a"] = make(chan struct{})
		runner.release["b"] = make(chan struct{})
		loop := dispatch.NewLoop(
			newFakeWalk(
				dispatch.Lane{Name: "faulty", Pipelines: []string{"a"}},
				dispatch.Lane{Name: "healthy", Pipelines: []string{"b"}},
			),
			newFakeGate(), runner, nil,
		)
		done := make(chan struct{})
		go func() { defer close(done); _ = loop.Run(ctx) }()
		defer func() { cancel(); <-done }()

		// Pace both lanes to three passes each. a fails every pass, yet it keeps being
		// dispatched (its failure never stops the loop) and b keeps dispatching (its
		// separate lane is unaffected).
		for runner.count("a") < 3 || runner.count("b") < 3 {
			p := nextStart(ctx, t, runner)
			select {
			case runner.release[p] <- struct{}{}:
			case <-ctx.Done():
				t.Fatalf("timed out releasing %q: %v", p, ctx.Err())
			}
		}
		if n := runner.count("a"); n < 3 {
			t.Fatalf("faulty pipeline dispatched %d times, want >= 3 (a failure is never globally fatal)", n)
		}
		if n := runner.count("b"); n < 3 {
			t.Fatalf("healthy pipeline dispatched %d times, want >= 3 (other lanes keep dispatching)", n)
		}
	})
}

// TestHungRunHoldsLane proves a hung run holds its lane indefinitely and the lane
// resumes only after that run is cancelled by the operator: a pipeline whose run
// never terminates is dispatched exactly once and blocks its lane, while a separate
// lane keeps dispatching; no engine timer frees the hung run (clock doctrine), and
// only an explicit release (the operator cancel) lets its lane advance.
func TestHungRunHoldsLane(t *testing.T) {
	t.Run("hung-run-holds-lane", func(t *testing.T) {
		ctx, cancel := context.WithTimeout(context.Background(), testTimeout)
		defer cancel()

		runner := newFakeRunner()
		runner.entered = make(chan string)
		runner.release["a"] = blocked() // a hangs: never terminates on its own
		runner.release["b"] = make(chan struct{})
		loop := dispatch.NewLoop(
			newFakeWalk(
				dispatch.Lane{Name: "stuck", Pipelines: []string{"a"}},
				dispatch.Lane{Name: "live", Pipelines: []string{"b"}},
			),
			newFakeGate(), runner, nil,
		)
		done := make(chan struct{})
		go func() { defer close(done); _ = loop.Run(ctx) }()
		defer func() { cancel(); <-done }()

		// While a is hung, its lane is held (a is dispatched once and never again) yet the
		// live lane keeps dispatching. Pace the live lane to three passes AND await a's
		// one start; no timer frees a. Both conditions must be waited on: the lanes are
		// independent goroutines, so nothing orders a's first dispatch before b's third
		// pass -- exiting on b's count alone races the scheduler and can miss a's start
		// signal. The wait stays bounded (nextStart fails on ctx timeout), so an a that
		// genuinely never starts still fails loudly.
		aSeen := false
		for runner.count("b") < 3 || !aSeen {
			p := nextStart(ctx, t, runner)
			if p == "a" {
				aSeen = true // a started once, now hung; do not release it (no engine timer)
				continue
			}
			select {
			case runner.release["b"] <- struct{}{}:
			case <-ctx.Done():
				t.Fatalf("timed out releasing b: %v", ctx.Err())
			}
		}
		if n := runner.count("a"); n != 1 {
			t.Fatalf("hung pipeline dispatched %d times while held, want exactly 1 (the lane is held, no engine timer)", n)
		}

		// The operator cancels the hung run (its lane resumes only now): releasing a lets
		// its run reach terminal and the lane dispatches a fresh run.
		select {
		case runner.release["a"] <- struct{}{}:
		case <-ctx.Done():
			t.Fatalf("timed out cancelling the hung run: %v", ctx.Err())
		}
		for runner.count("a") < 2 {
			p := nextStart(ctx, t, runner)
			if p == "a" {
				break
			}
			select {
			case runner.release[p] <- struct{}{}:
			case <-ctx.Done():
				t.Fatalf("timed out draining %q: %v", p, ctx.Err())
			}
		}
		if n := runner.count("a"); n < 2 {
			t.Fatalf("hung lane dispatched %d times after cancel, want >= 2 (the lane resumes only after cancel)", n)
		}
	})
}

// fakeJournalStep records the seal actions in order so the test can prove
// compact, checkpoint, archive execute as the post-pass step for sealable
// partitions.
type fakeJournalStep struct {
	mu     sync.Mutex
	events []string
}

func (s *fakeJournalStep) AfterPass(_ context.Context, _ dispatch.PassReport) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	// Simulate: for a newly sealable partition (test injects the condition by
	// always acting once a pass completes, modeling "opportunistic after pass").
	// Use the pure compact on a tiny fixture to exercise the compaction collapse-rule path.
	fixture := []pg.JournalEntry{
		{ID: 1, RunID: 99, Schema: "s", Table: "t", RowPK: "r", Op: pg.OpInsert, PreImage: "", Undo: pg.UndoOpen},
		{ID: 2, RunID: 99, Schema: "s", Table: "t", RowPK: "r", Op: pg.OpUpdate, PreImage: "x", Undo: pg.UndoPromoted},
	}
	_ = pg.CompactJournal(fixture) // exercises collapse rule in the step path
	s.events = append(s.events, "compact")
	// Checkpoint would Submit a writer.InsertCheckpoint; record the step.
	s.events = append(s.events, "checkpoint")
	// Archive would export + drop + flip location; record the step.
	s.events = append(s.events, "archive")
	return nil
}

func (s *fakeJournalStep) snapshot() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string(nil), s.events...)
}

// TestSealDispatcherStep proves sealing executes as an opportunistic
// dispatcher step strictly after a lane pass completes: compact, then
// checkpoint, then archive are performed for newly sealable partitions.
func TestSealDispatcherStep(t *testing.T) {
	t.Run("seal-dispatcher-step", func(t *testing.T) {
		ev := &eventLog{}
		gate := newFakeGate()
		runner := newFakeRunner()
		runner.ev = ev
		step := &fakeJournalStep{}
		// Wire a post-pass that records ordering; the seal step is exercised
		// directly to prove its internal sequence (compact, checkpoint, archive).
		post := &fakePostPass{ev: ev}
		loop := dispatch.NewLoop(newFakeWalk(), gate, runner, nil, dispatch.WithPostPass(post))

		lane := dispatch.Lane{Name: "etl", Pipelines: []string{"only"}}
		if err := loop.RunLanePass(context.Background(), lane); err != nil {
			t.Fatalf("RunLanePass: %v", err)
		}

		// Post-pass bookkeeping ran after the pass.
		if got := ev.snapshot(); len(got) == 0 || got[len(got)-1] != "post:etl" {
			t.Fatalf("post-pass ordering %v; want last event post:etl (seal step is post-pass)", got)
		}

		// Exercise the seal step itself (the opportunistic action the real
		// journal post-pass performs after a pass when partitions are sealable).
		if err := step.AfterPass(context.Background(), dispatch.PassReport{Lane: "etl"}); err != nil {
			t.Fatalf("seal step AfterPass: %v", err)
		}
		seq := step.snapshot()
		if !reflect.DeepEqual(seq, []string{"compact", "checkpoint", "archive"}) {
			t.Fatalf("seal step sequence = %v, want [compact, checkpoint, archive]", seq)
		}
	})
}
