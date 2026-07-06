package daemon

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"os"
	osexec "os/exec"
	"syscall"
	"time"

	"github.com/MateusAMP2119/iris-engine-cli/internal/api"
	"github.com/MateusAMP2119/iris-engine-cli/internal/config"
)

// This file is the daemon's foreground/detached lifecycle at the process edge
// (specification section 2): a foreground daemon (Run) that serves the listeners
// and blocks until signalled; the detach re-exec (Detach) that backgrounds a
// session-leader copy of the binary with its output redirected to the daemon
// log; and the minimal stop (StopDaemon) that signals a detached daemon by its
// recorded pid. The listener wiring is server.go; this owns only the lifecycle.

// DaemonizedEnv marks the re-exec'd child of a `--detach` start: the child sees
// it set and runs in the foreground (attached to its new session), so a detach
// never recurses. It is set by Detach on the child's environment.
const DaemonizedEnv = "IRIS_DAEMONIZED"

// detachReadyPollBackoff caps the backoff between socket-reachability probes while
// a detach waits for its child to come up. Readiness is a successful dial, never
// elapsed time; the backoff only keeps the poll from spinning.
const detachReadyPollBackoff = 200 * time.Millisecond

// ErrManagedNotInstalled is returned by Run when the engine runs in managed-
// Postgres mode but the managed build has not been installed yet. `iris engine
// start` maps it to a fail-fast with install guidance (specification section 2:
// managed mode needs `iris engine install` first).
var ErrManagedNotInstalled = errors.New("daemon: the engine's managed Postgres is not installed; run \"iris engine install\" first")

// IsManagedInstalled reports whether the managed Postgres has been installed for
// these settings: its data directory records a Postgres major version (initdb has
// run under <workspace>/.iris/pg). It is the guard Run uses to fail fast rather
// than start a managed engine with no database to manage.
func IsManagedInstalled(s config.Settings) bool {
	v, err := ReadDataDirMajorVersion(managedDataDir(ManagedPGDir(s)))
	return err == nil && v > 0
}

// Run starts the daemon in the foreground: it serves the control/read API on the
// always-on unix socket and, when configured, the PAT-gated TCP listener, records
// the pidfile, logs a ready line, and blocks until ctx is cancelled
// (SIGTERM/SIGINT), then shuts down gracefully (specification section 2:
// foreground default, streaming). In managed mode it fails fast when the managed
// Postgres is not installed; it does not itself start Postgres here (managed-PG
// startup and meta connectivity land in E02.6).
func Run(ctx context.Context, s config.Settings, logger *slog.Logger) error {
	if logger == nil {
		logger = slog.New(slog.NewTextHandler(io.Discard, nil))
	}
	if s.Managed() && !IsManagedInstalled(s) {
		return ErrManagedNotInstalled
	}

	srv := NewServer(s, api.NewMux(), WithServerLogger(logger))
	if err := srv.Start(ctx); err != nil {
		return err
	}
	if err := WritePIDFile(s, os.Getpid()); err != nil {
		_ = srv.Shutdown()
		return err
	}
	defer func() { _ = RemovePIDFile(s) }()

	logger.Info("iris daemon listening",
		"socket", srv.SocketPath(), "tcp", srv.TCPAddr(), "mode", modeLabel(s))
	return srv.Serve(ctx)
}

// Detach re-execs the binary at exePath with childArgs as a background,
// session-leading daemon whose stdout and stderr are redirected to the workspace
// daemon log, then waits until the daemon's socket is reachable before returning
// (specification section 2: -d detaches; the daemon survives the CLI's exit).
// The child inherits DaemonizedEnv so it runs in the foreground of its new
// session and never detaches again. The parent does not reap the child: it
// outlives the CLI.
func Detach(ctx context.Context, s config.Settings, exePath string, childArgs []string) error {
	logFile, err := OpenDaemonLog(s)
	if err != nil {
		return err
	}
	defer func() { _ = logFile.Close() }()

	// exePath is this binary's own path (os.Executable) and childArgs are the CLI's
	// own args minus -d: re-execing ourselves as a background daemon.
	cmd := osexec.Command(exePath, childArgs...)
	cmd.Env = append(os.Environ(), DaemonizedEnv+"=1")
	cmd.Stdin = nil
	cmd.Stdout = logFile
	cmd.Stderr = logFile
	cmd.SysProcAttr = detachedSysProcAttr()
	if err := cmd.Start(); err != nil {
		return fmt.Errorf("daemon: start detached daemon: %w", err)
	}
	// Do not Wait: the daemon outlives this process. Release our reference so the
	// child is reparented to init when we exit.
	if err := cmd.Process.Release(); err != nil {
		return fmt.Errorf("daemon: release detached daemon: %w", err)
	}

	if err := waitSocketReachable(ctx, s.Socket); err != nil {
		return fmt.Errorf("daemon: detached daemon did not become reachable: %w", err)
	}
	return nil
}

// StopDaemon signals the daemon with the given pid to shut down gracefully
// (SIGTERM), waits until the process is gone, and escalates to SIGKILL if the
// grace deadline (ctx) passes. It removes the pidfile once the daemon is gone
// (specification section 2: engine stop stops a detached daemon).
func StopDaemon(ctx context.Context, s config.Settings, pid int) error {
	proc, err := os.FindProcess(pid)
	if err != nil {
		return fmt.Errorf("daemon: find process %d: %w", pid, err)
	}
	if err := proc.Signal(syscall.SIGTERM); err != nil {
		if errors.Is(err, os.ErrProcessDone) {
			return RemovePIDFile(s)
		}
		return fmt.Errorf("daemon: signal process %d: %w", pid, err)
	}

	backoff := 10 * time.Millisecond
	for {
		if !processAlive(pid) {
			return RemovePIDFile(s)
		}
		select {
		case <-ctx.Done():
			// Grace elapsed: force the daemon down, then reap the pidfile.
			_ = proc.Kill()
			return RemovePIDFile(s)
		case <-time.After(backoff):
		}
		if backoff < detachReadyPollBackoff {
			backoff *= 2
		}
	}
}

// processAlive reports whether a process with pid is still running, probed with
// the null signal (signal 0 delivers nothing but validates the target exists).
func processAlive(pid int) bool {
	proc, err := os.FindProcess(pid)
	if err != nil {
		return false
	}
	return proc.Signal(syscall.Signal(0)) == nil
}

// waitSocketReachable polls until the unix socket at path accepts a connection or
// ctx is done. Readiness is decided by a successful dial, never elapsed time; the
// backoff only keeps the loop from spinning.
func waitSocketReachable(ctx context.Context, path string) error {
	var dialer net.Dialer
	backoff := 5 * time.Millisecond
	for {
		conn, err := dialer.DialContext(ctx, "unix", path)
		if err == nil {
			return conn.Close()
		}
		select {
		case <-ctx.Done():
			return fmt.Errorf("socket %s never became ready: %w", path, ctx.Err())
		case <-time.After(backoff):
		}
		if backoff < detachReadyPollBackoff {
			backoff *= 2
		}
	}
}

// modeLabel names the Postgres mode for the daemon's ready line.
func modeLabel(s config.Settings) string {
	if s.Managed() {
		return "managed"
	}
	return "external"
}
