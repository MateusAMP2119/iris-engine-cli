package cli

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/MateusAMP2119/iris-lakehouse/internal/api"
	"github.com/MateusAMP2119/iris-lakehouse/internal/config"
)

// scriptedView wires a psView over scripted channels, a buffer, and a fixed
// geometry -- the loop under test with no terminal, poller, or daemon.
type scriptedView struct {
	v        *psView
	out      *bytes.Buffer
	keys     chan psKey
	polls    chan psPollMsg
	notes    chan string
	focusCh  chan string
	cancelCh chan string
}

func newScriptedView() *scriptedView {
	s := &scriptedView{
		out:      &bytes.Buffer{},
		keys:     make(chan psKey, 16),
		polls:    make(chan psPollMsg, 1),
		notes:    make(chan string, 1),
		focusCh:  make(chan string, 4),
		cancelCh: make(chan string, 4),
	}
	s.v = &psView{
		out: s.out, p: painter{}, size: func() (int, int) { return 80, 24 },
		keys: s.keys, polls: s.polls, notes: s.notes, focusCh: s.focusCh, cancelCh: s.cancelCh,
	}
	return s
}

// TestRunPsLoop proves the event loop's exits and message plumbing: q and a
// closed key stream exit clean, a failed poll exits errPsEngineGone, ctx
// cancellation exits clean, a confirmed cancel reaches the poller channel,
// and a fresh poll re-renders with the new snapshot.
func TestRunPsLoop(t *testing.T) {
	t.Run("run-ps-loop", func(t *testing.T) {
		t.Run("q exits clean", func(t *testing.T) {
			s := newScriptedView()
			s.keys <- key('q')
			if err := runPsLoop(context.Background(), s.v, newPsModel(psvFixture(), "")); err != nil {
				t.Fatalf("q exit = %v, want nil", err)
			}
			if !strings.Contains(s.out.String(), "ENGINE") {
				t.Error("the loop never rendered a frame")
			}
		})

		t.Run("a failed poll exits with the poll error", func(t *testing.T) {
			s := newScriptedView()
			s.polls <- psPollMsg{err: errPsEngineGone}
			if err := runPsLoop(context.Background(), s.v, newPsModel(psvFixture(), "")); err != errPsEngineGone {
				t.Fatalf("poll-failure exit = %v, want the poll error back", err)
			}
			// A reached daemon's refusal surfaces as the typed error, so ps()
			// can keep its exit-4 classification.
			s = newScriptedView()
			s.polls <- psPollMsg{err: &psHTTPError{status: 500, code: "internal", message: "meta down"}}
			var herr *psHTTPError
			if err := runPsLoop(context.Background(), s.v, newPsModel(psvFixture(), "")); !errors.As(err, &herr) {
				t.Fatalf("http poll-failure exit = %v, want the *psHTTPError back", err)
			}
		})

		t.Run("context cancellation exits clean", func(t *testing.T) {
			s := newScriptedView()
			ctx, cancel := context.WithCancel(context.Background())
			cancel()
			if err := runPsLoop(ctx, s.v, newPsModel(psvFixture(), "")); err != nil {
				t.Fatalf("cancelled-ctx exit = %v, want nil", err)
			}
		})

		t.Run("a confirmed cancel reaches the poller, its note lands on screen", func(t *testing.T) {
			s := newScriptedView()
			sb := &syncBuffer{}
			s.v.out = sb
			m := newPsModel(psvFixture(), "")
			m.pane = psPaneLogs // the target is the running run 14

			done := make(chan error, 1)
			go func() { done <- runPsLoop(context.Background(), s.v, m) }()

			s.keys <- key('c')
			s.keys <- key('y')
			select {
			case id := <-s.cancelCh:
				if id != "14" {
					t.Errorf("cancel request = %q, want 14", id)
				}
			case <-time.After(5 * time.Second):
				t.Fatal("the confirmed cancel never reached the poller channel")
			}
			s.notes <- "cancelled 14 (dead_lettered)"
			deadline := time.After(5 * time.Second)
			for !strings.Contains(sb.String(), "cancelled 14 (dead_lettered)") {
				select {
				case <-deadline:
					t.Fatal("the cancel note never rendered")
				case <-time.After(time.Millisecond):
				}
			}
			s.keys <- key('q')
			if err := <-done; err != nil {
				t.Fatalf("loop exit = %v, want nil", err)
			}
		})

		t.Run("the loop points the poller at the selection's run and follows it", func(t *testing.T) {
			s := newScriptedView()
			m := newPsModel(psvFixture(), "")
			s.keys <- key('j') // extract row: its only run is 12
			s.keys <- key('q')
			if err := runPsLoop(context.Background(), s.v, m); err != nil {
				t.Fatalf("loop exit = %v, want nil", err)
			}
			var got []string
			for {
				select {
				case f := <-s.focusCh:
					got = append(got, f)
					continue
				default:
				}
				break
			}
			if len(got) != 2 || got[0] != "14" || got[1] != "12" {
				t.Errorf("focus pushes = %v, want the initial 14 then the reselected 12", got)
			}
		})

		t.Run("a fresh poll re-renders the new snapshot", func(t *testing.T) {
			s := newScriptedView()
			sb := &syncBuffer{}
			s.v.out = sb
			m := newPsModel(psvFixture(), "")
			next := psvFixture()
			next.ps.Engine.Uptime = "9h9m"

			done := make(chan error, 1)
			go func() { done <- runPsLoop(context.Background(), s.v, m) }()
			s.polls <- psPollMsg{snap: next}
			deadline := time.After(5 * time.Second)
			for !strings.Contains(sb.String(), "9h9m") {
				select {
				case <-deadline:
					t.Fatal("the re-poll never re-rendered the engine facts")
				case <-time.After(time.Millisecond):
				}
			}
			s.keys <- key('q')
			if err := <-done; err != nil {
				t.Fatalf("loop exit = %v, want nil", err)
			}
		})
	})
}

// syncBuffer is a mutex-guarded frame sink for tests that read the output
// while the loop goroutine writes it.
type syncBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (s *syncBuffer) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.Write(p)
}

func (s *syncBuffer) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.String()
}

// staticLogs serves a run's captured output for the loop's poller tests.
type staticLogs struct{ text string }

func (s staticLogs) Logs(context.Context, string) (io.ReadCloser, error) {
	return io.NopCloser(strings.NewReader(s.text)), nil
}

// recordingCancel records the cancelled run id.
type recordingCancel struct{ last atomic.Value }

func (r *recordingCancel) CancelRun(_ context.Context, run string) error {
	r.last.Store(run)
	return nil
}

func (r *recordingCancel) CancelPipeline(_ context.Context, pipeline string) (string, error) {
	r.last.Store(pipeline)
	return "1", nil
}

// TestPollPs drives the real poller against the real api mux over a unix
// socket: every tick reads the whole history and the pipeline listing, a
// pointed focus tails the run's logs, a cancel request POSTs /run/cancel, and
// a stopped daemon ends the poller with the fatal message.
func TestPollPs(t *testing.T) {
	t.Setenv("IRIS_HOST", "")
	t.Setenv("IRIS_SOCKET", "")
	t.Setenv("IRIS_TOKEN", "")

	t.Run("poll-ps", func(t *testing.T) {
		sock := shortSocket(t)
		var sawAll atomic.Bool
		cancels := &recordingCancel{}
		role := api.NewRoleState()
		role.SetLeader()

		ln, err := net.Listen("unix", sock)
		if err != nil {
			t.Fatalf("listen unix %s: %v", sock, err)
		}
		mux := api.NewMux(
			api.WithRole(role),
			api.WithPs(psFunc(func(_ context.Context, all bool) (api.PsPayload, error) {
				sawAll.Store(all)
				return psFixture(), nil
			})),
			api.WithPipelines(&pipelinesListFunc{items: []api.PipelineListItem{
				{Name: "extract", Active: true, Lane: "ingest"},
			}}),
			api.WithRunLogs(staticLogs{text: "line one\nline two\n"}),
			api.WithRunCancel(cancels),
		)
		srv := &http.Server{Handler: mux, ReadHeaderTimeout: 5 * time.Second}
		go func() { _ = srv.Serve(ln) }()
		shutdown := func() { _ = srv.Shutdown(context.Background()) }
		t.Cleanup(shutdown)

		var out, errb bytes.Buffer
		a := newApp(&out, &errb)
		c := a.newPsDaemonClient(config.Settings{Socket: sock})

		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		polls := make(chan psPollMsg, 1)
		notes := make(chan string, 1)
		focusCh := make(chan string, 1)
		cancelCh := make(chan string, 1)
		done := make(chan struct{})
		go func() {
			pollPs(ctx, c, 5*time.Millisecond, focusCh, cancelCh, polls, notes)
			close(done)
		}()

		waitPoll := func(what string) psPollMsg {
			t.Helper()
			select {
			case pm := <-polls:
				return pm
			case <-time.After(5 * time.Second):
				t.Fatalf("poller never delivered %s", what)
				return psPollMsg{}
			}
		}

		pm := waitPoll("a first snapshot")
		if pm.err != nil {
			t.Fatalf("first poll failed: %v", pm.err)
		}
		if !sawAll.Load() {
			t.Error("the poller must read the whole history (?all=true)")
		}
		if len(pm.snap.pipelines) != 1 || pm.snap.pipelines[0].Lane != "ingest" {
			t.Errorf("snapshot listing = %+v, want the lane-carrying row", pm.snap.pipelines)
		}

		focusCh <- "7"
		deadline := time.After(5 * time.Second)
		for {
			pm = waitPoll("a focused snapshot")
			if pm.err != nil {
				t.Fatalf("focused poll failed: %v", pm.err)
			}
			if len(pm.snap.logs) == 2 && pm.snap.logs[0] == "line one" {
				break
			}
			select {
			case <-deadline:
				t.Fatalf("focused snapshot never carried the log tail: %+v", pm.snap.logs)
			default:
			}
		}

		cancelCh <- "7"
		select {
		case note := <-notes:
			if !strings.Contains(note, "cancelled 7") {
				t.Errorf("cancel note = %q, want the cancelled outcome", note)
			}
		case <-time.After(5 * time.Second):
			t.Fatal("the cancel outcome never returned")
		}
		if got, _ := cancels.last.Load().(string); got != "7" {
			t.Errorf("daemon cancelled %q, want 7", got)
		}

		// The daemon stopping mid-view ends the poller with the fatal message.
		shutdown()
		_ = ln.Close()
		deadline = time.After(5 * time.Second)
		for {
			select {
			case pm = <-polls:
				if pm.err != nil {
					select {
					case <-done:
					case <-time.After(5 * time.Second):
						t.Fatal("the poller kept running after a fatal poll")
					}
					return
				}
			case <-deadline:
				t.Fatal("the poller never reported the stopped daemon")
			}
		}
	})
}

// pipelinesListFunc serves a canned pipeline listing.
type pipelinesListFunc struct{ items []api.PipelineListItem }

func (p *pipelinesListFunc) RunPipeline(context.Context, api.PipelineRunRequest) (api.PipelineRunResult, error) {
	return api.PipelineRunResult{}, api.ErrControlUnavailable
}

func (p *pipelinesListFunc) ListPipelines(context.Context, bool) (api.PipelineListResult, error) {
	return api.PipelineListResult{Pipelines: p.items}, nil
}
