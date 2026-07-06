package daemon

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"

	"github.com/MateusAMP2119/iris-engine-cli/internal/pg"
	"github.com/MateusAMP2119/iris-engine-cli/internal/store"
)

// This file wires the full `iris engine install` bootstrap over the pieces the
// earlier E02 tasks built (specification section 4, bootstrap Q/A): probe for the
// meta database and create it with a plain CREATE DATABASE if missing, ensure its
// tables, create the partitioned public.data_journal on the data connection, store
// the minted ed25519 engine key on the meta connection, and set up the socket.
//
// BootstrapEngine is the orchestration; it composes store.Bootstrap/EnsureSchema,
// pg.EnsureJournal, the engine key (enginekey.go), and the socket setup
// (socket.go) over injected seams, so the whole sequence is proven with recording
// fakes and no live Postgres. The seams are exactly the E02.1/E02.2 connection
// seams (store.Execer, pg.DB) plus the small MetaProbe and SocketPreparer here;
// the pgx-backed implementations of the DB seams land with the daemon's live
// connection wiring, at which point the CLI drives this same function against a
// real cluster.

// MetaProbe answers whether the dedicated meta database already exists, so
// BootstrapEngine issues CREATE DATABASE only when it does not (CREATE DATABASE has
// no IF NOT EXISTS). It runs on the admin/maintenance connection; the query it
// issues is store.MetaExistsQuery.
type MetaProbe interface {
	// MetaExists reports whether the meta database already exists in the cluster.
	MetaExists(ctx context.Context) (bool, error)
}

// SocketPreparer sets up the control socket location (the workspace .iris
// directory, clearing any stale socket). FileSocketPreparer is the production
// implementation; the install-sequence test records the step through a fake.
type SocketPreparer interface {
	// PrepareSocket creates the socket directory and removes a stale socket file.
	PrepareSocket(ctx context.Context) error
}

// InstallDeps bundles the seams `iris engine install` orchestrates over. Probe,
// Cluster, Meta, and Data are the connection seams (the pgx-backed ones land with
// the live-connection wiring; recording fakes drive them in tests); Key is the
// freshly minted engine key; Socket sets up the control socket; Logger receives
// progress diagnostics and must never be handed the key.
type InstallDeps struct {
	// Probe answers the meta-existence guard (admin/maintenance connection).
	Probe MetaProbe
	// Cluster runs CREATE DATABASE meta (admin/maintenance connection).
	Cluster store.Execer
	// Meta ensures the control tables and stores the engine key (meta connection).
	Meta store.Execer
	// Data creates the partitioned journal (data connection).
	Data pg.DB
	// Key is the minted ed25519 engine key whose private half is stored in meta.
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

// BootstrapEngine performs the `iris engine install` bootstrap over deps
// (specification section 4): probe for meta and create it if missing, ensure its
// tables, create the partitioned journal on the data connection, store the engine
// key on the meta connection, and set up the socket -- in that order. It never
// logs the engine key: only the meta-connection statement that persists it carries
// the private half, and that statement is never handed to the logger.
func BootstrapEngine(ctx context.Context, deps InstallDeps) (InstallReport, error) {
	if err := deps.validate(); err != nil {
		return InstallReport{}, err
	}
	log := deps.Logger
	if log == nil {
		log = slog.New(slog.NewTextHandler(io.Discard, nil))
	}

	var report InstallReport

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

	// Store the engine key on the meta connection. The DDL carries the private half,
	// so it is issued directly and never logged.
	if err := deps.Meta.Exec(ctx, SetEngineKeyDDL(deps.Key)); err != nil {
		return report, fmt.Errorf("daemon: store engine key: %w", err)
	}
	log.Info("engine install: stored engine key")

	// Set up the control socket.
	if err := deps.Socket.PrepareSocket(ctx); err != nil {
		return report, fmt.Errorf("daemon: set up control socket: %w", err)
	}
	log.Info("engine install: control socket ready")

	report.EngineKeyPublic = deps.Key.PublicBase64()
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
	case !d.Key.valid():
		return errors.New("daemon: install requires a minted engine key")
	}
	return nil
}
