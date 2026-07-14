package daemon

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"

	"github.com/MateusAMP2119/iris-engine-cli/internal/config"
	"github.com/MateusAMP2119/iris-engine-cli/internal/pg"
	"github.com/MateusAMP2119/iris-engine-cli/internal/store"
)

// This file wires the full `iris engine uninstall` teardown: drop the meta database
// (all captured provenance, endpoints, and access ledger with it), drop the data
// journal and its dependent triggers on the data connection, delete the object
// store under objects_path (artifact bytes and archived partitions), and remove the
// control socket and the service unit -- leaving nothing behind. It is a
// daemonless, local-machine-only teardown.
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
// still holds a meta connection: the shared meta database is never dropped under a
// live candidate. The predicate that detects this is a seam a later task fills; the
// default predicate proceeds.
var ErrLiveCandidate = errors.New("daemon: refusing engine uninstall while a daemon candidate holds a meta connection")

// LiveCandidatePredicate reports whether any daemon candidate currently holds a
// meta connection, so uninstall can refuse rather than drop meta out from under a
// live candidate. It is still an open seam: no predicate backed by the
// leadership/liveness wiring exists, so ProceedWithoutLiveCheck -- which always
// reports no live candidate -- remains the only implementation and the one the CLI
// uses.
type LiveCandidatePredicate interface {
	// LiveCandidateHoldsMeta reports whether a daemon candidate holds a meta
	// connection right now.
	LiveCandidateHoldsMeta(ctx context.Context) (bool, error)
}

// ProceedWithoutLiveCheck returns the default live-candidate predicate: it reports
// no live candidate, so uninstall proceeds. It is the documented default a real
// meta-connection check over the leadership/liveness wiring would replace; nothing
// replaces it today, so the guard is open.
func ProceedWithoutLiveCheck() LiveCandidatePredicate { return proceedPredicate{} }

// proceedPredicate is the default LiveCandidatePredicate: it always reports no live
// candidate.
type proceedPredicate struct{}

// LiveCandidateHoldsMeta always reports no live candidate (proceed).
func (proceedPredicate) LiveCandidateHoldsMeta(context.Context) (bool, error) { return false, nil }

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
	// Removed lists the on-disk paths removed (object store, socket, service unit).
	Removed []string
}

// UninstallEngine performs the full `iris engine uninstall` teardown over deps:
// refuse if a daemon candidate holds meta, then drop the meta database, drop the
// journal on the data connection, and delete the object store, socket, and service
// unit on disk. It returns ErrLiveCandidate unchanged when the guard refuses, so
// the caller can surface its guidance.
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

	// Delete the object store, socket, and service unit on disk.
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
// <workspace>/.iris/iris.service. See ServiceUnitName for why this path is the
// convention service install and engine uninstall share.
func ServiceUnitPath(s config.Settings) string {
	return filepath.Join(irisDir(s), ServiceUnitName)
}

// RemoveEngineArtifacts deletes the engine's on-disk state for the settings: the
// object store directory under objects_path (its artifact bytes and archived
// partitions), the control socket, and the service unit -- each removed only if
// present. It returns the paths it removed, in that order, and stops at the first
// hard error. It is the real, daemonless filesystem half of uninstall, shared by
// UninstallEngine and the CLI.
func RemoveEngineArtifacts(s config.Settings) ([]string, error) {
	var removed []string

	// Object store: a directory tree of plain files (artifact bytes + archived
	// partitions); remove it whole.
	if s.ObjectsPath != "" {
		switch _, err := os.Stat(s.ObjectsPath); {
		case err == nil:
			if err := os.RemoveAll(s.ObjectsPath); err != nil {
				return removed, fmt.Errorf("daemon: remove object store %s: %w", s.ObjectsPath, err)
			}
			removed = append(removed, s.ObjectsPath)
		case !errors.Is(err, os.ErrNotExist):
			return removed, fmt.Errorf("daemon: inspect object store %s: %w", s.ObjectsPath, err)
		}
	}

	for _, path := range []string{s.Socket, ServiceUnitPath(s)} {
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
