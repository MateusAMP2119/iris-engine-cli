package exectest_test

import (
	"bytes"
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/MateusAMP2119/iris-engine-cli/internal/exec"
	"github.com/MateusAMP2119/iris-engine-cli/internal/exec/exectest"
	"github.com/MateusAMP2119/iris-engine-cli/internal/store"
	"github.com/MateusAMP2119/iris-engine-cli/internal/store/storetest"
)

// TestFakeRunnerSatisfiesRunner proves the fake process runner stands behind the
// exec seam: it implements exec.Runner, so dispatch code runs against it with no
// real process.
//
// spec: S16/integration-fakes-interfaces
func TestFakeRunnerSatisfiesRunner(t *testing.T) {
	fake := exectest.New()
	fake.Script("noop", exectest.Outcome{Exit: 0})

	// The fake is assignable to the exec seam, and works when driven through it.
	var r exec.Runner = fake
	h, err := r.Start(context.Background(), exec.Spec{Argv: []string{"noop"}})
	if err != nil {
		t.Fatalf("Start through exec.Runner: %v", err)
	}
	st, err := h.Wait()
	if err != nil || st.Code != 0 || st.Signaled {
		t.Fatalf("Wait through the seam = (%+v, %v), want a clean exit 0", st, err)
	}
}

// TestFakeRunnerScriptedOutcome proves the fake streams a scripted subprocess's
// output to the seam's writers and reports its scripted exit status through
// Wait, all with no real process and a fake process-group handle.
//
// spec: S16/integration-fakes-interfaces
func TestFakeRunnerScriptedOutcome(t *testing.T) {
	ctx := context.Background()
	r := exectest.New()
	r.Script("main.py", exectest.Outcome{Stdout: "hello\n", Stderr: "warn\n", Exit: 5})

	var out, errb bytes.Buffer
	h, err := r.Start(ctx, exec.Spec{Argv: []string{"main.py"}, Stdout: &out, Stderr: &errb})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	if h.PGID() <= 0 {
		t.Errorf("PGID() = %d, want a positive fake process-group id", h.PGID())
	}
	st, err := h.Wait()
	if err != nil {
		t.Fatalf("Wait: %v", err)
	}
	if st.Code != 5 || st.Signaled {
		t.Errorf("exit status = %+v, want code 5, not signaled", st)
	}
	if out.String() != "hello\n" || errb.String() != "warn\n" {
		t.Errorf("streamed output = (%q, %q), want (hello, warn)", out.String(), errb.String())
	}
}

// TestDispatchShapedComposerOrderStreamCancel is the dispatch-shaped proof the
// task calls for: a serial lane starts three pipelines in composer order,
// streams each one's output, and cancels the middle run mid-flight. It exercises
// the exec fake and the meta-store fake together with no real process or
// database, and it synchronizes purely on channels and run state -- never a
// fixed sleep.
//
// spec: S16/integration-fakes-interfaces
func TestDispatchShapedComposerOrderStreamCancel(t *testing.T) {
	ctx := context.Background()
	runner := exectest.New()
	meta := storetest.New()

	// The lane's composer order. The middle pipeline hangs (Block) so it can be
	// cancelled mid-flight; the other two exit cleanly after streaming output.
	runner.Script("extract_orders", exectest.Outcome{Stdout: "extract ok\n", Exit: 0})
	runner.Script("reset_counters", exectest.Outcome{Stdout: "reset starting\n", Block: true})
	runner.Script("load_orders", exectest.Outcome{Stdout: "load ok\n", Exit: 0})
	composerOrder := []string{"extract_orders", "reset_counters", "load_orders"}

	// A canceller waits for the hung run to be handed to it, then cancels it by
	// killing its process group -- the engine's cancel path. The hand-off channel
	// is the synchronization point: no sleeps.
	hung := make(chan exec.Handle, 1)
	cancelled := make(chan struct{})
	go func() {
		h := <-hung
		if err := h.Kill(); err != nil {
			t.Errorf("cancel: Kill: %v", err)
		}
		close(cancelled)
	}()

	outputs := map[string]*bytes.Buffer{}

	// The serial lane runner: composer order, one run at a time.
	for _, pipeline := range composerOrder {
		run, err := meta.CreateRun(ctx, store.RunSpec{Pipeline: pipeline, Lane: "ingest"})
		if err != nil {
			t.Fatalf("CreateRun(%s): %v", pipeline, err)
		}
		buf := &bytes.Buffer{}
		outputs[pipeline] = buf

		h, err := runner.Start(ctx, exec.Spec{Argv: []string{pipeline}, Stdout: buf, Stderr: buf})
		if err != nil {
			t.Fatalf("Start(%s): %v", pipeline, err)
		}
		if _, err := meta.SetRunState(ctx, run.ID, store.RunRunning, store.WithHandle(h.PGID())); err != nil {
			t.Fatalf("mark running: %v", err)
		}

		// The hung pipeline gets handed to the canceller before we block on Wait.
		if pipeline == "reset_counters" {
			hung <- h
		}

		st, err := h.Wait()
		if err != nil {
			t.Fatalf("Wait(%s): %v", pipeline, err)
		}
		if st.Signaled {
			if _, err := meta.SetRunState(ctx, run.ID, store.RunDeadLettered, store.WithReason("cancelled")); err != nil {
				t.Fatalf("dead-letter: %v", err)
			}
		} else {
			if _, err := meta.SetRunState(ctx, run.ID, store.RunSucceeded, store.WithExitCode(st.Code)); err != nil {
				t.Fatalf("succeed: %v", err)
			}
		}
	}

	<-cancelled // the mid-flight cancel actually happened

	// Composer order preserved, and every run reached its expected terminal state.
	runs, err := meta.ListRuns(ctx, store.RunFilter{})
	if err != nil {
		t.Fatalf("ListRuns: %v", err)
	}
	wantState := map[string]store.RunState{
		"extract_orders": store.RunSucceeded,
		"reset_counters": store.RunDeadLettered,
		"load_orders":    store.RunSucceeded,
	}
	if len(runs) != len(composerOrder) {
		t.Fatalf("got %d runs, want %d", len(runs), len(composerOrder))
	}
	for i, r := range runs {
		if r.Pipeline != composerOrder[i] {
			t.Errorf("run %d = %s, want composer order %s", i, r.Pipeline, composerOrder[i])
		}
		if r.State != wantState[r.Pipeline] {
			t.Errorf("%s final state = %q, want %q", r.Pipeline, r.State, wantState[r.Pipeline])
		}
	}

	// Output streamed for every run, including the one cancelled mid-stream.
	for pipeline, want := range map[string]string{
		"extract_orders": "extract ok",
		"reset_counters": "reset starting",
		"load_orders":    "load ok",
	} {
		if got := outputs[pipeline].String(); !strings.Contains(got, want) {
			t.Errorf("%s output = %q, want it to contain %q", pipeline, got, want)
		}
	}
}

// TestFakeRunnerContextCancel proves a blocking scripted run also unblocks when
// the context passed to Start is cancelled -- the other half of the engine's
// cancel path -- reported as a signaled terminal status.
//
// spec: S16/integration-fakes-interfaces
func TestFakeRunnerContextCancel(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	r := exectest.New()
	r.Script("hang", exectest.Outcome{Block: true})

	var out bytes.Buffer
	h, err := r.Start(ctx, exec.Spec{Argv: []string{"hang"}, Stdout: &out, Stderr: &out})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}

	done := make(chan exec.ExitStatus, 1)
	go func() {
		st, _ := h.Wait()
		done <- st
	}()
	cancel() // cancel mid-flight; Wait must unblock

	st := <-done
	if !st.Signaled {
		t.Errorf("cancelled run status = %+v, want signaled", st)
	}
}

// TestFakeRunnerRejectsEmptyArgv proves the fake enforces the same precondition
// as the real runner and the Spec doc: Argv must be non-empty. A fake that
// silently ran an empty argv would let a bug slip past every integration test
// that the real runner rejects at once.
//
// spec: S16/integration-fakes-interfaces
func TestFakeRunnerRejectsEmptyArgv(t *testing.T) {
	r := exectest.New()
	_, err := r.Start(context.Background(), exec.Spec{})
	if err == nil {
		t.Fatal("Start with empty Argv = nil error, want the empty-argv precondition the real runner enforces")
	}
	if !strings.Contains(err.Error(), "empty argv") {
		t.Errorf("Start error = %v, want it to name the empty-argv precondition", err)
	}
}

// gateWriter blocks every Write until its gate is released, then records the
// bytes. It stands in for a backpressured writer (an unread pipe) to prove the
// fake does not stream inside Start.
type gateWriter struct {
	gate chan struct{}
	mu   sync.Mutex
	buf  bytes.Buffer
}

func (w *gateWriter) Write(p []byte) (int, error) {
	<-w.gate
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.buf.Write(p)
}

func (w *gateWriter) String() string {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.buf.String()
}

// TestFakeRunnerStreamsAsynchronously proves the fake streams output after Start
// returns, mirroring the real runner: Start does not block on a backpressured
// writer, and output is delivered concurrently with the handle's lifetime.
//
// spec: S16/integration-fakes-interfaces
func TestFakeRunnerStreamsAsynchronously(t *testing.T) {
	r := exectest.New()
	r.Script("stream", exectest.Outcome{Stdout: "streamed-output\n", Exit: 0})

	// A writer that blocks until released: a fake that streams inside Start would
	// hang here, exactly as the old synchronous fake did on an unread pipe.
	w := &gateWriter{gate: make(chan struct{})}

	type startResult struct {
		h   exec.Handle
		err error
	}
	started := make(chan startResult, 1)
	go func() {
		h, err := r.Start(context.Background(), exec.Spec{Argv: []string{"stream"}, Stdout: w})
		started <- startResult{h, err}
	}()

	var h exec.Handle
	select {
	case s := <-started:
		if s.err != nil {
			t.Fatalf("Start: %v", s.err)
		}
		h = s.h
	case <-time.After(1 * time.Second):
		t.Fatal("Start blocked on the backpressured writer; the fake must stream asynchronously after Start returns")
	}

	// Release the writer, then Wait: output is delivered concurrently with the
	// handle's lifetime and complete once Wait returns.
	close(w.gate)
	st, err := h.Wait()
	if err != nil {
		t.Fatalf("Wait: %v", err)
	}
	if st.Code != 0 || st.Signaled {
		t.Errorf("exit status = %+v, want code 0, not signaled", st)
	}
	if w.String() != "streamed-output\n" {
		t.Errorf("streamed output = %q, want %q", w.String(), "streamed-output\n")
	}
}

// errWriter fails every Write with a sentinel, to prove a streaming writer error
// reaches the caller through Wait, not Start.
type errWriter struct{}

var errWrite = errors.New("exectest: write failed")

func (errWriter) Write([]byte) (int, error) { return 0, errWrite }

// TestFakeRunnerWriterErrorSurfacesFromWait proves a writer error during
// streaming is reported from Wait, not Start, matching the real runner's seam:
// the run is dispatched, then its output capture fails.
//
// spec: S16/integration-fakes-interfaces
func TestFakeRunnerWriterErrorSurfacesFromWait(t *testing.T) {
	r := exectest.New()
	r.Script("noisy", exectest.Outcome{Stdout: "data\n", Exit: 0})

	h, err := r.Start(context.Background(), exec.Spec{Argv: []string{"noisy"}, Stdout: errWriter{}})
	if err != nil {
		t.Fatalf("Start surfaced the writer error, want it deferred to Wait: %v", err)
	}
	if _, werr := h.Wait(); !errors.Is(werr, errWrite) {
		t.Errorf("Wait error = %v, want the streaming writer error", werr)
	}
}
