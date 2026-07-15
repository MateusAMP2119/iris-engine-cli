package dispatch

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"time"

	"github.com/MateusAMP2119/iris-lakehouse/internal/store"
)

// idleReadInterval bounds how long the perpetual loop waits before re-reading the
// walk while WHOLLY idle (no lane running). It is not a dispatch timer and gates
// nothing on a clock: the running path is event-driven on pass completions and
// meta-change wakes (clock doctrine), and this bounded wait only wakes an
// otherwise-idle loop as a safety net -- a graph change whose bump was somehow
// missed, or a composition with no Events wired, is picked up promptly rather
// than never. With the watermark wired, an idle iteration is the doctrine's
// accepted hot idle loop kept cheap: the walk read alone, no gate evaluation and
// no run for any parked lane.
const idleReadInterval = 250 * time.Millisecond

// This file is the perpetual lane loop: the leader-owned dispatch runtime that turns
// the persisted walk into runs. It composes the lane walk (lane.go's BuildWalk, one
// goroutine per lane), the depends_on gate (gate.go, pass eligibility), and the
// run-start seam (run.go's RunManager, behind FreshRunner) into the pass semantics
// the engine runs forever once a leader wins the lock.
//
// Pass semantics:
//   - One goroutine per lane, a perpetual per-lane loop. Distinct lanes run in
//     parallel with no engine cap; a laneless pipeline is its own lane (BuildWalk).
//   - The loop is watermark-parked (events.go): a lane re-passes only when the
//     meta-change sequence advanced since its last pass started -- every
//     engine-visible cause bumps it through the single dispatcher -- so an idle
//     graph dispatches nothing and costs nothing beyond the bounded walk re-read.
//     Root (edge-less) pipelines are gated on their declaration (rootgate.go), so
//     a pass on an unchanged graph starts no run: no loop run without an
//     unconsumed cause.
//   - Each pass reads the walk at pass start (a snapshot): a graph change mid-pass
//     lands only at the next pass, and an in-flight run is never touched. A removed
//     pipeline finishes its current run, then stops appearing.
//   - Within a lane, members are dispatched serially in composer order (member N+1
//     starts only after member N reaches a terminal state), each open-gated one as a
//     FRESH run on current data (cause=loop). A failed run is never retried or backed
//     off; re-execution is only ever an explicit replay. A run failure is isolated:
//     it never gates the walk, never stops the loop, and never blocks another lane.
//   - A hung run holds its lane indefinitely (no engine timeout -- clock doctrine):
//     the lane's next pipeline waits, and only an operator cancel frees it; other
//     lanes keep dispatching.
//   - Dispatcher-owned bookkeeping (failure propagation, replay, snapshot pin,
//     pruning, journal lifecycle) runs opportunistically AFTER a lane pass completes,
//     never mid-pass.

// WalkReader reads the current lane walk from meta. The loop calls it once at each
// pass's start, so the pass runs on that snapshot even if the graph changes mid-pass:
// an in-flight pass finishes on the old graph and the next pass reads the new one. A
// meta-backed implementation (lane rows plus the registered-pipeline set, fed through
// BuildWalk) and a fake both satisfy it.
type WalkReader interface {
	// Walk returns the current per-lane runnable walk, in BuildWalk's stable order.
	Walk(ctx context.Context) ([]Lane, error)
}

// PassGate resolves a pipeline's depends_on eligibility at its turn in a pass,
// exactly like the manual path (manual.go): an ungated pipeline is always eligible,
// an open gate runs and consumes the resolved upstreams 1:1, a closed gate mints no
// run, and a poisoned gate defers to post-pass failure propagation. The loop
// evaluates it at each member's turn -- after the previous same-lane member reached
// terminal -- so a same-lane dependent sees the upstream's run of this pass. The
// daemon's lane plane supplies the meta-backed implementation (edges joined to each
// upstream's latest run, over the run_inputs consumed check); a fake satisfies it in
// tests.
type PassGate interface {
	// Eligible resolves the pipeline's gate for this pass turn.
	Eligible(ctx context.Context, pipeline string) (Decision, error)
}

// FreshRunner starts a fresh cause=loop run of an open-gated pipeline and blocks
// until it reaches a terminal state, returning that disposition. It is the loop's
// seam onto run execution (run.go): the daemon's lane-plane adapter mints the run
// (pinning its snapshot at dispatch), execs the subprocess, and records the terminal
// transition through the single writer, while a test fakes it. A returned error
// means the run could not be carried out at all (for example ctx was cancelled) and
// stops the lane's pass; a run that executes and then dead-letters is not an error --
// it returns (RunDeadLettered, nil), and the lane proceeds to its next member,
// because composer order never gates.
type FreshRunner interface {
	// StartFresh mints and runs rec (cause=loop), blocking until it is terminal.
	StartFresh(ctx context.Context, rec store.RunRecord) (RunOutcome, error)
}

// PassReport is what one lane pass produced, handed to the post-pass bookkeeping: the
// lane, the pipelines started as fresh runs this pass in composer order, and the
// pipelines whose gate poisoned (an awaited upstream dead-lettered), each carrying the
// gate decision the post-pass propagation plan reads. It is the loop's account of a
// completed pass, never a mid-pass view.
type PassReport struct {
	// Lane is the lane the pass walked.
	Lane string
	// Pipelines are the lane's members in composer order -- the walked set, whether
	// or not each started a run this pass. Post-pass retention scopes to it: the
	// lane's pipelines are pruned on the lane's own pass boundary, and lanes never
	// share a pipeline, so concurrent lane post-passes never prune the same run.
	Pipelines []string
	// Started are the pipelines started as fresh cause=loop runs this pass, in
	// composer order.
	Started []string
	// Poisoned are the pipelines whose gate resolved poisoned this pass (deferred to
	// post-pass failure propagation), in composer order.
	Poisoned []PoisonedMember
}

// PoisonedMember is one pipeline whose gate poisoned in a pass: its name and the gate
// decision, so post-pass propagation reads the poisoned edges (PlanPropagation) without
// re-evaluating the gate. The propagation write itself is the post-pass step's, never
// the pass's -- a poisoned gate mid-pass records only that it poisoned.
type PoisonedMember struct {
	// Pipeline is the poisoned dependent.
	Pipeline string
	// Decision is the gate decision (its Ledger names the poisoned edges).
	Decision Decision
}

// PostPass is the dispatcher-owned bookkeeping the loop runs opportunistically after
// a lane pass completes, never mid-pass: failure propagation for the pass's poisoned
// gates, replay processing, snapshot-pin and journal-ceiling stamping, count-based
// pruning, and journal lifecycle. The loop hands it the pass report and invokes it
// exactly once per lane pass, after every one of that lane's runs reached a terminal
// state. A meta-backed implementation (the single writer) and a fake both satisfy it;
// a nil PostPass runs no bookkeeping (the walk-only wiring tests use).
type PostPass interface {
	// AfterPass runs the dispatcher-owned bookkeeping for a completed lane pass.
	AfterPass(ctx context.Context, report PassReport) error
}

// freshRunRecord builds the run record for an open-gated pipeline's fresh loop run: a
// cause=loop run consuming exactly the upstream runs the gate resolved
// (Decision.Consume, one run_inputs row per edge, 1:1). It is the loop-owned shape
// only; the dispatch-time snapshot pin and declaration checksum are filled by the
// run-start adapter, never here. It carries no replayed_from and no reference to any
// prior run, so a loop run is always fresh -- never a retry. If artifactHash is
// non-nil it is recorded (built run); nil for dev.
func freshRunRecord(pipeline string, d Decision, artifactHash *string) store.RunRecord {
	rec := store.RunRecord{
		Pipeline:               pipeline,
		Cause:                  store.CauseLoop,
		ConsumedUpstreamRunIDs: d.Consume,
	}
	rec.ArtifactHash = artifactHash
	return rec
}

// PlanFreshRuns computes the fresh cause=loop runs a lane pass starts, in composer
// order, from each member's depends_on gate decision alone. It is pure -- no I/O --
// and, crucially, a function of the walk and the gate alone: it takes no prior run
// outcome, holds no retry queue, and applies no backoff, so a failed or dead-lettered
// run is never re-dispatched here. The next pass starts a pipeline again only when
// its gate re-opens (an ungated pipeline every pass on current data; a gated one on a
// new upstream success), which a failed run never triggers on its own; re-executing a
// dead-lettered run is only ever an explicit replay (cause=replay), which this
// function never emits.
//
// An open gate (Decision.Run) yields exactly one fresh run consuming the resolved
// upstreams 1:1; a closed gate (nothing new to consume, or an unmet dependency) yields
// none; a poisoned gate yields none here (failure propagates as a never-executed
// dead-lettered run in post-pass bookkeeping, not a fresh loop run). A member absent from
// decide is treated as ungated (always runs), never panicking.
func PlanFreshRuns(members []string, decide map[string]Decision) []store.RunRecord {
	var runs []store.RunRecord
	for _, pipeline := range members {
		d, ok := decide[pipeline]
		if !ok {
			// Absent: ungated, always eligible (Decide over no edges runs every pass).
			d = Decision{Run: true}
		}
		if d.Poisoned || !d.Run {
			continue // poisoned propagates post-pass; a closed gate mints no run.
		}
		runs = append(runs, freshRunRecord(pipeline, d, nil))
	}
	return runs
}

// Loop is the engine's perpetual lane dispatch runtime: one goroutine per lane, each
// running its lane's pass in a perpetual loop, distinct lanes in parallel with no
// engine cap. It reads the walk at each pass start, gates and starts each lane's
// members serially in composer order, and runs the dispatcher-owned bookkeeping
// after every lane pass. It holds only seams, so it is composed with fakes or the
// real meta+exec stack alike.
type Loop struct {
	walk   WalkReader
	gate   PassGate
	runner FreshRunner
	post   PostPass
	logger *slog.Logger

	// events, when set, is the leader's meta-change watermark: a lane whose last
	// pass started at the current sequence is parked (not re-spawned) until the
	// sequence advances, and a wholly idle loop blocks on the wake channel instead
	// of spinning. Nil events park nothing: every walk lane re-spawns at each pass
	// boundary (the walk-only wiring tests use this).
	events *Events

	// onPass, when set, is invoked after each lane pass's post-pass bookkeeping
	// completes: an observability hook the daemon and tests synchronize on. It never
	// influences dispatch.
	onPass func(PassReport)
}

// LoopOption configures a Loop at construction.
type LoopOption func(*Loop)

// WithPostPass sets the dispatcher-owned post-pass bookkeeping seam. Absent it, a lane
// pass runs no post-pass work (the walk-only wiring tests use this).
func WithPostPass(post PostPass) LoopOption {
	return func(l *Loop) { l.post = post }
}

// WithOnPass sets the per-pass observability hook, invoked after each lane pass's
// post-pass bookkeeping completes. A nil hook is ignored.
func WithOnPass(hook func(PassReport)) LoopOption {
	return func(l *Loop) { l.onPass = hook }
}

// WithEvents sets the leader's meta-change watermark the loop parks lanes on: a
// lane re-passes only when the sequence advanced since its last pass started.
// Absent it, every walk lane re-spawns at each pass boundary.
func WithEvents(e *Events) LoopOption {
	return func(l *Loop) { l.events = e }
}

// NewLoop builds the perpetual lane loop over the walk read seam, the depends_on gate,
// and the fresh-run seam. A nil logger discards output. Options add the post-pass
// bookkeeping seam and the per-pass hook.
func NewLoop(walk WalkReader, gate PassGate, runner FreshRunner, logger *slog.Logger, opts ...LoopOption) *Loop {
	if logger == nil {
		logger = slog.New(slog.NewTextHandler(io.Discard, nil))
	}
	l := &Loop{walk: walk, gate: gate, runner: runner, logger: logger}
	for _, o := range opts {
		o(l)
	}
	return l
}

// runLanePass walks lane's members once, serially in composer order: at each member's
// turn it resolves the gate and, on an open gate, starts a fresh cause=loop run and
// blocks until that run reaches a terminal state before the next member. It never gates
// on a member's outcome -- a dead-lettered run does not stop the walk -- so a run that
// merely fails is not an error here; a poisoned gate starts no run and records the
// member for post-pass propagation; a closed gate mints no run. It returns a non-nil
// error only when a step could not be carried out (ctx cancellation, or a gate/runner
// operational error), stopping the pass so the runner is reusable rather than left
// mid-lane.
func (l *Loop) runLanePass(ctx context.Context, lane Lane) (PassReport, error) {
	report := PassReport{Lane: lane.Name, Pipelines: append([]string(nil), lane.Pipelines...)}
	for _, pipeline := range lane.Pipelines {
		if err := ctx.Err(); err != nil {
			return report, err
		}
		d, err := l.gate.Eligible(ctx, pipeline)
		if err != nil {
			return report, fmt.Errorf("dispatch: lane %q gate %q: %w", lane.Name, pipeline, err)
		}
		switch {
		case d.Poisoned:
			// An awaited upstream dead-lettered: no run starts, and the member is
			// deferred to post-pass failure propagation. The lane proceeds -- composer
			// order never gates on a member's disposition.
			report.Poisoned = append(report.Poisoned, PoisonedMember{Pipeline: pipeline, Decision: d})
		case d.Run:
			// Open gate: start a FRESH run on current data (cause=loop), consuming the
			// resolved upstreams 1:1, and await its terminal state before the next
			// member (serial within the lane). The outcome is deliberately discarded:
			// a failed run is isolated and never gates the walk, and it is never
			// retried -- the next pass starts a fresh run only if the gate re-opens.
			report.Started = append(report.Started, pipeline)
			if _, err := l.runner.StartFresh(ctx, freshRunRecord(pipeline, d, nil)); err != nil {
				return report, fmt.Errorf("dispatch: lane %q start %q: %w", lane.Name, pipeline, err)
			}
		default:
			// Closed gate: nothing new to consume, or an unmet dependency. No run row
			// this pass (absence is the record); the lane proceeds.
		}
	}
	return report, nil
}

// RunLanePass runs one complete lane pass: the ordered, serial member walk followed by
// the dispatcher-owned post-pass bookkeeping (only after the pass completes, never
// mid-pass). A pass cut short by ctx cancellation runs no post-pass work and returns the
// cancellation, so the loop exits promptly on shutdown. Any in-flight runs the pass
// started are left to finish (the engine never kills a run unilaterally -- clock
// doctrine).
func (l *Loop) RunLanePass(ctx context.Context, lane Lane) error {
	report, err := l.runLanePass(ctx, lane)
	if err != nil {
		return err
	}
	// POST-PASS: dispatcher-owned bookkeeping runs opportunistically now that every run
	// of this pass has reached a terminal state -- never interleaved mid-pass.
	if l.post != nil {
		if err := l.post.AfterPass(ctx, report); err != nil {
			return fmt.Errorf("dispatch: lane %q post-pass: %w", lane.Name, err)
		}
	}
	// The per-pass hook fires only while the pass's term is still live: Run
	// deliberately never joins in-flight lane goroutines on shutdown, so a pass
	// completing after its context was cancelled would otherwise increment a
	// LATER term's counter (the pass counter resets on leader change, and a
	// deposed term's stragglers must not leak into the new term's counts).
	if l.onPass != nil && ctx.Err() == nil {
		l.onPass(report)
	}
	return nil
}

// laneDone signals that a lane's perpetual-loop goroutine finished one pass.
type laneDone struct{ lane string }

// Run drives the perpetual dispatch loop until ctx is cancelled: one goroutine per
// lane, each running that lane's pass, distinct lanes in parallel with no engine cap.
// It reads the walk at each reconcile point (a pass boundary or a watermark wake),
// starts a pass goroutine for every eligible walk lane not already running, then
// blocks until one lane finishes its pass, the watermark advances, or ctx is
// cancelled -- so the walk is re-read at each boundary, a new lane starts looping,
// and a removed lane is not re-spawned once its current pass finishes (its in-flight
// run finishes first). A lane whose run hangs holds its lane indefinitely (its
// goroutine never finishes, so it is never re-spawned), while every other lane keeps
// dispatching -- a hung or failed run is isolated, never globally fatal, and no
// engine timer frees it (clock doctrine).
//
// With an Events watermark wired, a lane is eligible only when the sequence advanced
// since its last pass STARTED (or it has never passed this term): a pass whose own
// runs wrote meta re-passes once more and then parks on its first empty pass, so an
// idle graph costs the bounded walk re-read and nothing else -- no gate query, no
// run. Every engine-visible cause bumps the watermark through the single dispatcher,
// so a parked lane wakes exactly when a cause lands. Without a watermark every walk
// lane is always eligible (the walk-only wiring tests).
//
// It is clockless: it blocks on lane-pass completions, watermark wakes, and ctx --
// the bounded idle re-read is a safety net, never a dispatch gate. On ctx
// cancellation it returns promptly without joining in-flight lane goroutines -- those
// observe ctx through their run seam and drain themselves -- so shutdown is not delayed
// by a still-running (or hung) lane.
func (l *Loop) Run(ctx context.Context) error {
	running := map[string]bool{}
	// lastSeq is each lane's watermark sequence at its last pass START (this term).
	// Recording the sequence before the pass reads the walk means a bump landing
	// mid-pass re-opens eligibility at the pass boundary: a change is never missed,
	// at worst one extra (empty, cheap) pass runs.
	lastSeq := map[string]uint64{}
	done := make(chan laneDone)

	// wake is the watermark's coalescing channel, nil (never ready) when no
	// watermark is wired, so the selects below degrade to the legacy shape.
	var wake <-chan struct{}
	if l.events != nil {
		wake = l.events.Wake()
	}

	for {
		lanes, err := l.walk.Walk(ctx)
		if err != nil {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			return fmt.Errorf("dispatch: read walk: %w", err)
		}

		// Start a pass goroutine for every eligible walk lane not already running. A
		// lane already running (its previous pass still in flight, e.g. a hung run) is
		// left alone: it holds its own lane and is re-spawned only once its current
		// pass finishes. A parked lane (watermark unchanged since its last pass
		// started) is skipped without a gate query or a run.
		walkNames := make(map[string]bool, len(lanes))
		for _, lane := range lanes {
			walkNames[lane.Name] = true
			if running[lane.Name] {
				continue
			}
			if l.events != nil {
				if seq, passed := lastSeq[lane.Name]; passed && seq == l.events.Seq() {
					continue // parked: no cause since this lane's last pass started
				}
				lastSeq[lane.Name] = l.events.Seq()
			}
			running[lane.Name] = true
			l.spawnLanePass(ctx, lane, done)
		}
		// Forget the park state of lanes the walk no longer names, so a removed
		// pipeline's entry does not accumulate and a re-added lane passes fresh.
		for name := range lastSeq {
			if !walkNames[name] && !running[name] {
				delete(lastSeq, name)
			}
		}

		// No lane running: idle. Block until the watermark advances (a cause landed),
		// or -- the safety net, and the only driver when no watermark is wired -- the
		// bounded re-read elapses, so a graph applied to a cold leader is picked up
		// promptly rather than never. An idle iteration with every lane parked costs
		// the walk read alone.
		if len(running) == 0 {
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-wake:
			case <-time.After(idleReadInterval):
			}
			continue
		}

		// Block until one lane finishes a pass (reconcile: re-read the walk and re-spawn
		// it), the watermark advances (a cause may unpark a lane other than the running
		// ones -- reconcile without waiting for a possibly hung lane), or ctx is
		// cancelled (stop dispatching promptly).
		select {
		case d := <-done:
			delete(running, d.lane)
		case <-wake:
		case <-ctx.Done():
			return ctx.Err()
		}
	}
}

// spawnLanePass launches one lane's pass on its own goroutine (one goroutine per lane)
// and reports completion on done. A pass error is isolated: it is logged (unless it is
// mere ctx cancellation) and never propagated, so one lane's failure to run neither stops
// the loop nor blocks another lane. The completion send is ctx-guarded so the goroutine
// never blocks forever after Run has stopped receiving.
func (l *Loop) spawnLanePass(ctx context.Context, lane Lane, done chan<- laneDone) {
	go func() {
		if err := l.RunLanePass(ctx, lane); err != nil && ctx.Err() == nil {
			l.logger.Warn("iris lane pass error", "lane", lane.Name, "err", err)
		}
		select {
		case done <- laneDone{lane: lane.Name}:
		case <-ctx.Done():
		}
	}()
}
