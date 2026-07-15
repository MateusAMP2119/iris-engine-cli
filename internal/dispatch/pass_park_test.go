package dispatch_test

import (
	"context"
	"testing"

	"github.com/MateusAMP2119/iris-lakehouse/internal/dispatch"
)

// passReports wires a WithOnPass hook that forwards each report on a channel,
// stop-guarded so a report landing after the test finished never blocks the loop.
func passReports(t *testing.T) (hook func(dispatch.PassReport), reports <-chan dispatch.PassReport) {
	t.Helper()
	ch := make(chan dispatch.PassReport)
	stop := make(chan struct{})
	t.Cleanup(func() { close(stop) })
	return func(r dispatch.PassReport) {
		select {
		case ch <- r:
		case <-stop:
		}
	}, ch
}

// nextReport reads the next pass report, failing rather than hanging if the loop
// stalls (the context bounds the wait).
func nextReport(ctx context.Context, t *testing.T, reports <-chan dispatch.PassReport) dispatch.PassReport {
	t.Helper()
	select {
	case r := <-reports:
		return r
	case <-ctx.Done():
		t.Fatalf("timed out waiting for the next pass report: %v", ctx.Err())
		return dispatch.PassReport{}
	}
}

// TestLaneParksUntilCause proves the watermark parks an idle lane: with Events
// wired, a lane passes once (never passed this term), then does NOT re-pass while
// the sequence stands still, and a bump -- a cause landing -- unparks exactly one
// more pass. Without the bump the loop would spin the lane back to back, the
// busy loop of issue #172.
func TestLaneParksUntilCause(t *testing.T) {
	t.Run("lane-parks-until-cause", func(t *testing.T) {
		ctx, cancel := context.WithTimeout(context.Background(), testTimeout)
		defer cancel()

		events := dispatch.NewEvents()
		runner := newFakeRunner()
		hook, reports := passReports(t)
		loop := dispatch.NewLoop(
			newFakeWalk(dispatch.Lane{Name: "solo", Pipelines: []string{"a"}}),
			newFakeGate(), runner, nil,
			dispatch.WithOnPass(hook), dispatch.WithEvents(events),
		)
		done := make(chan struct{})
		go func() { defer close(done); _ = loop.Run(ctx) }()
		defer func() { cancel(); <-done }()

		// Pass one: the lane has never passed this term, so it runs unconditionally.
		if r := nextReport(ctx, t, reports); len(r.Started) != 1 || r.Started[0] != "a" {
			t.Fatalf("pass one started = %v, want [a]", r.Started)
		}

		// The watermark stands still: the lane must park. A cause lands (Bump), and
		// exactly one more pass runs.
		events.Bump()
		if r := nextReport(ctx, t, reports); len(r.Started) != 1 || r.Started[0] != "a" {
			t.Fatalf("pass two started = %v, want [a] (a cause unparks the lane)", r.Started)
		}

		// A second cause, a third pass: causes and passes stay one to one.
		events.Bump()
		nextReport(ctx, t, reports)

		// Stop the loop at a quiet point and count: exactly three dispatches, one per
		// cause boundary (the unconditional first pass, then one per bump). A loop
		// that fails to park would have dispatched dozens more between the reports.
		cancel()
		<-done
		if n := runner.count("a"); n != 3 {
			t.Fatalf("a dispatched %d times, want exactly 3 (first pass + one per cause; a parked lane never re-passes on nothing)", n)
		}
	})
}

// TestBumpMidPassRepassesOnce proves a cause landing while a lane's pass is in
// flight is never lost: the lane re-passes once at the boundary (its sequence was
// recorded at pass START, so the mid-pass bump re-opens eligibility) and then
// parks -- the lost-wake window is closed by ordering, not by polling.
func TestBumpMidPassRepassesOnce(t *testing.T) {
	t.Run("bump-mid-pass-repasses-once", func(t *testing.T) {
		ctx, cancel := context.WithTimeout(context.Background(), testTimeout)
		defer cancel()

		events := dispatch.NewEvents()
		runner := newFakeRunner()
		runner.entered = make(chan string)
		runner.release["a"] = make(chan struct{})
		hook, reports := passReports(t)
		loop := dispatch.NewLoop(
			newFakeWalk(dispatch.Lane{Name: "solo", Pipelines: []string{"a"}}),
			newFakeGate(), runner, nil,
			dispatch.WithOnPass(hook), dispatch.WithEvents(events),
		)
		done := make(chan struct{})
		go func() { defer close(done); _ = loop.Run(ctx) }()
		defer func() { cancel(); <-done }()

		// Pass one is in flight; a cause lands MID-PASS, then the run finishes.
		if got := nextStart(ctx, t, runner); got != "a" {
			t.Fatalf("pass one start = %q, want a", got)
		}
		events.Bump()
		runner.release["a"] <- struct{}{}
		nextReport(ctx, t, reports)

		// The mid-pass cause re-opens eligibility at the boundary: pass two runs.
		if got := nextStart(ctx, t, runner); got != "a" {
			t.Fatalf("after a mid-pass cause, next start = %q, want a (the bump is never lost)", got)
		}
		runner.release["a"] <- struct{}{}
		nextReport(ctx, t, reports)

		// No further cause: the lane parks after the catch-up pass.
		cancel()
		<-done
		if n := runner.count("a"); n != 2 {
			t.Fatalf("a dispatched %d times, want exactly 2 (one catch-up pass per mid-pass cause, then park)", n)
		}
	})
}

// TestWakeUnparksIdleLoopPromptly proves a wholly idle loop (every lane parked)
// wakes on the watermark's channel rather than only on the bounded fallback: a
// bump lands while nothing runs, and the next pass starts without waiting out
// the idle re-read interval. The test cannot observe the absence of a timer
// directly, but the wake path is the select branch the bump makes ready.
func TestWakeUnparksIdleLoopPromptly(t *testing.T) {
	t.Run("wake-unparks-idle-loop", func(t *testing.T) {
		ctx, cancel := context.WithTimeout(context.Background(), testTimeout)
		defer cancel()

		events := dispatch.NewEvents()
		runner := newFakeRunner()
		hook, reports := passReports(t)
		loop := dispatch.NewLoop(
			newFakeWalk(
				dispatch.Lane{Name: "one", Pipelines: []string{"a"}},
				dispatch.Lane{Name: "two", Pipelines: []string{"b"}},
			),
			newFakeGate(), runner, nil,
			dispatch.WithOnPass(hook), dispatch.WithEvents(events),
		)
		done := make(chan struct{})
		go func() { defer close(done); _ = loop.Run(ctx) }()
		defer func() { cancel(); <-done }()

		// Both lanes pass once, then the whole loop is idle (all parked).
		nextReport(ctx, t, reports)
		nextReport(ctx, t, reports)

		// A cause lands on the idle loop: BOTH lanes unpark (the watermark is
		// global -- eligibility is the lane's, the wake is the loop's).
		events.Bump()
		nextReport(ctx, t, reports)
		nextReport(ctx, t, reports)

		cancel()
		<-done
		if na, nb := runner.count("a"), runner.count("b"); na != 2 || nb != 2 {
			t.Fatalf("dispatch counts a=%d b=%d, want 2 and 2 (one first pass + one per cause, each lane)", na, nb)
		}
	})
}
