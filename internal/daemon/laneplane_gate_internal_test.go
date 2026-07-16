package daemon

import (
	"context"
	"testing"

	"github.com/MateusAMP2119/iris-lakehouse/internal/store"
)

// gateRegistryFake is a store.RegistryReader with no dependency edges: every
// pipeline resolves edge-less, exercising the for-loop path of the pass gate.
type gateRegistryFake struct{}

func (gateRegistryFake) RegisteredPipelines(context.Context) ([]string, error) { return nil, nil }
func (gateRegistryFake) DependencyEdges(context.Context) ([]store.DependencyEdge, error) {
	return nil, nil
}
func (gateRegistryFake) LaneMembers(context.Context, string) ([]string, error) { return nil, nil }

// gateManualFake is a store.ManualReader over a fixed latest-run map: a pipeline
// absent from the map has no run at all. Only LatestRun is exercised here.
type gateManualFake map[string]store.LatestRunInfo

func (f gateManualFake) LatestRun(_ context.Context, pipeline string) (store.LatestRunInfo, bool, error) {
	info, ok := f[pipeline]
	return info, ok, nil
}

func (gateManualFake) PipelineRunTarget(context.Context, string) (store.PipelineRunTarget, bool, error) {
	return store.PipelineRunTarget{}, false, nil
}
func (gateManualFake) Consumed(context.Context, string, int64) (bool, error) { return false, nil }
func (gateManualFake) LaneRows(context.Context) ([]store.LaneEntry, error)   { return nil, nil }

// TestLanePassGateForLoopWithNoRetryBrake proves the edge-less pass gate: a
// pipeline whose latest run succeeded (or that never ran) is always eligible --
// the perpetual for-loop -- while a dead-lettered latest run parks it (a failed
// run is never retried on its own; replay and manual run are the retry
// surfaces), and an in-flight latest run skips the turn.
func TestLanePassGateForLoopWithNoRetryBrake(t *testing.T) {
	t.Run("lane-pass-gate-for-loop-no-retry", func(t *testing.T) {
		manual := gateManualFake{
			"healthy": {ID: 7, State: store.RunSucceeded},
			"broken":  {ID: 8, State: store.RunDeadLettered, DeadLetterReason: store.ReasonFailed},
			"stopped":   {ID: 11, State: store.RunDeadLettered, DeadLetterReason: store.ReasonStopped, DeadLetterDetail: "run stopped: daemon terminated"},
			"cancelled": {ID: 13, State: store.RunDeadLettered, DeadLetterReason: store.ReasonStopped, DeadLetterDetail: runCancelDetail},
			"drained":   {ID: 12, State: store.RunDeadLettered}, // worklist row drained away: no outstanding reason
			"waiting": {ID: 9, State: store.RunRunning},
			"minted":  {ID: 10, State: store.RunQueued},
		}
		gate := lanePassGate{
			edges:  edgeReader{registry: gateRegistryFake{}, manual: manual},
			latest: manual,
		}

		tests := []struct {
			pipeline string
			wantRun  bool
		}{
			{"fresh", true},    // never ran: first loop run
			{"healthy", true},  // succeeded: run again immediately (for-loop)
			{"broken", false},  // outstanding failure: never retried on its own
			{"stopped", true},    // crash-reconciliation stop: always-alive resumes
			{"cancelled", false}, // operator cancel is a manual stop: not resurrected (#192)
			{"drained", true},    // drain released the brake: the loop tries anew
			{"waiting", false}, // running: already in flight
			{"minted", false},  // queued: already minted, wait for its terminal
		}
		for _, tt := range tests {
			t.Run(tt.pipeline, func(t *testing.T) {
				d, err := gate.Eligible(context.Background(), tt.pipeline)
				if err != nil {
					t.Fatalf("Eligible(%q) returned %v, want nil", tt.pipeline, err)
				}
				if d.Run != tt.wantRun {
					t.Fatalf("Eligible(%q).Run = %v, want %v", tt.pipeline, d.Run, tt.wantRun)
				}
			})
		}
	})
}
