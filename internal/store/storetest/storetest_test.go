package storetest_test

import (
	"context"
	"errors"
	"testing"

	"github.com/MateusAMP2119/iris-engine-cli/internal/store"
	"github.com/MateusAMP2119/iris-engine-cli/internal/store/storetest"
)

// TestRunStateWireValues pins the run-state constants to the exact tokens the
// spec's runs DDL and wire grammar use (specification sections 4 and 7:
// `state in (queued, running, succeeded, dead_lettered)` and
// `state=dead_lettered`). E02's Postgres CHECK constraint and every --json
// golden depend on the underscore form; a faithful meta-store fake must speak
// the same tokens.
//
// spec: S16/integration-fakes-interfaces
func TestRunStateWireValues(t *testing.T) {
	for _, tc := range []struct {
		got  store.RunState
		want string
	}{
		{store.RunQueued, "queued"},
		{store.RunRunning, "running"},
		{store.RunSucceeded, "succeeded"},
		{store.RunDeadLettered, "dead_lettered"},
	} {
		if string(tc.got) != tc.want {
			t.Errorf("run-state wire value = %q, want %q (spec sections 4 and 7)", tc.got, tc.want)
		}
	}
}

// TestFakeSatisfiesStore proves the meta-store fake stands in for meta: it
// implements the store.Store interface, so any code written against the seam
// runs against the fake with no live meta database.
//
// spec: S16/integration-fakes-interfaces
func TestFakeSatisfiesStore(t *testing.T) {
	// The fake is assignable to the meta seam, and works when driven through it.
	var s store.Store = storetest.New()
	r, err := s.CreateRun(context.Background(), store.RunSpec{Pipeline: "p", Lane: "l"})
	if err != nil {
		t.Fatalf("CreateRun through store.Store: %v", err)
	}
	if r.State != store.RunQueued {
		t.Errorf("new run state = %q, want queued", r.State)
	}
}

// TestFakeRunLifecycle drives the run records and states the later dispatch
// tests depend on through the fake: create a queued run, transition it through
// running (with its process-group handle) to a terminal state, dead-letter
// another with a reason, and read them back by id and by filter in creation
// order.
//
// spec: S16/integration-fakes-interfaces
func TestFakeRunLifecycle(t *testing.T) {
	ctx := context.Background()
	s := storetest.New()

	// Create three runs in a fixed order; each starts queued with a monotonic
	// ordering sequence (identity, never a clock).
	specs := []store.RunSpec{
		{Pipeline: "extract_orders", Lane: "ingest"},
		{Pipeline: "reset_counters", Lane: "ingest"},
		{Pipeline: "load_orders", Lane: "ingest"},
	}
	var created []store.Run
	for _, sp := range specs {
		r, err := s.CreateRun(ctx, sp)
		if err != nil {
			t.Fatalf("CreateRun(%+v): %v", sp, err)
		}
		if r.State != store.RunQueued {
			t.Errorf("new run %s state = %q, want %q", r.ID, r.State, store.RunQueued)
		}
		if r.ID == "" {
			t.Errorf("new run for %s has empty id", sp.Pipeline)
		}
		created = append(created, r)
	}
	if a, b := created[0].Seq, created[1].Seq; a >= b {
		t.Errorf("ordering sequence not monotonic: %d then %d", a, b)
	}

	// extract: queued -> running (handle recorded) -> succeeded (exit 0).
	ex := created[0].ID
	if _, err := s.SetRunState(ctx, ex, store.RunRunning, store.WithHandle(4242)); err != nil {
		t.Fatalf("SetRunState running: %v", err)
	}
	got, err := s.SetRunState(ctx, ex, store.RunSucceeded, store.WithExitCode(0))
	if err != nil {
		t.Fatalf("SetRunState succeeded: %v", err)
	}
	if got.State != store.RunSucceeded {
		t.Errorf("state = %q, want succeeded", got.State)
	}
	if got.Handle != 4242 {
		t.Errorf("handle = %d, want 4242 (process-group id preserved)", got.Handle)
	}
	if got.ExitCode == nil || *got.ExitCode != 0 {
		t.Errorf("exit code = %v, want 0", got.ExitCode)
	}

	// reset: queued -> dead-lettered with a reason (single non-success terminal).
	// The reason is the spec's closed dead_letters.reason enum token; a cancelled
	// run maps to "stopped" (specification sections 4 and 8), never the prose
	// "cancelled".
	rc := created[1].ID
	dl, err := s.SetRunState(ctx, rc, store.RunDeadLettered, store.WithReason("stopped"))
	if err != nil {
		t.Fatalf("SetRunState dead-lettered: %v", err)
	}
	if dl.State != store.RunDeadLettered || dl.Reason != "stopped" {
		t.Errorf("dead-letter = (%q, %q), want (dead_lettered, stopped)", dl.State, dl.Reason)
	}

	// GetRun round-trips a stored run by id.
	back, err := s.GetRun(ctx, ex)
	if err != nil {
		t.Fatalf("GetRun(%s): %v", ex, err)
	}
	if back.State != store.RunSucceeded {
		t.Errorf("GetRun state = %q, want succeeded", back.State)
	}

	// ListRuns returns every run in creation order.
	all, err := s.ListRuns(ctx, store.RunFilter{})
	if err != nil {
		t.Fatalf("ListRuns: %v", err)
	}
	if len(all) != 3 {
		t.Fatalf("ListRuns returned %d runs, want 3", len(all))
	}
	for i := range all {
		if all[i].ID != created[i].ID {
			t.Errorf("ListRuns[%d] = %s, want creation order %s", i, all[i].ID, created[i].ID)
		}
	}

	// ListRuns filters by state and by pipeline.
	dead, err := s.ListRuns(ctx, store.RunFilter{State: store.RunDeadLettered})
	if err != nil {
		t.Fatalf("ListRuns(state): %v", err)
	}
	if len(dead) != 1 || dead[0].ID != rc {
		t.Errorf("dead-lettered filter = %+v, want just %s", dead, rc)
	}
	byPipe, err := s.ListRuns(ctx, store.RunFilter{Pipeline: "load_orders"})
	if err != nil {
		t.Fatalf("ListRuns(pipeline): %v", err)
	}
	if len(byPipe) != 1 || byPipe[0].Pipeline != "load_orders" {
		t.Errorf("pipeline filter = %+v, want just load_orders", byPipe)
	}
}

// TestFakeIsolatesState proves the fake returns copies, not internal aliases: a
// caller mutating a returned run (including its exit-code pointer) cannot corrupt
// the store's own state. A fake standing in for a database must have database-like
// value semantics.
//
// spec: S16/integration-fakes-interfaces
func TestFakeIsolatesState(t *testing.T) {
	ctx := context.Background()
	s := storetest.New()

	r, err := s.CreateRun(ctx, store.RunSpec{Pipeline: "p", Lane: "l"})
	if err != nil {
		t.Fatalf("CreateRun: %v", err)
	}
	done, err := s.SetRunState(ctx, r.ID, store.RunSucceeded, store.WithExitCode(3))
	if err != nil {
		t.Fatalf("SetRunState: %v", err)
	}

	// Mutate the returned copy in every writable field.
	done.State = store.RunDeadLettered
	done.Handle = 99
	*done.ExitCode = 127
	done.Reason = "tampered"

	back, err := s.GetRun(ctx, r.ID)
	if err != nil {
		t.Fatalf("GetRun: %v", err)
	}
	if back.State != store.RunSucceeded {
		t.Errorf("state leaked: %q, want succeeded", back.State)
	}
	if back.ExitCode == nil || *back.ExitCode != 3 {
		t.Errorf("exit-code pointer aliased: %v, want 3", back.ExitCode)
	}
	if back.Reason == "tampered" || back.Handle == 99 {
		t.Errorf("field leaked into stored run: %+v", back)
	}
}

// TestFakeUnknownRun proves the fake reports a missing run with the seam's
// sentinel error rather than a zero value, on every id-addressed method.
//
// spec: S16/integration-fakes-interfaces
func TestFakeUnknownRun(t *testing.T) {
	ctx := context.Background()
	s := storetest.New()

	if _, err := s.GetRun(ctx, "run-nope"); !errors.Is(err, store.ErrRunNotFound) {
		t.Errorf("GetRun(unknown) err = %v, want ErrRunNotFound", err)
	}
	if _, err := s.SetRunState(ctx, "run-nope", store.RunRunning); !errors.Is(err, store.ErrRunNotFound) {
		t.Errorf("SetRunState(unknown) err = %v, want ErrRunNotFound", err)
	}
}
