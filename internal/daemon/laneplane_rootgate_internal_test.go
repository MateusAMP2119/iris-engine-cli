package daemon

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/MateusAMP2119/iris-lakehouse/internal/store"
)

// fakeRootDetail is a store.RootGateReader over a fixed latest-run map: a
// pipeline absent from the map has no run at all.
type fakeRootDetail map[string]store.LatestRunDetail

func (f fakeRootDetail) LatestRunDetail(_ context.Context, pipeline string) (store.LatestRunDetail, bool, error) {
	d, ok := f[pipeline]
	return d, ok, nil
}

// fakeRootManual is a store.ManualReader whose only exercised read is the run
// target (the root gate reads the folder to checksum the declaration); the
// remaining methods return empty values.
type fakeRootManual map[string]store.PipelineRunTarget

func (f fakeRootManual) PipelineRunTarget(_ context.Context, name string) (store.PipelineRunTarget, bool, error) {
	t, ok := f[name]
	return t, ok, nil
}

func (f fakeRootManual) LatestRun(context.Context, string) (store.LatestRunInfo, bool, error) {
	return store.LatestRunInfo{}, false, nil
}

func (f fakeRootManual) Consumed(context.Context, string, int64) (bool, error) { return false, nil }

func (f fakeRootManual) LaneRows(context.Context) ([]store.LaneEntry, error) { return nil, nil }

// TestLanePassGateRootCause proves the daemon's pass gate resolves an edge-less
// pipeline through the root cause gate over real reads: the latest-run detail,
// the run target's folder, and the live declaration file's checksum. A root with
// no run yet runs; one whose latest terminal run stamped the current checksum
// parks; a declaration edit since that run is an unconsumed cause and re-opens
// the gate; an unregistered name mints nothing.
func TestLanePassGateRootCause(t *testing.T) {
	t.Run("lane-pass-gate-root-cause", func(t *testing.T) {
		ctx := context.Background()
		workspace := t.TempDir()
		if err := os.MkdirAll(filepath.Join(workspace, "pipelines", "root"), 0o750); err != nil {
			t.Fatal(err)
		}
		declPath := filepath.Join(workspace, "pipelines", "root", "iris-declare.yaml")
		if err := os.WriteFile(declPath, []byte("name: root\nrun: [true]\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		currentSum, err := declarationChecksum(workspace, filepath.Join("pipelines", "root"))
		if err != nil {
			t.Fatalf("declarationChecksum: %v", err)
		}

		targets := fakeRootManual{"root": {Folder: filepath.Join("pipelines", "root")}}

		tests := []struct {
			name    string
			details fakeRootDetail
			wantRun bool
		}{
			{
				name:    "no run at all: the registration is the unconsumed cause",
				details: fakeRootDetail{},
				wantRun: true,
			},
			{
				name: "latest succeeded on the current declaration: consumed, park",
				details: fakeRootDetail{"root": {
					ID: 7, State: store.RunSucceeded, Cause: store.CauseLoop, DeclarationChecksum: currentSum,
				}},
				wantRun: false,
			},
			{
				name: "latest ran an older declaration: the edit is the unconsumed cause",
				details: fakeRootDetail{"root": {
					ID: 7, State: store.RunSucceeded, Cause: store.CauseLoop, DeclarationChecksum: "stale",
				}},
				wantRun: true,
			},
			{
				name: "latest dead-lettered on the current declaration: park, replay is the retry surface",
				details: fakeRootDetail{"root": {
					ID: 7, State: store.RunDeadLettered, Cause: store.CauseLoop, DeclarationChecksum: currentSum,
				}},
				wantRun: false,
			},
			{
				name: "a run is in flight: wait for its terminal",
				details: fakeRootDetail{"root": {
					ID: 7, State: store.RunRunning, Cause: store.CauseManual, DeclarationChecksum: currentSum,
				}},
				wantRun: false,
			},
		}

		for _, tt := range tests {
			t.Run(tt.name, func(t *testing.T) {
				gate := lanePassGate{roots: tt.details, manual: targets, workspace: workspace}
				d, err := gate.eligibleRoot(ctx, "root")
				if err != nil {
					t.Fatalf("eligibleRoot returned %v, want nil", err)
				}
				if d.Run != tt.wantRun {
					t.Fatalf("eligibleRoot Run = %v, want %v", d.Run, tt.wantRun)
				}
			})
		}

		// An unregistered name mints nothing: a closed decision, no error (absence
		// is the record), so a pipeline unregistering mid-pass never faults the lane.
		gate := lanePassGate{roots: fakeRootDetail{}, manual: fakeRootManual{}, workspace: workspace}
		d, err := gate.eligibleRoot(ctx, "gone")
		if err != nil {
			t.Fatalf("eligibleRoot(gone) returned %v, want nil", err)
		}
		if d.Run {
			t.Fatalf("eligibleRoot(gone).Run = true, want false (an unregistered name mints nothing)")
		}
	})
}
