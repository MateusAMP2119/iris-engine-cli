package daemon

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/MateusAMP2119/iris-lakehouse/internal/config"
)

// This file fixes the runtime paths under the engine home (the directory the
// control socket sits in, ~/.iris by default) that the daemon owns: the log
// directory and its daemon.log, the per-run run-<id>.log naming convention, and
// the pidfile a detached daemon records so `iris engine stop` can address it.
// The socket, iris.toml, objects, and managed-Postgres paths live in config and
// managedpg.go; these are the remaining engine-home leaves.

const (
	// logsDirName is the log subdirectory under the engine home.
	logsDirName = "logs"
	// daemonLogName is the daemon's own log file under the logs directory.
	daemonLogName = "daemon.log"
	// pidFileName is the file a detached daemon records its pid in, under the engine home.
	pidFileName = "iris.pid"
	// logsDirPerm mirrors the owner-only engine home: logs may name pipeline output
	// paths, so the directory stays private to the engine owner.
	logsDirPerm os.FileMode = 0o700
	// logFilePerm is the mode of the daemon log file: owner read/write.
	logFilePerm os.FileMode = 0o600
	// pidFilePerm is the mode of the pidfile: owner read/write.
	pidFilePerm os.FileMode = 0o600
)

// LogsDir returns the daemon's log directory, <engine home>/logs, derived from
// the engine home the control socket sits in.
func LogsDir(s config.Settings) string {
	return filepath.Join(irisDir(s), logsDirName)
}

// LogPath returns the daemon log path, <engine home>/logs/daemon.log: where the
// detached daemon's stdout/stderr are redirected.
func LogPath(s config.Settings) string {
	return filepath.Join(LogsDir(s), daemonLogName)
}

// RunLogPath returns a per-run log path, <engine home>/logs/run-<id>.log, the
// run-id-keyed naming convention run output follows (runs.log_ref). The run logs
// themselves are written by the dispatcher (E05); this fixes the path convention
// the whole engine shares.
func RunLogPath(s config.Settings, runID string) string {
	return filepath.Join(LogsDir(s), "run-"+runID+".log")
}

// PIDPath returns the pidfile path, <engine home>/iris.pid, that a detached
// daemon records its process id in so `iris engine stop` can signal it.
func PIDPath(s config.Settings) string {
	return filepath.Join(irisDir(s), pidFileName)
}

// EnsureLogsDir creates the log directory if absent, owner-only. It is
// idempotent.
func EnsureLogsDir(s config.Settings) error {
	dir := LogsDir(s)
	if err := os.MkdirAll(dir, logsDirPerm); err != nil {
		return fmt.Errorf("daemon: create logs directory %s: %w", dir, err)
	}
	return nil
}

// OpenDaemonLog ensures the log directory and opens (creating/appending) the
// daemon log for writing, so a detached daemon's output lands under the engine
// home's logs directory.
// The caller closes the returned file.
func OpenDaemonLog(s config.Settings) (*os.File, error) {
	if err := EnsureLogsDir(s); err != nil {
		return nil, err
	}
	path := LogPath(s)
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, logFilePerm) //nolint:gosec // G304: path is the engine-owned daemon log under the engine home, not user or network input.
	if err != nil {
		return nil, fmt.Errorf("daemon: open daemon log %s: %w", path, err)
	}
	return f, nil
}

// WritePIDFile records pid in the engine-home pidfile so a detached daemon can
// be stopped by `iris engine stop`. It creates the engine home if absent.
func WritePIDFile(s config.Settings, pid int) error {
	if err := os.MkdirAll(irisDir(s), socketDirPerm); err != nil {
		return fmt.Errorf("daemon: create engine home for pidfile: %w", err)
	}
	path := PIDPath(s)
	if err := os.WriteFile(path, []byte(strconv.Itoa(pid)+"\n"), pidFilePerm); err != nil {
		return fmt.Errorf("daemon: write pidfile %s: %w", path, err)
	}
	return nil
}

// ReadPIDFile reads the pid a detached daemon recorded. A missing pidfile is an
// error (no detached daemon has been started for this engine).
func ReadPIDFile(s config.Settings) (int, error) {
	path := PIDPath(s)
	raw, err := os.ReadFile(path) //nolint:gosec // G304: path is the engine-owned pidfile under the engine home, not user or network input.
	if err != nil {
		return 0, fmt.Errorf("daemon: read pidfile %s: %w", path, err)
	}
	pid, err := strconv.Atoi(strings.TrimSpace(string(raw)))
	if err != nil {
		return 0, fmt.Errorf("daemon: pidfile %s does not hold a valid pid: %w", path, err)
	}
	return pid, nil
}

// RemovePIDFile deletes the engine-home pidfile. An absent pidfile is not an error.
func RemovePIDFile(s config.Settings) error {
	if err := os.Remove(PIDPath(s)); err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("daemon: remove pidfile %s: %w", PIDPath(s), err)
	}
	return nil
}

// requireWorkspaceTree enforces the per-host prerequisite:
// a daemon candidate started on a host lacking the workspace tree the leader dispatches
// from (pipeline folders, dev source, env_files) refuses to start. The check is performed
// with the resolved CWD early in Run so no managed PG or listeners are started for an
// impossible candidate.
func requireWorkspaceTree(workspace string) error {
	fi, err := os.Stat(workspace)
	if err != nil {
		return fmt.Errorf("daemon: workspace tree %s missing or inaccessible; candidate refuses to start: %w", workspace, err)
	}
	if !fi.IsDir() {
		return fmt.Errorf("daemon: workspace tree %s is not a directory; candidate refuses to start", workspace)
	}
	return nil
}
