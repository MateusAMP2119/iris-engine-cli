package daemon

import (
	"context"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/MateusAMP2119/iris-lakehouse/internal/config"
)

// TestTryAdoptManagedLivePostmaster proves Startup adopts a live postmaster for
// this data directory instead of calling Supervisor.Start (which would fail on
// postmaster.pid). The shipped Manager.tryAdoptManaged path is exercised with a
// real listening TCP port and a real postmaster.pid layout.
func TestTryAdoptManagedLivePostmaster(t *testing.T) {
	home := t.TempDir()
	pgDir := filepath.Join(home, "pg")
	dataDir := filepath.Join(pgDir, "data")
	if err := os.MkdirAll(dataDir, 0o700); err != nil {
		t.Fatal(err)
	}
	// Pinned major so CheckDataDirVersion passes.
	if err := os.WriteFile(filepath.Join(dataDir, "PG_VERSION"), []byte(strconv.Itoa(PinnedMajorVersion)+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	// Engine-minted password file.
	const pw = "test-superuser-secret"
	if err := os.WriteFile(filepath.Join(pgDir, superuserPasswordFile), []byte(pw), 0o600); err != nil {
		t.Fatal(err)
	}

	// Listen on an ephemeral port to stand in for a live postmaster.
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	port := ln.Addr().(*net.TCPAddr).Port

	// postmaster.pid: line 1 = pid (this test process, alive), line 4 = port.
	pidBody := fmt.Sprintf("%d\n%s\n%d\n%d\n/tmp\nlocalhost\n\nready\n",
		os.Getpid(), dataDir, 0, port)
	if err := os.WriteFile(filepath.Join(dataDir, "postmaster.pid"), []byte(pidBody), 0o600); err != nil {
		t.Fatal(err)
	}

	settings := config.Resolve(
		config.Defaults(home),
		config.Layer{},
		config.Layer{},
		config.Layer{Socket: ptrStr(filepath.Join(home, "iris.sock"))},
	)
	if !settings.Managed() {
		t.Fatal("want managed mode")
	}

	startCalls := 0
	m := NewManager(settings, func(cfg SupervisorConfig) (Supervisor, error) {
		return &countingSupervisor{onStart: func() { startCalls++ }}, nil
	})

	dsn, err := m.Startup(context.Background())
	if err != nil {
		t.Fatalf("Startup: %v", err)
	}
	if startCalls != 0 {
		t.Fatalf("Supervisor.Start called %d times, want 0 (adopt path)", startCalls)
	}
	wantHost := fmt.Sprintf("localhost:%d", port)
	if got := dsn.Source().ConnString(); !containsAll(got, ManagedSuperuser, wantHost, pw) {
		t.Fatalf("admin DSN %q missing superuser/host/password", got)
	}
	// Live daemon pid file absent → we took Stop ownership of the "orphan".
	if m.sup == nil {
		t.Fatal("expected adoptedSupervisor Stop ownership when no iris daemon is alive")
	}
	// Shutdown must not error when pg_ctl is absent (best-effort); clear ownership.
	_ = m.Shutdown()
}

// TestTryAdoptManagedStalePidClearsAndColdStarts proves a dead postmaster.pid is
// removed and Startup proceeds to Supervisor.Start rather than failing.
func TestTryAdoptManagedStalePidClearsAndColdStarts(t *testing.T) {
	home := t.TempDir()
	pgDir := filepath.Join(home, "pg")
	dataDir := filepath.Join(pgDir, "data")
	if err := os.MkdirAll(dataDir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dataDir, "PG_VERSION"), []byte(strconv.Itoa(PinnedMajorVersion)+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(pgDir, superuserPasswordFile), []byte("pw"), 0o600); err != nil {
		t.Fatal(err)
	}
	// Pid that is not alive (PID 1 is init — Signal(0) may succeed on Linux for
	// any process we can see; use a high unused pid).
	stalePID := 2147483646
	if processAlive(stalePID) {
		t.Skip("stale pid unexpectedly alive")
	}
	pidBody := fmt.Sprintf("%d\n%s\n0\n59999\n/tmp\nlocalhost\n\nready\n", stalePID, dataDir)
	pidPath := filepath.Join(dataDir, "postmaster.pid")
	if err := os.WriteFile(pidPath, []byte(pidBody), 0o600); err != nil {
		t.Fatal(err)
	}

	settings := config.Resolve(
		config.Defaults(home),
		config.Layer{},
		config.Layer{},
		config.Layer{Socket: ptrStr(filepath.Join(home, "iris.sock"))},
	)
	startCalls := 0
	m := NewManager(settings, func(cfg SupervisorConfig) (Supervisor, error) {
		return &countingSupervisor{onStart: func() { startCalls++ }}, nil
	})
	if _, err := m.Startup(context.Background()); err != nil {
		t.Fatalf("Startup: %v", err)
	}
	if startCalls != 1 {
		t.Fatalf("Start calls = %d, want 1 cold start after clearing stale pid", startCalls)
	}
	if _, err := os.Stat(pidPath); !os.IsNotExist(err) {
		// tryAdopt removes stale pid before returning false; cold start's fake
		// does not recreate it.
		t.Fatalf("stale postmaster.pid still present: %v", err)
	}
}

type countingSupervisor struct {
	onStart func()
}

func (c *countingSupervisor) EnsureInstalled(context.Context) error { return nil }
func (c *countingSupervisor) Start(context.Context) error {
	if c.onStart != nil {
		c.onStart()
	}
	return nil
}
func (c *countingSupervisor) Stop() error { return nil }

func ptrStr(s string) *string { return &s }

func containsAll(s string, parts ...string) bool {
	for _, p := range parts {
		if p != "" && !strings.Contains(s, p) {
			return false
		}
	}
	return true
}
