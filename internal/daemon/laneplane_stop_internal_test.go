package daemon

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/MateusAMP2119/iris-lakehouse/internal/dispatch"
	"github.com/MateusAMP2119/iris-lakehouse/internal/exec"
	"github.com/MateusAMP2119/iris-lakehouse/internal/store"
	"github.com/MateusAMP2119/iris-lakehouse/internal/store/storetest"
)

// stopHandle is a minimal exec.Handle whose kill is observable.
type stopHandle struct{ killed bool }

func (h *stopHandle) PGID() int                     { return 4242 }
func (h *stopHandle) Wait() (exec.ExitStatus, error) { return exec.ExitStatus{}, nil }
func (h *stopHandle) Kill() error                   { h.killed = true; return nil }

// TestLanePlaneCancelPipeline proves the pipeline-level park (#202): the latest run resolves and dead-letters inside one submission (running kills the group, queued parks without a kill), an already-parked pipeline succeeds idempotently, and nothing stoppable reports not in flight.
func TestLanePlaneCancelPipeline(t *testing.T) {
	t.Run("lane-plane-cancel-pipeline", func(t *testing.T) {
		build := func(latest gateManualFake) (*lanePlane, *storetest.WriteRecorder, *inflightRuns) {
			rec := &storetest.WriteRecorder{}
			inflight := newInflightRuns()
			p := newLanePlane(nil, inflight, latest)
			p.install(gateSubmitter{rec: rec})
			return p, rec, inflight
		}

		t.Run("running latest is killed and dead-lettered as stopped", func(t *testing.T) {
			p, rec, inflight := build(gateManualFake{"demo": {ID: 7, State: store.RunRunning}})
			h := &stopHandle{}
			inflight.track("7", h)
			run, err := p.CancelPipeline(context.Background(), "demo")
			if err != nil || run != "7" {
				t.Fatalf("CancelPipeline = (%q, %v), want (7, nil)", run, err)
			}
			if !h.killed {
				t.Error("running run's process group was not killed")
			}
			stmts := rec.Statements()
			if len(stmts) != 1 || !strings.Contains(stmts[0].SQL, "UPDATE runs SET state") {
				t.Fatalf("writes = %+v, want the one dead-letter CTE", stmts)
			}
			if got := stmts[0].Args; got[2] != store.RunRunning || got[3] != store.ReasonStopped || got[4] != runCancelDetail {
				t.Errorf("dead-letter args = %v, want running guard, stopped reason, cancel detail", got)
			}
		})

		t.Run("queued latest parks without a kill", func(t *testing.T) {
			p, rec, _ := build(gateManualFake{"demo": {ID: 9, State: store.RunQueued}})
			run, err := p.CancelPipeline(context.Background(), "demo")
			if err != nil || run != "9" {
				t.Fatalf("CancelPipeline = (%q, %v), want (9, nil)", run, err)
			}
			stmts := rec.Statements()
			if len(stmts) != 1 || stmts[0].Args[2] != store.RunQueued {
				t.Fatalf("writes = %+v, want the queued-guard dead-letter", stmts)
			}
		})

		t.Run("already parked is idempotent success with no writes", func(t *testing.T) {
			p, rec, _ := build(gateManualFake{"demo": {ID: 5, State: store.RunDeadLettered,
				DeadLetterReason: store.ReasonStopped, DeadLetterDetail: runCancelDetail}})
			run, err := p.CancelPipeline(context.Background(), "demo")
			if err != nil || run != "5" {
				t.Fatalf("CancelPipeline = (%q, %v), want (5, nil)", run, err)
			}
			if stmts := rec.Statements(); len(stmts) != 0 {
				t.Errorf("idempotent park wrote %+v, want nothing", stmts)
			}
		})

		t.Run("terminal latest and unknown pipeline report not in flight", func(t *testing.T) {
			p, _, _ := build(gateManualFake{"demo": {ID: 3, State: store.RunSucceeded}})
			if _, err := p.CancelPipeline(context.Background(), "demo"); !errors.Is(err, dispatch.ErrRunNotInFlight) {
				t.Errorf("succeeded latest: err = %v, want ErrRunNotInFlight", err)
			}
			if _, err := p.CancelPipeline(context.Background(), "ghost"); !errors.Is(err, dispatch.ErrRunNotInFlight) {
				t.Errorf("unknown pipeline: err = %v, want ErrRunNotInFlight", err)
			}
		})

		t.Run("no leader submitter reports not in flight", func(t *testing.T) {
			p := newLanePlane(nil, newInflightRuns(), gateManualFake{})
			if _, err := p.CancelPipeline(context.Background(), "demo"); !errors.Is(err, dispatch.ErrRunNotInFlight) {
				t.Errorf("no submitter: err = %v, want ErrRunNotInFlight", err)
			}
		})
	})
}
