package daemon

// Unit tests for the daemon's in-flight run registry, the production InflightKiller
// the self-demotion kill acts through. The Candidate-level contract (a lost session
// kills in-flight runs) is proven end to end over the store fakes in
// failover_test.go; this covers the registry's own bookkeeping -- only tracked runs
// are killed, an untracked (reaped) run is never a target.

import (
	"context"
	"testing"

	"github.com/MateusAMP2119/iris-engine-cli/internal/exec"
	"github.com/MateusAMP2119/iris-engine-cli/internal/exec/exectest"
)

// startTracked scripts a blocking fake run, starts it, and returns its handle so a
// test can track it and later observe the kill.
func startTracked(t *testing.T, runner *exectest.Runner, program string) exec.Handle {
	t.Helper()
	h, err := runner.Start(context.Background(), exec.Spec{Argv: []string{program}})
	if err != nil {
		t.Fatalf("start %s: %v", program, err)
	}
	return h
}

// TestInflightRunsKillsOnlyTracked proves the self-demotion kill reaches every tracked
// run and no other: two tracked runs are both killed, and a run that was untracked
// (reaped) before the demotion is not a kill target.
func TestInflightRunsKillsOnlyTracked(t *testing.T) {
	runner := exectest.New()
	runner.Script("hang", exectest.Outcome{Block: true})

	reg := newInflightRuns()

	h1 := startTracked(t, runner, "hang")
	h2 := startTracked(t, runner, "hang")
	reg.track("1", h1)
	reg.track("2", h2)

	// A third run that completed and was untracked before the demotion.
	h3 := startTracked(t, runner, "hang")
	reg.track("3", h3)
	reg.untrack("3")

	if got := reg.KillInflight(); got != 2 {
		t.Fatalf("KillInflight signalled %d groups, want 2 (the two still-tracked runs)", got)
	}

	// The two tracked runs were killed (their blocking Wait resolves signaled); the
	// untracked run was never signalled by the registry.
	if st, _ := h1.Wait(); !st.Signaled {
		t.Errorf("tracked run 1 exited %+v, want a signaled (killed) termination", st)
	}
	if st, _ := h2.Wait(); !st.Signaled {
		t.Errorf("tracked run 2 exited %+v, want a signaled (killed) termination", st)
	}

	// A second kill after both are reaped signals nothing new only if they were
	// untracked; the registry still holds them (untrack is the reap goroutine's job),
	// so a repeat kill is idempotent and best-effort. Clean up so no handle leaks.
	reg.untrack("1")
	reg.untrack("2")
	if got := reg.KillInflight(); got != 0 {
		t.Errorf("KillInflight after untracking all signalled %d groups, want 0", got)
	}
	_ = h3.Kill()
	_, _ = h3.Wait()
}
