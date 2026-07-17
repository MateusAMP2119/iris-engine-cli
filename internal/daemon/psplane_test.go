package daemon

import (
	"context"
	"errors"
	"testing"

	"github.com/MateusAMP2119/iris-lakehouse/internal/api"
	"github.com/MateusAMP2119/iris-lakehouse/internal/store"
)

// fakeRunReader is an in-memory RunSnapshotReader serving a fixed run snapshot
// (or a scripted error).
type fakeRunReader struct {
	runs []store.Run
	err  error
}

func (f fakeRunReader) Runs(context.Context, store.RunFilter) ([]store.Run, error) {
	return f.runs, f.err
}

// fakeProbe is a loadProber serving a fixed host sample (or a scripted error).
type fakeProbe struct {
	samples []procSample
	err     error
}

func (f fakeProbe) Sample(context.Context) ([]procSample, error) { return f.samples, f.err }

// leaderRoleState builds a RoleState flipped to leader.
func leaderRoleState() *api.RoleState {
	r := api.NewRoleState()
	r.SetLeader()
	return r
}

// intp returns a pointer to c.
func intp(c int) *int { return &c }

// psTestLoads builds a collector over the fakes with a pinned pid (100) and
// managed postmaster (pid 200) so the load summing is assertable, and drives
// exactly one sample tick.
func psTestLoads(runs RunSnapshotReader, probe loadProber) *loadHistory {
	h := newLoadHistory(runs, func() int { return 200 }, nil, nil)
	h.probe = probe
	h.pid = 100
	h.sample(context.Background())
	return h
}

// psTestPlane builds a plane over the fakes and a once-ticked collector.
func psTestPlane(runs RunSnapshotReader, probe loadProber, role api.RoleReporter) *psPlane {
	p := NewPsPlane(role, runs, psTestLoads(runs, probe), nil, nil).(*psPlane)
	p.pid = 100
	return p
}

// TestPsPlaneComposesReadout proves the plane's composition: run rows newest
// first with the default queued+running filter (whole history under all), run
// counts over the whole snapshot regardless of the filter, exit codes only on
// terminal rows, and the host sample summed by process group -- the daemon's
// own group as the engine load, each running run's handle group as its row's
// load.
func TestPsPlaneComposesReadout(t *testing.T) {
	runs := []store.Run{
		{ID: "1", Pipeline: "extract", Lane: "ingest", State: store.RunSucceeded, ExitCode: intp(0), Seq: 1},
		{ID: "2", Pipeline: "extract", Lane: "ingest", State: store.RunDeadLettered, ExitCode: intp(3), Seq: 2},
		{ID: "3", Pipeline: "load", Lane: "ingest", State: store.RunRunning, Handle: 300, ExitCode: intp(0), Seq: 3},
		{ID: "4", Pipeline: "load", Lane: "ingest", State: store.RunQueued, ExitCode: intp(0), Seq: 4},
	}
	samples := []procSample{
		{PID: 100, PGID: 100, PPID: 1, CPUPercent: 1.0, RSSBytes: 10 << 20},   // the daemon
		{PID: 200, PGID: 200, PPID: 1, CPUPercent: 0.5, RSSBytes: 20 << 20},   // the managed postmaster (daemonized: ppid 1)
		{PID: 201, PGID: 201, PPID: 200, CPUPercent: 0.5, RSSBytes: 10 << 20}, // a postmaster backend (own group, child of 200)
		{PID: 300, PGID: 300, PPID: 100, CPUPercent: 25, RSSBytes: 5 << 20},   // run 3's process group
		{PID: 301, PGID: 300, PPID: 300, CPUPercent: 25, RSSBytes: 5 << 20},
		{PID: 999, PGID: 999, PPID: 1, CPUPercent: 90, RSSBytes: 1 << 30}, // an unrelated process
	}

	t.Run("default readout: queued and running, newest first, group-summed load", func(t *testing.T) {
		p := psTestPlane(fakeRunReader{runs: runs}, fakeProbe{samples: samples}, leaderRoleState())
		got, err := p.Ps(context.Background(), false, false)
		if err != nil {
			t.Fatalf("Ps: %v", err)
		}
		if got.Engine.Role != "leader" || got.Engine.PID != 100 || got.Engine.Uptime == "" {
			t.Errorf("engine block = %+v, want role leader, pid 100, a rendered uptime", got.Engine)
		}
		if got.Engine.QueuedRuns != 1 || got.Engine.RunningRuns != 1 {
			t.Errorf("run counts = %d queued / %d running, want 1/1", got.Engine.QueuedRuns, got.Engine.RunningRuns)
		}
		// The engine load sums the daemon's descendant tree (daemon + the run
		// processes it spawned: what iris costs the host includes its in-flight
		// runs) plus the managed postmaster's tree (postmaster + backend, found by
		// parentage: Postgres backends sit in their own process groups), and
		// excludes the unrelated process: 10+5+5 + 20+10 = 50MiB.
		if got.Engine.Load == nil || got.Engine.Load.RSSBytes != 50<<20 || got.Engine.Load.CPUPercent != 52.0 {
			t.Errorf("engine load = %+v, want the daemon + postmaster trees summed (rss 50MiB, cpu 52.0)", got.Engine.Load)
		}
		if len(got.Runs) != 2 || got.Runs[0].ID != "4" || got.Runs[1].ID != "3" {
			t.Fatalf("rows = %+v, want runs 4 then 3 (newest first, queued+running only)", got.Runs)
		}
		if got.Runs[0].ExitCode != nil {
			t.Errorf("queued run carries exit code %d, want none", *got.Runs[0].ExitCode)
		}
		running := got.Runs[1]
		if running.Load == nil || running.Load.CPUPercent != 50 || running.Load.RSSBytes != 10<<20 {
			t.Errorf("running run load = %+v, want its process group summed (cpu 50, rss 10MiB)", running.Load)
		}
		if got.Runs[0].Load != nil {
			t.Errorf("queued run carries load %+v, want none (no live process group)", got.Runs[0].Load)
		}
	})

	t.Run("all widens to the whole history with terminal exit codes", func(t *testing.T) {
		p := psTestPlane(fakeRunReader{runs: runs}, fakeProbe{samples: samples}, leaderRoleState())
		got, err := p.Ps(context.Background(), true, false)
		if err != nil {
			t.Fatalf("Ps: %v", err)
		}
		if len(got.Runs) != 4 || got.Runs[0].ID != "4" || got.Runs[3].ID != "1" {
			t.Fatalf("rows = %+v, want all 4 runs newest first", got.Runs)
		}
		dead := got.Runs[2]
		if dead.ID != "2" || dead.ExitCode == nil || *dead.ExitCode != 3 {
			t.Errorf("dead-lettered row = %+v, want exit code 3", dead)
		}
		if ok := got.Runs[3].ExitCode; ok == nil || *ok != 0 {
			t.Errorf("succeeded row = %+v, want exit code 0", got.Runs[3])
		}
	})

	t.Run("a failed host probe yields null load, never zeros", func(t *testing.T) {
		p := psTestPlane(fakeRunReader{runs: runs}, fakeProbe{err: errors.New("no ps binary")}, leaderRoleState())
		got, err := p.Ps(context.Background(), false, false)
		if err != nil {
			t.Fatalf("Ps: %v (the probe is best-effort, never a fault)", err)
		}
		if got.Engine.Load != nil {
			t.Errorf("engine load = %+v, want null on a failed probe", got.Engine.Load)
		}
		for _, r := range got.Runs {
			if r.Load != nil {
				t.Errorf("run %s load = %+v, want null on a failed probe", r.ID, r.Load)
			}
		}
	})

	t.Run("a nil role reads unknown", func(t *testing.T) {
		p := psTestPlane(fakeRunReader{}, fakeProbe{}, nil)
		got, err := p.Ps(context.Background(), false, false)
		if err != nil {
			t.Fatalf("Ps: %v", err)
		}
		if got.Engine.Role != "unknown" {
			t.Errorf("role = %q, want unknown", got.Engine.Role)
		}
		if got.Runs == nil {
			t.Error("runs = nil, want the empty (never nil) list")
		}
	})

	t.Run("a failed run read is a fault", func(t *testing.T) {
		p := psTestPlane(fakeRunReader{err: errors.New("meta down")}, fakeProbe{}, leaderRoleState())
		if _, err := p.Ps(context.Background(), false, false); err == nil {
			t.Fatal("Ps with a failing run read = nil error, want a fault")
		}
	})

	t.Run("the sample tick rides every payload, history only on request", func(t *testing.T) {
		p := psTestPlane(fakeRunReader{runs: runs}, fakeProbe{samples: samples}, leaderRoleState())
		got, err := p.Ps(context.Background(), false, false)
		if err != nil {
			t.Fatalf("Ps: %v", err)
		}
		if got.SampleTick != 1 {
			t.Errorf("sample tick = %d, want the collector's 1", got.SampleTick)
		}
		if got.History != nil {
			t.Error("history rode a payload that did not ask for it")
		}
		got, err = p.Ps(context.Background(), false, true)
		if err != nil {
			t.Fatalf("Ps with history: %v", err)
		}
		if got.History == nil || len(got.History.Series) == 0 {
			t.Fatalf("history = %+v, want the collector's series", got.History)
		}
	})

	t.Run("a nil collector reads as never sampled", func(t *testing.T) {
		p := NewPsPlane(leaderRoleState(), fakeRunReader{runs: runs}, nil, nil, nil).(*psPlane)
		got, err := p.Ps(context.Background(), false, true)
		if err != nil {
			t.Fatalf("Ps: %v", err)
		}
		if got.SampleTick != 0 || got.Engine.Load != nil || got.History != nil {
			t.Errorf("nil-collector payload = tick %d load %+v history %+v, want all absent", got.SampleTick, got.Engine.Load, got.History)
		}
	})
}
