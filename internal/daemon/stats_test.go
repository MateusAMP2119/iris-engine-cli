package daemon_test

import (
	"context"
	"testing"

	"github.com/MateusAMP2119/iris-engine-cli/internal/api"
	"github.com/MateusAMP2119/iris-engine-cli/internal/daemon"
	"github.com/MateusAMP2119/iris-engine-cli/internal/dispatch"
	"github.com/MateusAMP2119/iris-engine-cli/internal/store"
	"github.com/MateusAMP2119/iris-engine-cli/internal/store/storetest"
)

// statsWalkFake is a fixed WalkReader for the pass-counter term tests: the loop
// reads this walk at each pass start.
type statsWalkFake struct{ lanes []dispatch.Lane }

func (w statsWalkFake) Walk(context.Context) ([]dispatch.Lane, error) {
	return append([]dispatch.Lane(nil), w.lanes...), nil
}

// statsGateFake is an ungated PassGate: every member runs every pass.
type statsGateFake struct{}

func (statsGateFake) Eligible(context.Context, string) (dispatch.Decision, error) {
	return dispatch.Decision{Run: true}, nil
}

// statsRunnerFake completes every fresh run at once, so lane passes chain fast.
type statsRunnerFake struct{}

func (statsRunnerFake) StartFresh(context.Context, store.RunRecord) (dispatch.RunOutcome, error) {
	return dispatch.RunSucceeded, nil
}

// TestLanePassCounterLeaderTerm proves the per-lane loop pass counter is a
// leader-held runtime counter (specification section 11): during a leadership
// term the loop's pass hook drives it up (a count of completed passes, no clock),
// and a new term -- the same daemon re-winning after a leader change -- starts
// from zero because the candidate resets the counter when it wins the lock.
// Restart-reset needs no wiring: the counter is process memory, so a restarted
// daemon constructs a fresh, empty one (proven in internal/dispatch).
func TestLanePassCounterLeaderTerm(t *testing.T) {
	t.Run("S11/lane-pass-counter-reset", func(t *testing.T) {
		pc := dispatch.NewPassCounter()
		role := api.NewRoleState()
		set := storetest.NewLockSet()

		// Term one: the leader drives the lane loop; each completed pass increments
		// the lane's count through the loop's pass hook.
		build := func(dispatch.Submitter) *dispatch.Loop {
			return dispatch.NewLoop(
				statsWalkFake{lanes: []dispatch.Lane{{Name: "etl", Pipelines: []string{"a"}}}},
				statsGateFake{},
				statsRunnerFake{},
				nil,
				dispatch.WithOnPass(pc.Hook()),
			)
		}
		ctx1, cancel1 := context.WithCancel(context.Background())
		done1 := make(chan struct{})
		cand1 := daemon.NewCandidate(set.New(), role, &leaderWriteConn{}, nil,
			daemon.WithLaneLoop(build), daemon.WithPassCounter(pc))
		go func() { defer close(done1); _ = cand1.Serve(ctx1) }()

		if !pollUntil(func() bool { return pc.Counts()["etl"] >= 2 }) {
			cancel1()
			<-done1
			t.Fatalf("pass counter did not climb during the leadership term: counts = %v", pc.Counts())
		}
		// End the term (leadership lost / daemon demoted).
		cancel1()
		<-done1
		if pc.Counts()["etl"] == 0 {
			t.Fatal("counter lost its term counts before any new term began; nothing should zero it mid-term")
		}

		// Term two: leadership is re-won (a leader change back to this candidate).
		// The new term's walk has no lanes, so nothing increments -- any residue
		// visible after the leader role is reported would be the previous term's,
		// and the reset-on-leader-change contract forbids exactly that.
		role2 := api.NewRoleState()
		buildIdle := func(dispatch.Submitter) *dispatch.Loop {
			return dispatch.NewLoop(statsWalkFake{}, statsGateFake{}, statsRunnerFake{}, nil,
				dispatch.WithOnPass(pc.Hook()))
		}
		ctx2, cancel2 := context.WithCancel(context.Background())
		done2 := make(chan struct{})
		cand2 := daemon.NewCandidate(set.New(), role2, &leaderWriteConn{}, nil,
			daemon.WithLaneLoop(buildIdle), daemon.WithPassCounter(pc))
		go func() { defer close(done2); _ = cand2.Serve(ctx2) }()
		defer func() { cancel2(); <-done2 }()

		if !pollUntil(func() bool { return role2.Role() == api.RoleLeader }) {
			t.Fatal("second term never won leadership")
		}
		if got := pc.Counts(); len(got) != 0 {
			t.Fatalf("counts after a leader change = %v, want empty (the counter resets on leader change)", got)
		}
	})
}

// TestStatsPlane proves the daemon's stats handler serves the store rollup as the
// one read-only api payload (specification sections 11 and 14): the engine,
// lane, and pipeline rollups map field-for-field, the pass counts come from the
// leader-held counter, and an absent checkpoint chain stays an explicit null
// field, never a dropped one.
//
// spec: S11/stats-engine-rollup
// spec: S11/stats-lane-rollup
// spec: S11/stats-pipeline-rollup
func TestStatsPlane(t *testing.T) {
	f := storetest.NewStats()
	f.RegisterPipeline("extract")
	f.AddLaneMember("ingest", "extract")
	run, err := f.CreateRun(context.Background(), store.RunSpec{Pipeline: "extract", Lane: "ingest"})
	if err != nil {
		t.Fatalf("create run: %v", err)
	}
	if _, err := f.SetRunState(context.Background(), run.ID, store.RunSucceeded, store.WithExitCode(0)); err != nil {
		t.Fatalf("set run state: %v", err)
	}
	f.AddDeadLetter(store.DeadLetterEntry{RunID: "run-9", Reason: store.ReasonFailed})
	f.SetJournal(store.JournalStats{CapturedWrites: 10, WipeEligibleRows: 4, TotalRows: 20, HotRows: 15})
	f.AddCheckpoint(store.Checkpoint{Seq: 7, Digest: "beef", Location: "resident"})

	pc := dispatch.NewPassCounter()
	pc.Hook()(dispatch.PassReport{Lane: "ingest"})

	payload, err := daemon.NewStatsPlane(f, pc, nil).Stats(context.Background())
	if err != nil {
		t.Fatalf("stats plane: %v", err)
	}

	e := payload.Engine
	if e.DeadLetterDepth != 1 || e.DeadLettersByReason["failed"] != 1 {
		t.Errorf("engine dead-letter rollup = depth %d by-reason %v, want 1 / failed:1", e.DeadLetterDepth, e.DeadLettersByReason)
	}
	if e.CapturedWrites != 10 || e.WipeEligibleRows != 4 || e.JournalRows != 20 || e.HotRows != 15 {
		t.Errorf("engine journal rollup = %+v, want captured 10, wipe-eligible 4, rows 20, hot 15", e)
	}
	if e.SealedPartitions != 1 || e.ArchivedPartitions != 0 {
		t.Errorf("partition counts = sealed %d archived %d, want 1/0", e.SealedPartitions, e.ArchivedPartitions)
	}
	if e.CheckpointChainHead == nil || e.CheckpointChainHead.Seq != 7 || e.CheckpointChainHead.Digest != "beef" {
		t.Errorf("chain head = %+v, want seq 7 digest beef", e.CheckpointChainHead)
	}
	if len(payload.Lanes) != 1 || payload.Lanes[0].Lane != "ingest" ||
		payload.Lanes[0].Pipelines != 1 || payload.Lanes[0].Passes != 1 {
		t.Errorf("lane rollup = %+v, want one ingest lane, 1 pipeline, 1 pass", payload.Lanes)
	}
	if len(payload.Pipelines) != 1 {
		t.Fatalf("pipeline rollups = %+v, want exactly extract", payload.Pipelines)
	}
	p := payload.Pipelines[0]
	if p.Pipeline != "extract" || p.LatestRunState != "succeeded" || p.LastRunID != run.ID ||
		p.LastExitCode == nil || *p.LastExitCode != 0 || p.RunsByState["succeeded"] != 1 {
		t.Errorf("pipeline rollup = %+v, want extract succeeded run %s exit 0", p, run.ID)
	}

	// An empty engine keeps the chain-head field explicit and null (E07.4's chain
	// is not built yet; the payload field is present per the spec regardless).
	empty, err := daemon.NewStatsPlane(storetest.NewStats(), nil, nil).Stats(context.Background())
	if err != nil {
		t.Fatalf("stats plane over empty state: %v", err)
	}
	if empty.Engine.CheckpointChainHead != nil {
		t.Errorf("empty chain head = %+v, want nil", empty.Engine.CheckpointChainHead)
	}
	if empty.Engine.DeadLettersByReason == nil {
		t.Error("empty dead-letters-by-reason map is nil, want empty non-nil (a count readout, never absent)")
	}
}
