package daemon

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"

	"github.com/MateusAMP2119/iris-lakehouse/internal/config"
	"github.com/MateusAMP2119/iris-lakehouse/internal/pg"
	"github.com/MateusAMP2119/iris-lakehouse/internal/store"
)

// This file wires the full `iris engine install` bootstrap: probe for the meta
// database and create it with a plain CREATE DATABASE if missing, ensure its
// tables, create the partitioned public.data_journal on the data connection, store
// the minted ed25519 engine key on the meta connection, and set up the socket.
//
// Two entry points run that one sequence. InstallEngine is the live path the CLI
// drives against a real cluster: it brings Postgres up through the Manager and runs
// the legs over the pgx-backed clients (store.OpenInstallConns, pg.Connect), which
// the daemon composes without ever importing pgx itself. BootstrapEngine is the
// same sequence over injected seams -- the connection seams (store.Execer, pg.DB)
// plus the small MetaProbe and SocketPreparer here, with the engine key
// (enginekey.go) and the socket setup (socket.go) -- so the ordering is proven with
// recording fakes and no live Postgres.

// InstallEngine runs the `iris engine install` bootstrap against a live cluster: it
// brings up Postgres for the configured mode -- the managed local subprocess or the
// external cluster -- through the one Manager code path, creates meta alongside the
// dedicated data database, ensures meta's control tables and the partitioned data
// journal, and sets up the control socket, then shuts a managed instance back down.
// The pgx-backed connection seams live in store and pg (the daemon never imports
// pgx); this composes them. Every leg is create-if-missing, so InstallEngine is
// idempotent and a later `iris engine start` that re-checks the same legs (meta
// schema at election, journal at apply) touches nothing already present.
//
// It mints the ed25519 engine key into the engine_key meta table (a single-row
// create-once INSERT on the meta connection, superuser-free), superseding the
// earlier per-database GUC (which needed SUPERUSER the external admin role lacks)
// and the workspace key file (which forced a shared filesystem for HA). The two-
// database creation and the key insert both run privilege-safe in managed and
// external modes.
func InstallEngine(ctx context.Context, s config.Settings, logger *slog.Logger) (InstallReport, error) {
	if logger == nil {
		logger = slog.New(slog.NewTextHandler(io.Discard, nil))
	}

	mgr := NewManager(s, EmbeddedSupervisor)
	// Managed mode: download and place the pinned Postgres before it can be started;
	// external mode: a no-op.
	if err := mgr.Install(ctx); err != nil {
		return InstallReport{}, fmt.Errorf("daemon: install managed Postgres: %w", err)
	}
	// Bring up Postgres and resolve the admin DSN -- the managed subprocess or the
	// external cluster -- the one code path both modes share. Shutdown stops a managed
	// instance on return; it is a no-op in external mode.
	adminDSN, err := mgr.Startup(ctx)
	if err != nil {
		return InstallReport{}, fmt.Errorf("daemon: bring up Postgres for install: %w", err)
	}
	defer func() { _ = mgr.Shutdown() }()

	src := adminDSN.Source()

	// Preflight the admin DSN's privileges before the first CREATE DATABASE, so a
	// misconfigured role fails install with the missing grant named (CREATEROLE is
	// otherwise first needed only at daemon start, letting install report success
	// on a DSN that was never viable).
	if err := CheckPrivileges(ctx, NewAdminPrivilegeReader(src.ConnString())); err != nil {
		return InstallReport{}, err
	}
	logger.Info("engine install: admin DSN privileges verified")

	// Open the admin and meta connections the meta bootstrap rides. Close on a
	// background context so a cancelled install still tears the connections down.
	conns, err := store.OpenInstallConns(ctx, src)
	if err != nil {
		return InstallReport{}, fmt.Errorf("daemon: open meta connections for install: %w", err)
	}
	defer func() { _ = conns.Close(context.Background()) }()

	// Create meta if missing (CREATE DATABASE has no IF NOT EXISTS; the probe is the
	// guard), then ensure its control tables -- the one-time install path, the same
	// embedded DDL a leader re-checks at election.
	var report InstallReport
	exists, err := conns.MetaExists(ctx)
	if err != nil {
		return InstallReport{}, fmt.Errorf("daemon: probe meta database for install: %w", err)
	}
	if !exists {
		if err := conns.Cluster().Exec(ctx, store.CreateMetaDatabaseDDL()); err != nil {
			return InstallReport{}, fmt.Errorf("daemon: create meta database for install: %w", err)
		}
		report.MetaCreated = true
		logger.Info("engine install: created meta database", "database", store.MetaDatabase)
	} else {
		logger.Info("engine install: meta database already exists", "database", store.MetaDatabase)
	}
	if err := store.EnsureSchema(ctx, conns.Meta()); err != nil {
		return InstallReport{}, fmt.Errorf("daemon: ensure meta schema for install: %w", err)
	}
	logger.Info("engine install: ensured meta schema")

	// Create the dedicated data database and its partitioned journal. pg.Connect
	// creates the data database create-if-missing; EnsureJournal is the same leg a
	// declare apply ends with, so a later apply re-checks and touches nothing. The
	// pool Close runs before the deferred managed shutdown (defers are LIFO), so it is
	// released while Postgres is still up.
	data, err := pg.Connect(ctx, src)
	if err != nil {
		return InstallReport{}, fmt.Errorf("daemon: connect data database for install: %w", err)
	}
	defer data.Close()
	if err := pg.EnsureJournal(ctx, data); err != nil {
		return InstallReport{}, fmt.Errorf("daemon: ensure data journal for install: %w", err)
	}
	logger.Info("engine install: ensured data journal")

	// Set up the control socket (the engine home, clearing any stale
	// socket) -- real local filesystem I/O, no database.
	if err := PrepareSocketDir(s); err != nil {
		return InstallReport{}, fmt.Errorf("daemon: set up control socket for install: %w", err)
	}
	logger.Info("engine install: control socket ready")

	// Mint the engine key at install and persist it into the engine_key meta table: a
	// create-once INSERT (ON CONFLICT DO NOTHING) on the meta connection,
	// superuser-free -- superseding the per-database GUC (needs SUPERUSER) and the
	// workspace key file (needs a shared filesystem for HA). Re-running install never
	// overwrites an existing key (the conflict is a no-op); the report's public half
	// is read back from meta so a re-install reports the stored key's public half, not
	// the discarded fresh mint. The INSERT carries the private half, so it is issued
	// directly and never logged.
	minted, err := MintEngineKey()
	if err != nil {
		return InstallReport{}, fmt.Errorf("daemon: mint engine key for install: %w", err)
	}
	if err := conns.Meta().Exec(ctx, InsertEngineKeyDDL(minted)); err != nil {
		return InstallReport{}, fmt.Errorf("daemon: store engine key for install: %w", err)
	}
	stored, err := conns.ReadEngineKey(ctx)
	if err != nil {
		return InstallReport{}, fmt.Errorf("daemon: read engine key after install: %w", err)
	}
	key, err := DecodeEngineKeyBytes(stored)
	if err != nil {
		return InstallReport{}, fmt.Errorf("daemon: decode stored engine key: %w", err)
	}
	report.EngineKeyPublic = key.PublicBase64()
	logger.Info("engine install: engine key ready")

	return report, nil
}

// MetaProbe answers whether the dedicated meta database already exists, so
// BootstrapEngine issues CREATE DATABASE only when it does not (CREATE DATABASE has
// no IF NOT EXISTS). It runs on the admin/maintenance connection; the query it
// issues is store.MetaExistsQuery.
type MetaProbe interface {
	// MetaExists reports whether the meta database already exists in the cluster.
	MetaExists(ctx context.Context) (bool, error)
}

// SocketPreparer sets up the control socket location (the engine home
// directory, clearing any stale socket). FileSocketPreparer is the production
// implementation; the install-sequence test records the step through a fake.
type SocketPreparer interface {
	// PrepareSocket creates the socket directory and removes a stale socket file.
	PrepareSocket(ctx context.Context) error
}

// InstallDeps bundles the seams `iris engine install` orchestrates over. Probe,
// Cluster, Meta, and Data are the connection seams (the pgx-backed ones land with
// the live-connection wiring; recording fakes drive them in tests); Key is the
// (optional) ed25519 engine key (minted inside if not supplied, at install time);
// Socket sets up the control socket; Logger receives progress diagnostics and must
// never be handed the key.
type InstallDeps struct {
	// Probe answers the meta-existence guard (admin/maintenance connection).
	Probe MetaProbe
	// Cluster runs CREATE DATABASE meta (admin/maintenance connection).
	Cluster store.Execer
	// Meta ensures the control tables and stores the engine key (meta connection).
	Meta store.Execer
	// Data creates the partitioned journal (data connection).
	Data pg.DB
	// Key is optional: if the zero value, a fresh ed25519 key is minted inside
	// (minted at engine install time for signing checkpoints).
	Key EngineKey
	// Socket sets up the control socket location.
	Socket SocketPreparer
	// Logger receives progress diagnostics. It is never handed the engine key.
	Logger *slog.Logger
}

// InstallReport is the non-secret outcome of BootstrapEngine: whether meta had to
// be created and the engine key's public half. It never carries the private key.
type InstallReport struct {
	// MetaCreated reports whether the meta database was created (it was missing),
	// as opposed to already existing.
	MetaCreated bool
	// EngineKeyPublic is the base64 public half of the minted engine key.
	EngineKeyPublic string
}

// BootstrapEngine performs the `iris engine install` bootstrap over deps: probe for
// meta and create it if missing, ensure its tables, create the partitioned journal
// on the data connection, store the engine key on the meta connection, and set up
// the socket -- in that order. It never logs the engine key: only the
// meta-connection statement that persists it carries the private half, and that
// statement is never handed to the logger.
func BootstrapEngine(ctx context.Context, deps InstallDeps) (InstallReport, error) {
	if err := deps.validate(); err != nil {
		return InstallReport{}, err
	}
	log := deps.Logger
	if log == nil {
		log = slog.New(slog.NewTextHandler(io.Discard, nil))
	}

	var report InstallReport

	// Mint the engine key at install time if none was supplied.
	// The private half is stored in meta; only the public is reported.
	key := deps.Key
	if !key.valid() {
		var err error
		key, err = MintEngineKey()
		if err != nil {
			return report, fmt.Errorf("daemon: mint engine key at install: %w", err)
		}
	}

	exists, err := deps.Probe.MetaExists(ctx)
	if err != nil {
		return report, fmt.Errorf("daemon: probe meta database: %w", err)
	}
	if !exists {
		// CREATE DATABASE has no IF NOT EXISTS; create it only when the probe says it
		// is missing. It runs on the admin/maintenance connection (you cannot create
		// meta from a connection to meta).
		if err := deps.Cluster.Exec(ctx, store.CreateMetaDatabaseDDL()); err != nil {
			return report, fmt.Errorf("daemon: create meta database: %w", err)
		}
		report.MetaCreated = true
		log.Info("engine install: created meta database", "database", store.MetaDatabase)
	} else {
		log.Info("engine install: meta database already exists", "database", store.MetaDatabase)
	}

	// Ensure the control tables (idempotent, create-if-missing) on the meta
	// connection.
	if err := store.EnsureSchema(ctx, deps.Meta); err != nil {
		return report, fmt.Errorf("daemon: ensure meta schema: %w", err)
	}
	log.Info("engine install: ensured meta schema")

	// Create the partitioned public.data_journal on the data connection.
	if err := pg.EnsureJournal(ctx, deps.Data); err != nil {
		return report, fmt.Errorf("daemon: ensure data journal: %w", err)
	}
	log.Info("engine install: ensured data journal")

	// Store the engine key on the meta connection: a create-once INSERT into the
	// single-row engine_key table (ON CONFLICT DO NOTHING). The statement carries the
	// private half, so it is issued directly and never logged.
	if err := deps.Meta.Exec(ctx, InsertEngineKeyDDL(key)); err != nil {
		return report, fmt.Errorf("daemon: store engine key: %w", err)
	}
	log.Info("engine install: stored engine key")

	// Set up the control socket.
	if err := deps.Socket.PrepareSocket(ctx); err != nil {
		return report, fmt.Errorf("daemon: set up control socket: %w", err)
	}
	log.Info("engine install: control socket ready")

	report.EngineKeyPublic = key.PublicBase64()
	return report, nil
}

// validate rejects a partially-built InstallDeps before any statement is issued,
// so a missing seam surfaces as a clear error rather than a nil dereference.
func (d InstallDeps) validate() error {
	switch {
	case d.Probe == nil:
		return errors.New("daemon: install requires a meta-existence probe")
	case d.Cluster == nil:
		return errors.New("daemon: install requires a cluster connection")
	case d.Meta == nil:
		return errors.New("daemon: install requires a meta connection")
	case d.Data == nil:
		return errors.New("daemon: install requires a data connection")
	case d.Socket == nil:
		return errors.New("daemon: install requires a socket preparer")
		// Key is optional: BootstrapEngine mints a fresh ed25519 key if not supplied
		// (the key used to sign checkpoints is minted at engine install time).
	}
	return nil
}
