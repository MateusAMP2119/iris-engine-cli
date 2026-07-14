package dispatch_test

import (
	"context"
	"testing"

	"github.com/MateusAMP2119/iris-engine-cli/internal/dispatch"
	"github.com/MateusAMP2119/iris-engine-cli/internal/store"
)

// fakeEdgeReader resolves a pipeline's depends_on edges from a seeded map, so a manual
// run's gate can be driven with no live meta.
type fakeEdgeReader struct{ edges map[string][]dispatch.Edge }

func (f fakeEdgeReader) Edges(_ context.Context, pipeline string) ([]dispatch.Edge, error) {
	return f.edges[pipeline], nil
}

// fakeLaneReader returns a seeded set of persisted lane rows, so lane membership routing
// is provable with no live meta.
type fakeLaneReader struct{ rows []dispatch.LaneRow }

func (f fakeLaneReader) LaneRows(_ context.Context) ([]dispatch.LaneRow, error) {
	return f.rows, nil
}

// recordingQueue records the manual runs enqueued as a lane's next run, so a test proves
// a lane member's run is queued (never started out of band).
type recordingQueue struct {
	lanes   []string
	records []store.RunRecord
}

func (q *recordingQueue) Enqueue(_ context.Context, lane string, rec store.RunRecord) error {
	q.lanes = append(q.lanes, lane)
	q.records = append(q.records, rec)
	return nil
}

// recordingImmediate records the own-lane manual runs started immediately and returns a
// preset terminal outcome, so a test proves an own-lane run runs at once.
type recordingImmediate struct {
	records []store.RunRecord
	outcome dispatch.RunOutcome
}

func (r *recordingImmediate) RunNow(_ context.Context, rec store.RunRecord) (dispatch.RunOutcome, error) {
	r.records = append(r.records, rec)
	return r.outcome, nil
}

// TestManualRunRoutesByLaneMembership proves the lane-member routing contract: a manual
// run on a lane-member pipeline is QUEUED as that lane's next run at the current run
// boundary (same-lane serialization preserved -- the lane runner starts it in turn, the
// manual path never starts it out of band), while an own-lane pipeline (its own
// anonymous lane, no same-lane member to serialize against) runs immediately.
func TestManualRunRoutesByLaneMembership(t *testing.T) {
	t.Run("lane-member-manual-run-queued", func(t *testing.T) {
		ctx := context.Background()

		t.Run("lane member is queued as the lane's next run, not run immediately", func(t *testing.T) {
			// "b" is a member of a composed lane "ingest" (two members, so lanes rows
			// exist): a manual run of it must be queued behind same-lane serialization,
			// never started out of band.
			edges := fakeEdgeReader{} // "b" is ungated: the gate opens on composer order.
			lanes := fakeLaneReader{rows: []dispatch.LaneRow{
				{Lane: "ingest", Pipeline: "a", Pos: 0},
				{Lane: "ingest", Pipeline: "b", Pos: 1},
			}}
			queue := &recordingQueue{}
			immediate := &recordingImmediate{outcome: dispatch.RunSucceeded}
			runner := dispatch.NewManualRunner(dispatch.NewGate(newFakeConsumed()), edges, lanes, queue, immediate)

			state, reason, err := runner.Run(ctx, "b")
			if err != nil {
				t.Fatalf("Run: %v", err)
			}
			if state != dispatch.ManualRunQueued {
				t.Fatalf("state = %v (reason %q), want ManualRunQueued", state, reason)
			}
			if len(immediate.records) != 0 {
				t.Errorf("lane member was run immediately (%d immediate starts); same-lane serialization broken", len(immediate.records))
			}
			if len(queue.lanes) != 1 || queue.lanes[0] != "ingest" {
				t.Fatalf("enqueued lanes = %v, want [ingest]", queue.lanes)
			}
			if queue.records[0].Cause != store.CauseManual {
				t.Errorf("queued run cause = %q, want manual", queue.records[0].Cause)
			}
			if queue.records[0].Pipeline != "b" {
				t.Errorf("queued run pipeline = %q, want b", queue.records[0].Pipeline)
			}
		})

		t.Run("own-lane pipeline runs immediately, never queued", func(t *testing.T) {
			// "solo" is named by no lane row: it is its own anonymous lane, so a manual
			// run of it starts at once (no same-lane member to serialize against).
			lanes := fakeLaneReader{rows: []dispatch.LaneRow{
				{Lane: "ingest", Pipeline: "a", Pos: 0},
				{Lane: "ingest", Pipeline: "b", Pos: 1},
			}}
			queue := &recordingQueue{}
			immediate := &recordingImmediate{outcome: dispatch.RunSucceeded}
			runner := dispatch.NewManualRunner(dispatch.NewGate(newFakeConsumed()), fakeEdgeReader{}, lanes, queue, immediate)

			state, reason, err := runner.Run(ctx, "solo")
			if err != nil {
				t.Fatalf("Run: %v", err)
			}
			if state != dispatch.ManualRunSucceeded {
				t.Fatalf("state = %v (reason %q), want ManualRunSucceeded", state, reason)
			}
			if len(queue.lanes) != 0 {
				t.Errorf("own-lane pipeline was queued (%v); it must run immediately", queue.lanes)
			}
			if len(immediate.records) != 1 {
				t.Fatalf("own-lane immediate starts = %d, want 1", len(immediate.records))
			}
			if immediate.records[0].Cause != store.CauseManual || immediate.records[0].Pipeline != "solo" {
				t.Errorf("immediate run = %+v, want cause=manual pipeline=solo", immediate.records[0])
			}
		})

		t.Run("own-lane run that dead-letters is reported dead-lettered", func(t *testing.T) {
			// An own-lane manual run whose script fails dead-letters (the CLI exits 5).
			queue := &recordingQueue{}
			immediate := &recordingImmediate{outcome: dispatch.RunDeadLettered}
			runner := dispatch.NewManualRunner(dispatch.NewGate(newFakeConsumed()), fakeEdgeReader{}, fakeLaneReader{}, queue, immediate)

			state, _, err := runner.Run(ctx, "solo")
			if err != nil {
				t.Fatalf("Run: %v", err)
			}
			if state != dispatch.ManualRunDeadLettered {
				t.Fatalf("state = %v, want ManualRunDeadLettered", state)
			}
		})

		t.Run("ineligible pipeline neither queues nor runs, and carries a reason", func(t *testing.T) {
			// "b" depends on an upstream with no success yet: the gate does not open, so
			// the manual run is ineligible -- nothing queued, nothing started, exit 4.
			edges := fakeEdgeReader{edges: map[string][]dispatch.Edge{
				"b": {{Upstream: "a", Latest: dispatch.UpstreamPending, LatestRunID: 3}},
			}}
			queue := &recordingQueue{}
			immediate := &recordingImmediate{outcome: dispatch.RunSucceeded}
			runner := dispatch.NewManualRunner(dispatch.NewGate(newFakeConsumed()), edges, fakeLaneReader{}, queue, immediate)

			state, reason, err := runner.Run(ctx, "b")
			if err != nil {
				t.Fatalf("Run: %v", err)
			}
			if state != dispatch.ManualRunIneligible {
				t.Fatalf("state = %v, want ManualRunIneligible", state)
			}
			if reason == "" {
				t.Error("ineligible manual run carried no reason")
			}
			if len(queue.lanes) != 0 || len(immediate.records) != 0 {
				t.Errorf("ineligible run touched queue (%v) or immediate (%d); it must do neither", queue.lanes, len(immediate.records))
			}
		})
	})
}
