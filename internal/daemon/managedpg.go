package daemon

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/MateusAMP2119/iris-engine-cli/internal/config"
)

// This file holds the managed-Postgres supervisor: the daemon-side machinery that,
// in managed mode, brings up a pinned Postgres build as a supervised child
// subprocess before any lane dispatch and stops it on shutdown (specification
// section 2, managed-vs-external Q/A). It is deliberately split into a pure seam
// (this file: the version pin + guard, the Supervisor interface, the mode-selecting
// Manager) and a real implementation (embeddedpg.go, backed by
// fergusstrange/embedded-postgres). Integration tests drive a fake Supervisor
// through the same seam, so lifecycle ordering and socket-vs-TCP configuration are
// proven with no download and no live Postgres; the real subprocess path is proven
// by the conformance tier.
//
// # Two modes, one code path
//
// The mode is E02.2's admin-DSN chain applied: config.Settings.Managed() (an empty
// pg_dsn) selects managed mode, any pg_dsn selects external. External mode starts
// no local instance and resolves the admin DSN straight through daemon.Resolve;
// managed mode mints its own superuser DSN and starts the subprocess. Both modes
// end at the same AdminDSN the rest of the engine derives every connection from
// (AdminDSN.Connect), so the only thing that differs between them is where the
// admin DSN came from and whether a subprocess was supervised.

// PinnedMajorVersion is the PostgreSQL major version this engine release pins for
// managed mode (specification section 2: "major version pinned per engine
// release"). It matches the version the embedded-postgres supervisor fetches
// (embeddedpg.go) and is the value the data-directory version guard enforces. It is
// deliberately a single, documented constant: bumping the managed Postgres is a
// deliberate release decision, changed here and in the embedded-postgres version in
// embeddedpg.go together.
const PinnedMajorVersion = 18

// ManagedSuperuser is the fixed role name of the engine-minted managed-Postgres
// superuser. The engine mints a fresh random password for it (mintPassword) and
// holds the pair only in memory, like the admin DSN; the CLI never sees it
// (specification section 2: "engine-minted superuser CLI never sees").
const ManagedSuperuser = "iris_engine"

// pgDirName is the managed-Postgres subdirectory of the workspace .iris tree: the
// engine-managed cluster, hosting both the data and meta databases, lives under
// <workspace>/.iris/pg (specification section 10).
const pgDirName = "pg"

// dataDirName is the Postgres data directory under the managed-Postgres directory;
// it is where initdb writes PG_VERSION.
const dataDirName = "data"

// superuserPasswordFile is the file under the managed-Postgres directory that
// carries the engine-minted superuser password across processes. embedded-postgres
// sets the superuser password at initdb and health-checks with it on every start,
// so a managed cluster initialized by `iris engine install` can only be reopened by
// a later `iris engine start` if both use the same password. The credential is
// persisted here, engine-owned and 0600, so the daemon (never the CLI) can reopen
// its own cluster; it is a point flagged for spec review alongside the engine-key
// storage decision (the spec says the managed superuser is memory-held and the CLI
// never sees it -- this keeps "the CLI never sees it" true while giving the daemon
// continuity across restarts).
const superuserPasswordFile = "superuser.pw"

// superuserPasswordPerm is the mode the persisted superuser password file is
// clamped to: owner read/write only.
const superuserPasswordPerm os.FileMode = 0o600

// ErrPGVersionMismatch is the sentinel returned when a managed data directory
// records a Postgres major version different from PinnedMajorVersion. Startup and
// install fail fast on it rather than letting a version-mismatched data directory
// be silently reinitialized or upgraded (specification section 2: "mismatch fails
// fast, never silent auto-upgrade"). Callers test it with errors.Is; the wrapped
// message names both the recorded and the pinned major.
var ErrPGVersionMismatch = errors.New("daemon: managed Postgres data directory version mismatch")

// ManagedPGDir returns the managed-Postgres directory for the given settings:
// <workspace>/.iris/pg, derived from the workspace .iris tree the control socket
// lives in (specification section 10: the socket and the managed Postgres both sit
// under <workspace>/.iris). It is the directory the pinned build's binaries and
// data directory are placed under.
func ManagedPGDir(s config.Settings) string {
	return filepath.Join(irisDir(s), pgDirName)
}

// irisDir returns the workspace .iris directory the settings are rooted at, taken
// as the directory the control socket sits in (config.Defaults places the socket at
// <workspace>/.iris/iris.sock). Managed-Postgres placement tracks that same .iris
// root so the whole engine state stays under one workspace directory.
func irisDir(s config.Settings) string {
	return filepath.Dir(s.Socket)
}

// managedDataDir returns the Postgres data directory under dir.
func managedDataDir(dir string) string {
	return filepath.Join(dir, dataDirName)
}

// ReadDataDirMajorVersion reads the Postgres major version a data directory
// records in its PG_VERSION file. A modern data directory records just the major
// (e.g. "18"); a pre-10 one records "major.minor" (e.g. "9.6"), whose major is the
// leading component. An absent PG_VERSION yields (0, nil): the data directory has
// not been initialized yet, so there is no recorded version to conflict with. A
// present-but-unparseable PG_VERSION is an error (fail fast), never silently
// treated as a match.
func ReadDataDirMajorVersion(dataDir string) (int, error) {
	raw, err := os.ReadFile(filepath.Join(dataDir, "PG_VERSION")) //nolint:gosec // G304: dataDir is the engine-owned managed-Postgres data dir, never user or network input.
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return 0, nil
		}
		return 0, fmt.Errorf("daemon: read managed data dir PG_VERSION: %w", err)
	}
	field := strings.TrimSpace(string(raw))
	major := strings.SplitN(field, ".", 2)[0]
	n, convErr := strconv.Atoi(major)
	if convErr != nil {
		return 0, fmt.Errorf("daemon: managed data dir PG_VERSION %q is not a valid Postgres version", field)
	}
	return n, nil
}

// CheckDataDirVersion fails fast when a managed data directory records a Postgres
// major version different from want (specification section 2: mismatch fails fast,
// never silent auto-upgrade). It is read-only: it never touches the data directory,
// so a mismatch is surfaced, never repaired by a silent reinitialize or upgrade. A
// data directory with no recorded version yet (a fresh dir) is not a mismatch. An
// unreadable or malformed PG_VERSION is a fail-fast error, not a silent match.
func CheckDataDirVersion(dataDir string, want int) error {
	recorded, err := ReadDataDirMajorVersion(dataDir)
	if err != nil {
		return err
	}
	if recorded == 0 {
		return nil // fresh data dir: nothing recorded to conflict with.
	}
	if recorded != want {
		return fmt.Errorf("%w: data directory %s records major %d but this engine pins major %d; "+
			"resolve manually rather than upgrading in place", ErrPGVersionMismatch, dataDir, recorded, want)
	}
	return nil
}

// SupervisorConfig is the configuration the Manager hands a Supervisor for one
// managed-Postgres instance. It is what a fake records to prove the Manager's
// intent (socket-vs-TCP, the minted credential) and what the real supervisor turns
// into an embedded-postgres configuration.
type SupervisorConfig struct {
	// Dir is the managed-Postgres directory (<workspace>/.iris/pg): where the
	// pinned build's binaries are placed.
	Dir string
	// DataDir is the Postgres data directory (Dir/data): where initdb writes
	// PG_VERSION and the cluster's data.
	DataDir string
	// Superuser is the engine-minted superuser role name (ManagedSuperuser).
	Superuser string
	// Password is the engine-minted superuser password: crypto-random, held only in
	// daemon memory, never surfaced to the CLI.
	Password string
	// Port is the TCP port the managed instance is reached on locally.
	Port uint32
	// TCP reports whether the managed instance exposes a TCP listener beyond the
	// local host. False (the default) keeps it local; true is set only when a
	// standby / remote topology needs it (specification section 2: "TCP when
	// standbys need it").
	TCP bool
}

// Supervisor supervises one managed-Postgres subprocess lifecycle behind a seam.
// The production implementation (embeddedpg.go) fetches a pinned, checksum-verified
// Postgres and runs it as a child subprocess; a fake drives the integration tier.
//
// Error wrapping is the implementation's responsibility: each method returns a
// descriptive, %w-wrapped error naming the failed operation and its managed-
// Postgres context. The Manager surfaces those errors unchanged (Startup, Shutdown)
// so exactly one layer owns the prefix and no message is double-wrapped; the Manager
// wraps only its own operations (minting, port reservation, building the
// supervisor). A fake used in tests should follow the same contract when it returns
// a non-nil error.
type Supervisor interface {
	// EnsureInstalled downloads and places the pinned Postgres build and
	// materializes the data directory if absent, without leaving a server running.
	// It is the daemonless `iris engine install` leg and is idempotent.
	EnsureInstalled(ctx context.Context) error
	// Start brings the managed-Postgres subprocess up and returns once it is
	// accepting connections.
	Start(ctx context.Context) error
	// Stop stops the managed-Postgres subprocess. It is a no-op if nothing is
	// running.
	Stop() error
}

// SupervisorFactory builds a Supervisor for a SupervisorConfig. The production
// factory is EmbeddedSupervisor (embeddedpg.go); tests inject a fake so the Manager
// can be exercised without a download or a live Postgres.
type SupervisorFactory func(SupervisorConfig) (Supervisor, error)

// Manager selects managed vs external mode from the engine settings and, in managed
// mode, supervises the local Postgres subprocess through a SupervisorFactory. It is
// the single place the two modes are reconciled onto one admin-DSN code path.
type Manager struct {
	settings config.Settings
	newSup   SupervisorFactory
	sup      Supervisor // the running managed supervisor, nil in external mode or before Startup.
}

// NewManager builds a Manager for the resolved settings, using newSup to construct
// the managed-Postgres supervisor when managed mode needs one. In external mode the
// factory is never called.
func NewManager(settings config.Settings, newSup SupervisorFactory) *Manager {
	return &Manager{settings: settings, newSup: newSup}
}

// Install performs the daemonless `iris engine install` managed-Postgres leg: in
// managed mode it downloads and places the pinned, checksum-verified Postgres under
// <workspace>/.iris/pg and materializes the data directory (recording PG_VERSION),
// failing fast on a version-mismatched existing data directory. In external mode
// there is no local instance to install, so it is a no-op. It is idempotent.
//
// This implements only the managed-Postgres download/placement leg. Meta bootstrap,
// the control socket, and the engine key are `iris engine install`'s remaining legs
// and land in E02.4; they extend this method rather than replace it.
func (m *Manager) Install(ctx context.Context) error {
	if !m.settings.Managed() {
		return nil // external mode: the user's Postgres, nothing to install locally.
	}
	cfg, err := m.managedConfig()
	if err != nil {
		return err
	}
	if err := CheckDataDirVersion(cfg.DataDir, PinnedMajorVersion); err != nil {
		return err
	}
	sup, err := m.newSup(cfg)
	if err != nil {
		return fmt.Errorf("daemon: build managed Postgres supervisor: %w", err)
	}
	return sup.EnsureInstalled(ctx)
}

// Startup resolves the admin DSN for the configured mode and, in managed mode,
// starts the managed-Postgres subprocess so it is accepting connections before the
// caller dispatches any lane (specification section 2). External mode starts no
// local instance and resolves the admin DSN straight through the E02.2 chain. The
// returned AdminDSN is what the caller Connects meta and data from -- one code path
// for both modes. Call Shutdown to stop a managed instance.
//
// Managed Startup mints a fresh superuser credential each call; re-opening an
// already-initialized managed cluster with a matching credential (so a restarted
// daemon can reconnect) is E02.4's connection-bootstrap concern. Startup is exercised
// here through the fake supervisor; the CLI wires only Install in this task.
func (m *Manager) Startup(ctx context.Context) (AdminDSN, error) {
	if !m.settings.Managed() {
		// External mode: no local instance; the admin DSN is the user's, resolved by
		// the same chain a daemonless lifecycle command uses.
		return Resolve(m.settings)
	}

	cfg, err := m.managedConfig()
	if err != nil {
		return AdminDSN{}, err
	}
	// Guard the data directory before starting: a version-mismatched data dir must
	// fail fast, never be silently reinitialized by the subprocess on start.
	if err := CheckDataDirVersion(cfg.DataDir, PinnedMajorVersion); err != nil {
		return AdminDSN{}, err
	}
	sup, err := m.newSup(cfg)
	if err != nil {
		return AdminDSN{}, fmt.Errorf("daemon: build managed Postgres supervisor: %w", err)
	}
	// Supervisor.Start already returns a descriptive, %w-wrapped error (see the
	// interface contract), so surface it as-is rather than double-prefixing it.
	if err := sup.Start(ctx); err != nil {
		return AdminDSN{}, err
	}
	m.sup = sup
	return AdminDSN{conn: managedDSN(cfg)}, nil
}

// Shutdown stops a managed-Postgres subprocess started by Startup. In external mode,
// or before a managed Startup, it is a no-op: there is no local instance to stop.
// Supervisor.Stop owns the descriptive error (see the interface contract), so
// Shutdown surfaces it as-is rather than re-wrapping the same prefix.
func (m *Manager) Shutdown() error {
	if m.sup == nil {
		return nil
	}
	sup := m.sup
	m.sup = nil
	return sup.Stop()
}

// managedConfig builds the SupervisorConfig for managed mode: the .iris/pg
// placement, the engine-minted superuser credential (persisted so install and every
// later start share it), a free local port, and the socket-vs-TCP choice (TCP only
// when the engine's own TCP listener -- the standby / remote topology signal -- is
// configured).
func (m *Manager) managedConfig() (SupervisorConfig, error) {
	dir := ManagedPGDir(m.settings)
	password, err := resolveManagedPassword(dir)
	if err != nil {
		return SupervisorConfig{}, err
	}
	port, err := freeTCPPort()
	if err != nil {
		return SupervisorConfig{}, err
	}
	return SupervisorConfig{
		Dir:       dir,
		DataDir:   managedDataDir(dir),
		Superuser: ManagedSuperuser,
		Password:  password,
		Port:      port,
		TCP:       m.settings.TCP != "",
	}, nil
}

// resolveManagedPassword returns the engine-minted managed superuser password for
// the managed-Postgres directory, reusing the one persisted on first use so a
// managed cluster created by `iris engine install` can be reopened by a later
// `iris engine start`. embedded-postgres health-checks with the configured password
// on every start, so a fresh password each start would fail against the existing
// data directory. The credential is crypto-random, engine-owned, and stored 0600;
// the CLI never reads it. A fresh workspace (no file yet) mints a new one and
// persists it, so independent engines still get distinct credentials.
func resolveManagedPassword(dir string) (string, error) {
	path := filepath.Join(dir, superuserPasswordFile)
	raw, err := os.ReadFile(path) //nolint:gosec // G304: path is the engine-owned managed-Postgres dir, never user or network input.
	switch {
	case err == nil:
		if pw := strings.TrimSpace(string(raw)); pw != "" {
			return pw, nil
		}
		// An empty/corrupt file: fall through and re-mint rather than start with no
		// credential.
	case !errors.Is(err, os.ErrNotExist):
		return "", fmt.Errorf("daemon: read managed superuser credential: %w", err)
	}

	password, err := mintPassword()
	if err != nil {
		return "", err
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", fmt.Errorf("daemon: create managed Postgres directory %s: %w", dir, err)
	}
	if err := os.WriteFile(path, []byte(password), superuserPasswordPerm); err != nil {
		return "", fmt.Errorf("daemon: persist managed superuser credential: %w", err)
	}
	return password, nil
}

// managedDSN builds the admin DSN for a managed instance from its engine-minted
// superuser credential and local port. The raw string, password and all, only ever
// reaches the connection layer through AdminDSN.Source; every formatting path
// redacts it.
func managedDSN(cfg SupervisorConfig) string {
	return fmt.Sprintf("postgres://%s:%s@localhost:%d/postgres", cfg.Superuser, cfg.Password, cfg.Port)
}

// mintPassword mints a fresh 128-bit engine superuser password from crypto/rand,
// hex-encoded so it is safe to place in a DSN. A random secret per instance, never
// derived from anything the CLI could reconstruct.
func mintPassword() (string, error) {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", fmt.Errorf("daemon: mint managed superuser password: %w", err)
	}
	return hex.EncodeToString(b[:]), nil
}

// freeTCPPort asks the kernel for a free localhost TCP port by binding :0 and
// reading back the assigned port, then releasing it for the managed instance to
// bind. A brief bind/close window, acceptable for a local single-instance managed
// Postgres.
func freeTCPPort() (uint32, error) {
	l, err := net.Listen("tcp", "localhost:0")
	if err != nil {
		return 0, fmt.Errorf("daemon: reserve a free port for managed Postgres: %w", err)
	}
	defer func() { _ = l.Close() }()
	addr, ok := l.Addr().(*net.TCPAddr)
	if !ok {
		return 0, errors.New("daemon: reserved listener has no TCP address")
	}
	return uint32(addr.Port), nil //nolint:gosec // G115: a TCP port is 0-65535, always in uint32 range.
}
