package daemon

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"

	"github.com/MateusAMP2119/iris-lakehouse/internal/config"
	"github.com/MateusAMP2119/iris-lakehouse/internal/pg"
	"github.com/MateusAMP2119/iris-lakehouse/internal/store"
)

// This file wires the full `iris engine uninstall` teardown: drop the meta database
// (all captured provenance, endpoints, and access ledger with it), drop the data
// journal and its dependent triggers on the data connection, delete the object
// store under objects_path (artifact bytes and archived partitions), the managed
// Postgres tree (binaries and the data directory, taking the meta and data
// databases with it on a managed install), and the log directory, and remove the
// control socket, the service unit, and the pidfile -- leaving nothing behind. It
// is a daemonless, local-machine-only teardown.
//
// The database drops go through the same connection seams install uses (store.Execer,
// pg.DB); the filesystem teardown is real. UninstallEngine composes both. The CLI
// performs the filesystem half directly today (RemoveEngineArtifacts) and drives
// the database half through this function once the daemon's live admin connection
// is wired; the object-store, socket, and service-unit removal is real from now.

// ServiceUnitName is the filename of the local service unit under the workspace
// .iris directory. It is the convention the two halves agree on: `iris engine
// service install` (service.go) generates the real platform unit (systemd/launchd)
// and may place it elsewhere when given an explicit path, but its default target is
// this workspace-local path -- the single location uninstall removes, so the two
// never disagree on where the unit lives.
const ServiceUnitName = "iris.service"

// ErrLiveCandidate is returned when uninstall refuses because a daemon candidate
// is still live: engine state is never torn down out from under a running
// candidate. The CLI backs the check with its daemon probe and the pidfile
// predicate (PIDFileLiveCheck).
var ErrLiveCandidate = errors.New("daemon: refusing engine uninstall while a daemon candidate is running")

// LiveCandidatePredicate reports whether any daemon candidate currently holds a
// meta connection, so uninstall can refuse rather than tear the engine down out
// from under a live candidate. The CLI composes two live implementations: its
// daemon probe (GET /healthz over the resolved socket/TCP target, catching any
// serving daemon) and PIDFileLiveCheck below (catching a detached daemon whose
// listener is wedged or still starting). ProceedWithoutLiveCheck remains for
// compositions that deliberately skip the check.
type LiveCandidatePredicate interface {
	// LiveCandidateHoldsMeta reports whether a daemon candidate holds a meta
	// connection right now.
	LiveCandidateHoldsMeta(ctx context.Context) (bool, error)
}

// ProceedWithoutLiveCheck returns the always-open live-candidate predicate: it
// reports no live candidate, so uninstall proceeds. It exists for compositions
// that deliberately skip the check; the CLI's uninstall path uses the real
// probes instead.
func ProceedWithoutLiveCheck() LiveCandidatePredicate { return proceedPredicate{} }

// proceedPredicate is the always-open LiveCandidatePredicate: it always reports no
// live candidate.
type proceedPredicate struct{}

// LiveCandidateHoldsMeta always reports no live candidate (proceed).
func (proceedPredicate) LiveCandidateHoldsMeta(context.Context) (bool, error) { return false, nil }

// PIDFileLiveCheck returns a LiveCandidatePredicate over the workspace pidfile: a
// live candidate is reported when the pidfile a detached daemon recorded names a
// process that is still running. It catches the daemon the socket probe can miss
// -- one mid-start (socket not yet bound) or wedged (listener dead, process
// alive) -- and reports nothing when no pidfile exists (no detached daemon) or
// the recorded process is gone (a stale pidfile never blocks an uninstall).
func PIDFileLiveCheck(s config.Settings) LiveCandidatePredicate {
	return pidfilePredicate{settings: s}
}

// pidfilePredicate is the pidfile-backed LiveCandidatePredicate.
type pidfilePredicate struct{ settings config.Settings }

// LiveCandidateHoldsMeta reports whether the workspace pidfile names a running
// process.
func (p pidfilePredicate) LiveCandidateHoldsMeta(context.Context) (bool, error) {
	pid, err := ReadPIDFile(p.settings)
	if err != nil {
		return false, nil // no (or unreadable) pidfile: no detached daemon recorded
	}
	return processAlive(pid), nil
}

// UninstallDeps bundles the seams `iris engine uninstall` orchestrates over.
type UninstallDeps struct {
	// LiveCandidate gates the teardown: uninstall refuses if a candidate holds meta.
	// A nil predicate proceeds (equivalent to ProceedWithoutLiveCheck).
	LiveCandidate LiveCandidatePredicate
	// Cluster runs DROP DATABASE meta (admin/maintenance connection).
	Cluster store.Execer
	// Data drops the journal and its dependents (data connection).
	Data pg.DB
	// Settings carries the objects_path, socket, and workspace paths removed on disk.
	Settings config.Settings
	// Logger receives progress diagnostics.
	Logger *slog.Logger
}

// UninstallReport is the outcome of UninstallEngine: which database drops ran and
// the on-disk paths removed.
type UninstallReport struct {
	// MetaDropped reports whether DROP DATABASE meta was issued.
	MetaDropped bool
	// JournalDropped reports whether the journal teardown was issued.
	JournalDropped bool
	// Removed lists the on-disk paths removed (object store, managed Postgres
	// tree, log directory, socket, service unit, pidfile).
	Removed []string
}

// UninstallEngine performs the full `iris engine uninstall` teardown over deps:
// refuse if a daemon candidate holds meta, then drop the meta database, drop the
// journal on the data connection, and delete the on-disk engine state (the object
// store, the managed Postgres tree, the log directory, the socket, the service
// unit, and the pidfile). It returns ErrLiveCandidate unchanged when the guard
// refuses, so the caller can surface its guidance.
func UninstallEngine(ctx context.Context, deps UninstallDeps) (UninstallReport, error) {
	log := deps.Logger
	if log == nil {
		log = slog.New(slog.NewTextHandler(io.Discard, nil))
	}
	var report UninstallReport

	if deps.LiveCandidate != nil {
		held, err := deps.LiveCandidate.LiveCandidateHoldsMeta(ctx)
		if err != nil {
			return report, fmt.Errorf("daemon: check for a live daemon candidate: %w", err)
		}
		if held {
			return report, ErrLiveCandidate
		}
	}

	// Drop meta on the admin/maintenance connection: every control table, endpoint,
	// and provenance row goes with the database.
	if deps.Cluster != nil {
		if err := deps.Cluster.Exec(ctx, store.DropMetaDatabaseDDL()); err != nil {
			return report, fmt.Errorf("daemon: drop meta database: %w", err)
		}
		report.MetaDropped = true
		log.Info("engine uninstall: dropped meta database", "database", store.MetaDatabase)
	}

	// Drop the journal (and its dependents, via the cascade) on the data connection.
	if deps.Data != nil {
		for _, stmt := range pg.JournalTeardownDDL() {
			if err := deps.Data.Exec(ctx, stmt); err != nil {
				return report, fmt.Errorf("daemon: drop data journal: %w", err)
			}
		}
		report.JournalDropped = true
		log.Info("engine uninstall: dropped data journal")
	}

	// Delete the on-disk engine state.
	removed, err := RemoveEngineArtifacts(deps.Settings)
	report.Removed = removed
	if err != nil {
		return report, err
	}
	for _, path := range removed {
		log.Info("engine uninstall: removed", "path", path)
	}
	return report, nil
}

// ServiceUnitPath returns the workspace-local service-unit path for the settings:
// <engine home>/iris.service. See ServiceUnitName for why this path is the
// convention service install and engine uninstall share.
func ServiceUnitPath(s config.Settings) string {
	return filepath.Join(irisDir(s), ServiceUnitName)
}

// RemoveEngineArtifacts deletes the engine's on-disk state for the settings: the
// object store directory under objects_path (its artifact bytes and archived
// partitions), the managed Postgres tree (binaries and the data directory -- on a
// managed install the meta and data databases go with it; absent on an
// external-cluster install), the log directory, the control socket, the service
// unit, and the pidfile -- each removed only if present. It returns the paths it
// removed, in that order, and stops at the first hard error. It is the real,
// daemonless filesystem half of uninstall, shared by UninstallEngine and the CLI.
// The live-candidate guard runs before it, so the managed cluster and the pidfile
// are never removed out from under a running daemon.
func RemoveEngineArtifacts(s config.Settings) ([]string, error) {
	var removed []string

	// Directory trees, removed whole: the object store (artifact bytes + archived
	// partitions), the managed Postgres tree (binaries + data directory), and the
	// log directory. The engine-home-derived paths (pg, logs, pidfile) are
	// resolved from the socket's directory, so they are skipped when no socket is
	// configured rather than resolving relative to the working directory.
	dirs := []string{s.ObjectsPath}
	files := []string{s.Socket, ServiceUnitPath(s)}
	if s.Socket != "" {
		dirs = append(dirs, ManagedPGDir(s), LogsDir(s))
		files = append(files, PIDPath(s))
	}
	for _, dir := range dirs {
		if dir == "" {
			continue
		}
		switch _, err := os.Stat(dir); {
		case err == nil:
			if err := os.RemoveAll(dir); err != nil {
				return removed, fmt.Errorf("daemon: remove %s: %w", dir, err)
			}
			removed = append(removed, dir)
		case !errors.Is(err, os.ErrNotExist):
			return removed, fmt.Errorf("daemon: inspect %s: %w", dir, err)
		}
	}

	for _, path := range files {
		if path == "" {
			continue
		}
		if err := os.Remove(path); err != nil {
			if errors.Is(err, os.ErrNotExist) {
				continue
			}
			return removed, fmt.Errorf("daemon: remove %s: %w", path, err)
		}
		removed = append(removed, path)
	}
	return removed, nil
}
