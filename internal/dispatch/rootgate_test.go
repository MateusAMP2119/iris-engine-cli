package dispatch_test

import (
	"testing"

	"github.com/MateusAMP2119/iris-lakehouse/internal/dispatch"
	"github.com/MateusAMP2119/iris-lakehouse/internal/store"
)

// TestDecideRoot proves the root cause gate is a pure function of the latest run
// and the current declaration checksum: a root runs only on an unconsumed cause
// (no run at all, or a declaration change since its latest run), parks while a
// run is in flight, and never re-runs a terminal run's consumed declaration --
// dead-lettered exactly like succeeded, because a failed run is only ever
// re-executed by an explicit replay.
func TestDecideRoot(t *testing.T) {
	const sum = "aaaa"
	const changed = "bbbb"

	tests := []struct {
		name    string
		latest  *dispatch.RootRun
		current string
		wantRun bool
	}{
		{
			name:    "no run at all: the registration is the unconsumed cause",
			latest:  nil,
			current: sum,
			wantRun: true,
		},
		{
			name:    "latest queued: a run is already minted, wait for its terminal",
			latest:  &dispatch.RootRun{State: store.RunQueued, DeclarationChecksum: sum},
			current: sum,
			wantRun: false,
		},
		{
			name:    "latest running: in flight, wait for its terminal",
			latest:  &dispatch.RootRun{State: store.RunRunning, DeclarationChecksum: sum},
			current: sum,
			wantRun: false,
		},
		{
			name:    "latest running under an old declaration: still wait, its terminal re-opens the check",
			latest:  &dispatch.RootRun{State: store.RunRunning, DeclarationChecksum: changed},
			current: sum,
			wantRun: false,
		},
		{
			name:    "succeeded on the current declaration: consumed, park",
			latest:  &dispatch.RootRun{State: store.RunSucceeded, DeclarationChecksum: sum},
			current: sum,
			wantRun: false,
		},
		{
			name:    "succeeded on an old declaration: the change is the unconsumed cause",
			latest:  &dispatch.RootRun{State: store.RunSucceeded, DeclarationChecksum: changed},
			current: sum,
			wantRun: true,
		},
		{
			name:    "dead-lettered on the current declaration: park, never a retry (replay is the surface)",
			latest:  &dispatch.RootRun{State: store.RunDeadLettered, DeclarationChecksum: sum},
			current: sum,
			wantRun: false,
		},
		{
			name:    "dead-lettered on an old declaration: the change is a new cause, run",
			latest:  &dispatch.RootRun{State: store.RunDeadLettered, DeclarationChecksum: changed},
			current: sum,
			wantRun: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			d := dispatch.DecideRoot(tt.latest, tt.current)
			if d.Run != tt.wantRun {
				t.Fatalf("DecideRoot(%+v, %q).Run = %v, want %v", tt.latest, tt.current, d.Run, tt.wantRun)
			}
			if d.Poisoned {
				t.Fatalf("DecideRoot(%+v, %q).Poisoned = true, want false (a root has no edges to poison)", tt.latest, tt.current)
			}
			if tt.wantRun && len(d.Consume) != 0 {
				t.Fatalf("DecideRoot(%+v, %q).Consume = %v, want empty (a root consumes no upstream runs)", tt.latest, tt.current, d.Consume)
			}
		})
	}
}
