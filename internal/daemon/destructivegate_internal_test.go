package daemon

import (
	"context"
	"strings"
	"testing"

	"github.com/MateusAMP2119/iris-lakehouse/internal/api"
	"github.com/MateusAMP2119/iris-lakehouse/internal/dispatch"
	"github.com/MateusAMP2119/iris-lakehouse/internal/pg"
	"github.com/MateusAMP2119/iris-lakehouse/internal/store"
	"github.com/MateusAMP2119/iris-lakehouse/internal/store/storetest"
)

// This file proves the daemon-side destructive-op gate bites: a confirmed (--yes)
// op refuses with guidance while a soft-block holds, --force overrides and
// cancels exactly the in-flight runs it overrode, the destroy path's hard
// blockers fire over live meta snapshots, and the wipe and drain planes consult
// the same gate.

// gateSubmitter hands each mutation the single writer over the recorder.
type gateSubmitter struct{ rec *storetest.WriteRecorder }

func (s gateSubmitter) Submit(_ context.Context, fn func(*store.Writer) error) error {
	return fn(store.NewWriter(s.rec))
}

// seedRun creates one run in the fake meta and drives it to state.
func seedRun(t *testing.T, meta *storetest.Fake, pipeline string, state store.RunState) store.Run {
	t.Helper()
	run, err := meta.CreateRun(context.Background(), store.RunSpec{Pipeline: pipeline, Lane: "l"})
	if err != nil {
		t.Fatalf("CreateRun(%s): %v", pipeline, err)
	}
	if state != store.RunQueued {
		if _, err := meta.SetRunState(context.Background(), run.ID, state); err != nil {
			t.Fatalf("SetRunState(%s -> %s): %v", run.ID, state, err)
		}
	}
	return run
}

// countStatements returns how many recorded statements contain marker.
func countStatements(rec *storetest.WriteRecorder, marker string) int {
	n := 0
	for _, s := range rec.Statements() {
		if strings.Contains(s.SQL, marker) {
			n++
		}
	}
	return n
}

func TestDestructiveGateEnforce(t *testing.T) {
	t.Run("destructive-gate-enforce", func(t *testing.T) {
		ctx := context.Background()

		t.Run("--yes refuses on an in-flight run, naming it and the remedy", func(t *testing.T) {
			meta := storetest.New()
			running := seedRun(t, meta, "p", store.RunRunning)
			rec := storetest.NewWriteRecorder()
			g := destructiveGate{reader: meta, submit: gateSubmitter{rec: rec}}

			err := g.enforce(ctx, dispatch.OpWorkloadWipe, dispatch.GateScope{Pipeline: "p"}, false, 0)
			if err == nil {
				t.Fatal("--yes proceeded past an in-flight run; the soft-block must refuse")
			}
			if !strings.Contains(err.Error(), running.ID) || !strings.Contains(err.Error(), "--force") {
				t.Errorf("refusal %q does not name the run and the --force remedy", err)
			}
			if got := len(rec.Statements()); got != 0 {
				t.Errorf("a refusal wrote %d statements; a refused op must write nothing", got)
			}
		})

		t.Run("--force proceeds and cancels exactly the overridden runs", func(t *testing.T) {
			meta := storetest.New()
			seedRun(t, meta, "p", store.RunRunning)
			seedRun(t, meta, "p", store.RunQueued)
			seedRun(t, meta, "other", store.RunRunning) // out of scope: never cancelled
			rec := storetest.NewWriteRecorder()
			g := destructiveGate{reader: meta, submit: gateSubmitter{rec: rec}}

			if err := g.enforce(ctx, dispatch.OpWorkloadWipe, dispatch.GateScope{Pipeline: "p"}, true, 0); err != nil {
				t.Fatalf("--force enforce: %v", err)
			}
			// Each overridden run gets the guarded pair: the dead-letter (bites on a
			// running run) and the queued delete (bites on a queued one).
			if got := countStatements(rec, "INSERT INTO dead_letters"); got != 2 {
				t.Errorf("recorded %d dead-letter statements, want 2 (one per overridden run)", got)
			}
			if got := countStatements(rec, "DELETE FROM runs"); got != 2 {
				t.Errorf("recorded %d queued-delete statements, want 2 (one per overridden run)", got)
			}
		})

		t.Run("a clean scope proceeds under --yes with no writes", func(t *testing.T) {
			meta := storetest.New()
			seedRun(t, meta, "p", store.RunSucceeded) // terminal: never blocks
			rec := storetest.NewWriteRecorder()
			g := destructiveGate{reader: meta, submit: gateSubmitter{rec: rec}}

			if err := g.enforce(ctx, dispatch.OpWorkloadWipe, dispatch.GateScope{Pipeline: "p"}, false, 0); err != nil {
				t.Fatalf("clean-scope enforce: %v", err)
			}
			if got := len(rec.Statements()); got != 0 {
				t.Errorf("a clean gate wrote %d statements, want 0", got)
			}
		})

		t.Run("un-promoted data soft-blocks a teardown under --yes, --force discards", func(t *testing.T) {
			meta := storetest.New()
			rec := storetest.NewWriteRecorder()
			g := destructiveGate{reader: meta, submit: gateSubmitter{rec: rec}}

			err := g.enforce(ctx, dispatch.OpDeclareDestroy, dispatch.GateScope{Pipeline: "p"}, false, 3)
			if err == nil {
				t.Fatal("--yes teardown proceeded past un-promoted data; the soft-block must refuse")
			}
			if !strings.Contains(err.Error(), "3 un-promoted") {
				t.Errorf("refusal %q does not count the un-promoted entries", err)
			}
			if err := g.enforce(ctx, dispatch.OpDeclareDestroy, dispatch.GateScope{Pipeline: "p"}, true, 3); err != nil {
				t.Fatalf("--force teardown over un-promoted data: %v", err)
			}
		})

		t.Run("an unwired gate is a no-op", func(t *testing.T) {
			if err := (destructiveGate{}).enforce(ctx, dispatch.OpWorkloadWipe, dispatch.GateScope{}, false, 9); err != nil {
				t.Fatalf("unwired gate: %v", err)
			}
		})
	})
}

// gateDLReader is a store.DeadLetterReader over fixed worklist rows.
type gateDLReader struct {
	rows []store.DeadLetterWorklistEntry
}

func (r gateDLReader) Worklist(context.Context) ([]store.DeadLetterWorklistEntry, error) {
	return r.rows, nil
}
func (r gateDLReader) Consumptions(context.Context) ([]store.ConsumptionFact, error) {
	return nil, nil
}
func (r gateDLReader) LaneMembers(context.Context) ([]store.LaneMember, error) { return nil, nil }

func TestDestroyBlockerOverLiveSnapshots(t *testing.T) {
	t.Run("destroy-blocker-live-snapshots", func(t *testing.T) {
		ctx := context.Background()

		build := func(meta *storetest.Fake, reg *storetest.RegistryFake, dl store.DeadLetterReader) dispatch.DestroyBlockerFunc {
			c := &Candidate{
				registry:    reg,
				reader:      meta,
				deadletters: newDeadletterPlane(dl, reg, nil),
			}
			return c.destroyBlocker()
		}

		t.Run("a dependent's depends_on blocks", func(t *testing.T) {
			reg := storetest.NewRegistryFake()
			reg.Register("upstream").Register("dependent", "upstream")
			blocker := build(storetest.New(), reg, gateDLReader{})

			blocked, reason, err := blocker(ctx, "upstream")
			if err != nil {
				t.Fatalf("blocker: %v", err)
			}
			if !blocked || !strings.Contains(reason, "dependent") {
				t.Errorf("blocked=%v reason=%q; a declared depends_on must block the upstream's destroy", blocked, reason)
			}
		})

		t.Run("a downstream run_inputs row blocks, even for a pruned target run", func(t *testing.T) {
			meta := storetest.New()
			var lineage store.ProvenanceLineage
			// The target's run 1 exists only as an archival summary (pruned); the
			// downstream's live run 2 still names it in run_inputs.
			lineage.Summaries = append(lineage.Summaries, struct {
				RunID                  int64
				Pipeline               string
				State                  string
				ArtifactHash           *string
				DeclarationChecksum    string
				ConsumedUpstreamRunIDs []int64
				SnapshotLSN            *string
				JournalFloor           *int64
				JournalCeiling         *int64
			}{RunID: 1, Pipeline: "target", State: "succeeded"})
			lineage.Runs = append(lineage.Runs, struct {
				RunID               int64
				Pipeline            string
				State               string
				ArtifactHash        *string
				DeclarationChecksum string
				SnapshotLSN         *string
				JournalFloor        *int64
				JournalCeiling      *int64
			}{RunID: 2, Pipeline: "downstream", State: "succeeded"})
			lineage.Inputs = append(lineage.Inputs, struct {
				RunID         int64
				UpstreamRunID int64
			}{RunID: 2, UpstreamRunID: 1})
			meta.SetProvenanceLineage(lineage)

			blocker := build(meta, storetest.NewRegistryFake(), gateDLReader{})
			blocked, reason, err := blocker(ctx, "target")
			if err != nil {
				t.Fatalf("blocker: %v", err)
			}
			if !blocked || !strings.Contains(reason, "downstream") {
				t.Errorf("blocked=%v reason=%q; a downstream consumption row must block", blocked, reason)
			}
		})

		t.Run("an outstanding dead-letter naming the target as failed_upstream blocks", func(t *testing.T) {
			meta := storetest.New()
			var lineage store.ProvenanceLineage
			lineage.Runs = append(lineage.Runs, struct {
				RunID               int64
				Pipeline            string
				State               string
				ArtifactHash        *string
				DeclarationChecksum string
				SnapshotLSN         *string
				JournalFloor        *int64
				JournalCeiling      *int64
			}{RunID: 7, Pipeline: "target", State: "dead_lettered"})
			meta.SetProvenanceLineage(lineage)
			dl := gateDLReader{rows: []store.DeadLetterWorklistEntry{{
				RunID: 9, Pipeline: "victim", Reason: store.ReasonUpstreamDeadLettered, FailedUpstreamRunID: 7,
			}}}

			blocker := build(meta, storetest.NewRegistryFake(), dl)
			blocked, reason, err := blocker(ctx, "target")
			if err != nil {
				t.Fatalf("blocker: %v", err)
			}
			if !blocked || !strings.Contains(reason, "failed_upstream") {
				t.Errorf("blocked=%v reason=%q; an outstanding failed_upstream entry must block", blocked, reason)
			}
		})

		t.Run("a clean target destroys", func(t *testing.T) {
			blocker := build(storetest.New(), storetest.NewRegistryFake(), gateDLReader{})
			blocked, reason, err := blocker(ctx, "lonely")
			if err != nil {
				t.Fatalf("blocker: %v", err)
			}
			if blocked {
				t.Errorf("a target with no dependents, consumers, or worklist references was blocked: %q", reason)
			}
		})
	})
}

// wipeDataSpy is a dataPlane recording ExecuteWipe calls for the wipe-gate test.
type wipeDataSpy struct {
	controlDataFake
	wipes int
}

func (f *wipeDataSpy) ExecuteWipe(context.Context, pg.WipeTarget) (pg.WipeResult, error) {
	f.wipes++
	return pg.WipeResult{Wiped: 1}, nil
}

func TestWipeAndDrainConsultTheGate(t *testing.T) {
	t.Run("wipe-and-drain-consult-the-gate", func(t *testing.T) {
		ctx := context.Background()

		t.Run("wipe --yes refuses on an in-flight run; --force cancels and wipes", func(t *testing.T) {
			meta := storetest.New()
			seedRun(t, meta, "p", store.RunRunning)
			rec := storetest.NewWriteRecorder()
			data := &wipeDataSpy{}
			wo := newWipeOrchestrator(gateSubmitter{rec: rec}, meta, data, nil, nil)

			_, err := wo.wipe(ctx, api.WorkloadWipeRequest{Pipeline: "p", Confirm: true})
			if err == nil || !strings.Contains(err.Error(), "in flight") {
				t.Fatalf("wipe --yes over an in-flight run returned %v, want a soft-block refusal", err)
			}
			if data.wipes != 0 {
				t.Fatal("a refused wipe still executed on the data database")
			}

			if _, err := wo.wipe(ctx, api.WorkloadWipeRequest{Pipeline: "p", Confirm: true, Force: true}); err != nil {
				t.Fatalf("wipe --force: %v", err)
			}
			if data.wipes != 1 {
				t.Errorf("wipe --force executed %d wipes, want 1", data.wipes)
			}
			if got := countStatements(rec, "INSERT INTO dead_letters"); got != 1 {
				t.Errorf("wipe --force cancelled %d runs, want 1 (dead-lettered stopped)", got)
			}
		})

		t.Run("drain --yes refuses on an in-flight run in scope; --force proceeds", func(t *testing.T) {
			meta := storetest.New()
			seedRun(t, meta, "p", store.RunRunning)
			rec := storetest.NewWriteRecorder()
			dl := gateDLReader{rows: []store.DeadLetterWorklistEntry{{
				RunID: 3, Pipeline: "p", Reason: store.ReasonFailed,
			}}}
			plane := newDeadletterPlane(dl, storetest.NewRegistryFake(), nil)
			plane.install(&deadletterExec{
				submit: gateSubmitter{rec: rec},
				gate:   destructiveGate{reader: meta, submit: gateSubmitter{rec: rec}},
			})

			_, err := plane.Drain(ctx, api.DrainRequest{Pipeline: "p"})
			if err == nil || !strings.Contains(err.Error(), "in flight") {
				t.Fatalf("drain --yes over an in-flight run returned %v, want a soft-block refusal", err)
			}
			if got := countStatements(rec, "DELETE FROM dead_letters"); got != 0 {
				t.Errorf("a refused drain deleted %d worklist rows, want 0", got)
			}

			res, err := plane.Drain(ctx, api.DrainRequest{Pipeline: "p", Force: true})
			if err != nil {
				t.Fatalf("drain --force: %v", err)
			}
			if len(res.Drained) != 1 || res.Drained[0] != "3" {
				t.Errorf("drain --force drained %v, want [3]", res.Drained)
			}
		})
	})
}
