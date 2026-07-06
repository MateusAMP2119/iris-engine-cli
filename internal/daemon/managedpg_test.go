package daemon_test

import (
	"context"
	"strings"
	"testing"

	"github.com/MateusAMP2119/iris-engine-cli/internal/config"
	"github.com/MateusAMP2119/iris-engine-cli/internal/daemon"
)

// recordingFactory is a test SupervisorFactory: it records every SupervisorConfig
// it is asked to build and returns fake supervisors that append their lifecycle
// calls to one shared, ordered event log. Because the daemon builds the real
// managed-Postgres supervisor behind the same factory seam, these fakes let the
// integration tier prove lifecycle ordering and socket-vs-TCP configuration with
// no download and no live Postgres.
type recordingFactory struct {
	built    []daemon.SupervisorConfig
	events   []string
	startErr error
}

// New satisfies daemon.SupervisorFactory.
func (rf *recordingFactory) New(cfg daemon.SupervisorConfig) (daemon.Supervisor, error) {
	rf.built = append(rf.built, cfg)
	return &fakeSupervisor{rf: rf}, nil
}

// fakeSupervisor records its lifecycle calls into the factory's shared log, so a
// test can assert their order relative to a simulated dispatch hook.
type fakeSupervisor struct{ rf *recordingFactory }

func (s *fakeSupervisor) EnsureInstalled(context.Context) error {
	s.rf.events = append(s.rf.events, "ensure-installed")
	return nil
}

func (s *fakeSupervisor) Start(context.Context) error {
	s.rf.events = append(s.rf.events, "pg-start")
	return s.rf.startErr
}

func (s *fakeSupervisor) Stop() error {
	s.rf.events = append(s.rf.events, "pg-stop")
	return nil
}

// managedSettings resolves engine settings for managed mode (no pg_dsn) rooted at
// workspace, optionally enabling the daemon's TCP listener -- the signal a standby
// / remote topology is in play, which the managed Postgres mirrors by exposing TCP.
func managedSettings(workspace, tcp string) config.Settings {
	flags := config.Layer{}
	if tcp != "" {
		flags.TCP = ptr(tcp)
	}
	return config.Resolve(config.Defaults(workspace), config.Layer{}, config.Layer{}, flags)
}

// indexOf returns the position of want in events, or -1.
func indexOf(events []string, want string) int {
	for i, e := range events {
		if e == want {
			return i
		}
	}
	return -1
}

// assertBefore fails the test unless earlier occurs strictly before later in events.
func assertBefore(t *testing.T, events []string, earlier, later string) {
	t.Helper()
	ei, li := indexOf(events, earlier), indexOf(events, later)
	if ei < 0 {
		t.Fatalf("event %q never occurred in %v", earlier, events)
	}
	if li < 0 {
		t.Fatalf("event %q never occurred in %v", later, events)
	}
	if ei >= li {
		t.Fatalf("event %q (at %d) did not occur before %q (at %d): %v", earlier, ei, later, li, events)
	}
}

// TestManagedPGSubprocessLifecycle proves managed mode's subprocess lifecycle
// (specification section 2, managed-vs-external Q/A): the daemon starts the local
// Postgres subprocess -- which hosts both the data and meta databases in one
// cluster reached through one admin DSN -- before any lane could be dispatched, and
// stops it on shutdown. The instance listens on a local unix socket by default,
// exposing TCP only when a standby / remote topology needs it. The admin DSN is
// built from an engine-minted superuser credential that stays memory-only (redacts
// under formatting), so the CLI never sees it.
//
// spec: S02/managed-pg-subprocess-lifecycle
func TestManagedPGSubprocessLifecycle(t *testing.T) {
	ctx := context.Background()

	t.Run("starts before dispatch and stops on shutdown", func(t *testing.T) {
		settings := managedSettings(t.TempDir(), "")
		if !settings.Managed() {
			t.Fatal("expected managed mode with no pg_dsn set")
		}
		rf := &recordingFactory{}
		mgr := daemon.NewManager(settings, rf.New)

		admin, err := mgr.Startup(ctx)
		if err != nil {
			t.Fatalf("Startup: %v", err)
		}
		// The dispatch hook can only run after Startup returns: record it now, then
		// shut down. The ordering assertion below encodes "started before any lane".
		rf.events = append(rf.events, "dispatch")
		if err := mgr.Shutdown(); err != nil {
			t.Fatalf("Shutdown: %v", err)
		}

		assertBefore(t, rf.events, "pg-start", "dispatch")
		assertBefore(t, rf.events, "dispatch", "pg-stop")

		// One instance hosts both databases: meta (store) and data (pg) are each
		// dialed from the single managed admin DSN.
		meta, data := &recordingDialer{}, &recordingDialer{}
		if err := admin.Connect(ctx, meta, data); err != nil {
			t.Fatalf("Connect: %v", err)
		}
		if len(meta.dialed) != 1 || len(data.dialed) != 1 {
			t.Fatalf("meta dialed %d, data dialed %d; want 1 each from the one managed instance", len(meta.dialed), len(data.dialed))
		}
		if meta.dialed[0] == "" || meta.dialed[0] != data.dialed[0] {
			t.Errorf("meta (%q) and data (%q) did not derive from one managed admin DSN", meta.dialed[0], data.dialed[0])
		}
	})

	t.Run("local socket by default, no TCP", func(t *testing.T) {
		settings := managedSettings(t.TempDir(), "")
		rf := &recordingFactory{}
		mgr := daemon.NewManager(settings, rf.New)
		if _, err := mgr.Startup(ctx); err != nil {
			t.Fatalf("Startup: %v", err)
		}
		if len(rf.built) != 1 {
			t.Fatalf("built %d supervisors, want 1", len(rf.built))
		}
		if rf.built[0].TCP {
			t.Error("managed Postgres enabled TCP with no standby topology; the default is a local unix socket")
		}
		if rf.built[0].Dir == "" {
			t.Error("managed supervisor built with no data directory")
		}
	})

	t.Run("TCP only when standbys need it", func(t *testing.T) {
		settings := managedSettings(t.TempDir(), "0.0.0.0:5433")
		rf := &recordingFactory{}
		mgr := daemon.NewManager(settings, rf.New)
		if _, err := mgr.Startup(ctx); err != nil {
			t.Fatalf("Startup: %v", err)
		}
		if len(rf.built) != 1 {
			t.Fatalf("built %d supervisors, want 1", len(rf.built))
		}
		if !rf.built[0].TCP {
			t.Error("managed Postgres did not expose TCP even though the engine's TCP listener (standby topology) is enabled")
		}
	})

	t.Run("engine-minted superuser stays memory-only", func(t *testing.T) {
		settings := managedSettings(t.TempDir(), "")
		rf := &recordingFactory{}
		mgr := daemon.NewManager(settings, rf.New)
		admin, err := mgr.Startup(ctx)
		if err != nil {
			t.Fatalf("Startup: %v", err)
		}
		// The supervisor is configured with an engine-minted superuser password.
		pw := rf.built[0].Password
		if pw == "" {
			t.Fatal("managed supervisor was built without an engine-minted superuser password")
		}
		// The credential is engine-generated, not a fixed literal: it is long and
		// not the placeholder a human would type.
		if len(pw) < 16 || pw == "postgres" || pw == "password" {
			t.Errorf("minted superuser password %q does not look engine-generated (crypto/rand)", pw)
		}
		// The admin DSN carries that credential but never renders it: the CLI, a log
		// line, or a meta row can only ever see the redaction sentinel.
		for _, rendered := range []string{admin.String(), admin.GoString(), admin.Source().String()} {
			if strings.Contains(rendered, pw) {
				t.Errorf("managed admin DSN leaked the minted superuser password: %q", rendered)
			}
		}
	})

	t.Run("two managed startups mint distinct credentials", func(t *testing.T) {
		// crypto/rand, not a derived constant: independent managers mint different
		// superuser passwords.
		mint := func() string {
			rf := &recordingFactory{}
			mgr := daemon.NewManager(managedSettings(t.TempDir(), ""), rf.New)
			if _, err := mgr.Startup(ctx); err != nil {
				t.Fatalf("Startup: %v", err)
			}
			return rf.built[0].Password
		}
		if a, b := mint(), mint(); a == b {
			t.Errorf("two managed startups minted the same password %q; want crypto-random distinct credentials", a)
		}
	})
}

// TestExternalPGIdenticalPath proves that a user-provided pg_dsn drives external
// mode: the engine starts no local Postgres instance, and it reaches Postgres
// through the exact same admin-DSN code path managed mode uses -- resolve an admin
// DSN, then Connect meta (store) and data (pg) from it. The only difference between
// the two modes is where the admin DSN comes from (a minted local credential vs.
// the user's DSN) and whether a subprocess was supervised; everything downstream of
// Resolve is one code path (specification section 2: "two modes, one code path ...
// external: no local instance, internally identical (same admin-DSN path)").
//
// spec: S02/external-pg-identical-path
func TestExternalPGIdenticalPath(t *testing.T) {
	ctx := context.Background()
	const userDSN = "postgres://user:secretpw@db.example.com:5432/postgres?sslmode=require"

	settings := config.Resolve(
		config.Defaults(t.TempDir()),
		config.Layer{}, config.Layer{},
		config.Layer{PgDSN: ptr(userDSN)},
	)
	if settings.Managed() {
		t.Fatal("a user-provided pg_dsn must select external mode")
	}

	rf := &recordingFactory{}
	mgr := daemon.NewManager(settings, rf.New)

	admin, err := mgr.Startup(ctx)
	if err != nil {
		t.Fatalf("Startup: %v", err)
	}

	t.Run("no local instance started", func(t *testing.T) {
		if len(rf.built) != 0 {
			t.Errorf("external mode built a managed supervisor (%d); it must start no local instance", len(rf.built))
		}
		if len(rf.events) != 0 {
			t.Errorf("external mode ran managed-Postgres lifecycle events %v; it must start no local instance", rf.events)
		}
	})

	t.Run("shutdown never touches a local instance", func(t *testing.T) {
		if err := mgr.Shutdown(); err != nil {
			t.Fatalf("Shutdown: %v", err)
		}
		if len(rf.events) != 0 {
			t.Errorf("external Shutdown ran managed-Postgres events %v; there is no local instance to stop", rf.events)
		}
	})

	t.Run("identical admin-DSN code path", func(t *testing.T) {
		// The admin DSN external mode resolves is exactly the one the E02.2 admin-DSN
		// chain resolves for the same settings: Manager.Startup funnels external mode
		// straight through daemon.Resolve, adding no divergent path.
		direct, err := daemon.Resolve(settings)
		if err != nil {
			t.Fatalf("daemon.Resolve: %v", err)
		}
		if admin.Source().ConnString() != direct.Source().ConnString() {
			t.Error("external Manager.Startup diverged from the daemon.Resolve admin-DSN chain")
		}
		// And every connection derives from that one admin DSN, meta and data alike --
		// the same Connect funnel managed mode uses.
		meta, data := &recordingDialer{}, &recordingDialer{}
		if err := admin.Connect(ctx, meta, data); err != nil {
			t.Fatalf("Connect: %v", err)
		}
		if len(meta.dialed) != 1 || meta.dialed[0] != userDSN {
			t.Errorf("meta dialed %v, want [%q]", meta.dialed, userDSN)
		}
		if len(data.dialed) != 1 || data.dialed[0] != userDSN {
			t.Errorf("data dialed %v, want [%q]", data.dialed, userDSN)
		}
	})

	t.Run("external install starts no local instance", func(t *testing.T) {
		// The daemonless `iris engine install` leg also honors external mode: with a
		// user DSN there is nothing to download or place locally.
		irf := &recordingFactory{}
		imgr := daemon.NewManager(settings, irf.New)
		if err := imgr.Install(ctx); err != nil {
			t.Fatalf("Install: %v", err)
		}
		if len(irf.built) != 0 || len(irf.events) != 0 {
			t.Errorf("external Install touched a local instance: built=%v events=%v", irf.built, irf.events)
		}
	})
}
