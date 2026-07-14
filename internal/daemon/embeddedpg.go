package daemon

import (
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"time"

	embeddedpostgres "github.com/fergusstrange/embedded-postgres"
)

// managedStartTimeout bounds bringing the managed instance up: on a cold data
// directory this includes initdb, which is slow, so it is well above embedded-
// postgres's 15s default. Readiness is still decided by the health check, not by
// elapsed time; this only bounds how long that check waits before failing.
const managedStartTimeout = 90 * time.Second

// This file is the production Supervisor: the managed-Postgres subprocess backed by
// fergusstrange/embedded-postgres. embedded-postgres fetches a pinned,
// checksum-verified Postgres distribution and runs it via pg_ctl as a child
// subprocess -- never linked into the engine binary, so the engine stays a single
// cgo-free static executable. It is wrapped behind the Supervisor seam so the
// daemon logic depends only on the interface: integration tests use a fake, and
// only the conformance tier drives this real path (which downloads a real
// Postgres).

// pinnedEmbeddedVersion is the exact embedded-postgres build the engine pins. Its
// major must equal PinnedMajorVersion; the two are bumped together as a deliberate
// release decision.
const pinnedEmbeddedVersion = embeddedpostgres.V18

// pgInstance is the lifecycle subset of embedded-postgres that embeddedSupervisor
// drives. *embeddedpostgres.EmbeddedPostgres satisfies it directly; a scripted fake
// substitutes it in tests so the Start/Stop failure paths (which would otherwise
// need a real download and a way to force pg_ctl to fail) are provable in-process.
type pgInstance interface {
	Start() error
	Stop() error
}

// EmbeddedSupervisor is the production SupervisorFactory. It builds a managed-
// Postgres supervisor that places the pinned build under the configured
// <workspace>/.iris/pg directory and supervises it as a child subprocess. It
// satisfies daemon.SupervisorFactory, and is the factory a Manager is built with on
// both engine paths that need Postgres: `iris engine install` (install.go) and the
// daemon lifecycle (lifecycle.go).
func EmbeddedSupervisor(cfg SupervisorConfig) (Supervisor, error) {
	s := &embeddedSupervisor{cfg: cfg}
	s.newInstance = s.buildInstance
	return s, nil
}

// embeddedSupervisor adapts embedded-postgres to the Supervisor seam. It holds the
// running instance between Start and Stop so a supervised subprocess can be stopped
// on shutdown. newInstance builds the underlying instance and is a field so tests
// can inject a scripted pgInstance in place of the real embedded-postgres one.
type embeddedSupervisor struct {
	cfg         SupervisorConfig
	newInstance func() pgInstance
	running     pgInstance
}

// buildInstance builds an embedded-postgres instance configured to place its
// binaries and data under the managed-Postgres directory, pin the major version,
// and use the engine-minted superuser credential. Postgres server output is
// discarded rather than written to the process's stdout/stderr, so the CLI contract
// (stdout carries only command output) holds and the minted credential can never
// ride Postgres logs into the CLI's streams. TCP beyond localhost is enabled only
// when the config asks for it (standby topology); otherwise the instance stays
// local.
func (s *embeddedSupervisor) buildInstance() pgInstance {
	cfg := embeddedpostgres.DefaultConfig().
		Version(pinnedEmbeddedVersion).
		Username(s.cfg.Superuser).
		Password(s.cfg.Password).
		Database("postgres").
		Port(s.cfg.Port).
		BinariesPath(s.cfg.Dir).
		DataPath(s.cfg.DataDir).
		RuntimePath(filepath.Join(s.cfg.Dir, "runtime")).
		StartTimeout(managedStartTimeout).
		Logger(io.Discard)
	if s.cfg.TCP {
		cfg = cfg.StartParameters(map[string]string{"listen_addresses": "*"})
	}
	return embeddedpostgres.NewDatabase(cfg)
}

// EnsureInstalled materializes the managed Postgres: it downloads and places the
// pinned, checksum-verified build under the managed-Postgres directory and
// initializes the data directory (recording PG_VERSION), leaving no server running.
// On a cold install it runs the subprocess only long enough for initdb, then stops
// it. It is idempotent: when the binaries are already extracted and the data
// directory already records the pinned major, it does nothing at all -- it does not
// even start the subprocess, so it never depends on the engine-minted password
// matching an already-initialized cluster. Continuity of that credential across
// separate runs -- so a later `iris engine start` can re-open an existing managed
// cluster -- is managedpg.go's: resolveManagedPassword persists the minted
// superuser password (engine-owned, 0600) under the managed-Postgres directory and
// every later start reuses it.
//
// Context handling: EnsureInstalled honors a context cancelled before it begins, but
// once embedded-postgres's Start is underway a cancellation cannot interrupt it --
// the library offers no cancellation hook, so Start blocks until it succeeds or hits
// managedStartTimeout. Callers with short-lived contexts must account for that
// bound; racing Start in a goroutine to return early on ctx.Done is deliberately not
// done, as it would leave the library call -- and any postgres it spawned -- running
// detached with no handle (an orphan).
//
// Failed-stop safety: if the subprocess starts but the follow-up stop fails, the
// process is running with no other handle to it. EnsureInstalled never strands it
// silently: it retries the stop best-effort, and if that also fails it retains the
// instance handle (so a later Stop can still reach the process) and returns an error
// naming the orphan risk and its remediation.
func (s *embeddedSupervisor) EnsureInstalled(ctx context.Context) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if s.alreadyMaterialized() {
		return nil
	}
	instance := s.newInstance()
	if err := instance.Start(); err != nil {
		return fmt.Errorf("daemon: materialize managed Postgres under %s: %w", s.cfg.Dir, err)
	}
	if err := instance.Stop(); err != nil {
		// Start succeeded, so a postgres subprocess is running. A single stop failure
		// must never silently strand it: retry once, and only if that also fails give
		// up -- retaining the handle so a later Stop can reach it and surfacing the
		// orphan risk.
		if retryErr := instance.Stop(); retryErr == nil {
			return nil
		}
		s.running = instance
		return fmt.Errorf("daemon: managed Postgres started during install but could not be stopped; "+
			"a postgres subprocess may still be running under %s -- stop it before retrying "+
			"(pg_ctl stop -D %s): %w", s.cfg.Dir, s.cfg.DataDir, err)
	}
	return nil
}

// alreadyMaterialized reports whether the managed Postgres is already installed:
// the pg_ctl binary is extracted under the managed directory and the data directory
// records exactly the pinned major version. When both hold, a re-install is a no-op.
// A version-mismatched existing data directory is not "already materialized" and is
// caught by the fail-fast guard the Manager runs before this method.
func (s *embeddedSupervisor) alreadyMaterialized() bool {
	if _, err := os.Stat(filepath.Join(s.cfg.Dir, "bin", "pg_ctl")); err != nil {
		return false
	}
	major, err := ReadDataDirMajorVersion(s.cfg.DataDir)
	return err == nil && major == PinnedMajorVersion
}

// Start brings the managed-Postgres subprocess up and returns once it is accepting
// connections (embedded-postgres blocks on a readiness health check before
// returning). The running instance is retained so Stop can shut it down.
//
// Context handling: Start honors a context cancelled before it begins, but a
// cancellation during the underlying embedded-postgres Start cannot interrupt it --
// the library has no cancellation hook, so Start blocks until readiness or
// managedStartTimeout. Callers with short-lived contexts must account for that
// bound.
func (s *embeddedSupervisor) Start(ctx context.Context) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	instance := s.newInstance()
	if err := instance.Start(); err != nil {
		return fmt.Errorf("daemon: start managed Postgres under %s: %w", s.cfg.Dir, err)
	}
	s.running = instance
	return nil
}

// Stop stops the managed-Postgres subprocess started by Start. It is a no-op when
// nothing is running.
func (s *embeddedSupervisor) Stop() error {
	if s.running == nil {
		return nil
	}
	instance := s.running
	s.running = nil
	if err := instance.Stop(); err != nil {
		return fmt.Errorf("daemon: stop managed Postgres: %w", err)
	}
	return nil
}
