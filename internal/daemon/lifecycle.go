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
	"github.com/MateusAMP2119/iris-engine-cli/internal/dispatch"
	"github.com/MateusAMP2119/iris-engine-cli/internal/exec"
	"github.com/MateusAMP2119/iris-engine-cli/internal/pg"
	"github.com/MateusAMP2119/iris-engine-cli/internal/store"
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

// killConfirmTimeout bounds how long StopDaemon waits, after a SIGKILL
// escalation, to confirm the daemon has actually exited (and released its socket)
// before it reaps the pidfile. SIGKILL is fire-and-forget, so this is a short
// liveness poll, not a second grace period.
const killConfirmTimeout = 2 * time.Second

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

// Run starts the daemon in the foreground: it brings up Postgres (a managed
// subprocess or the external cluster), connects the meta client, serves the
// control/read API on the always-on unix socket and, when configured, the PAT-gated
// TCP listener, runs leader election, records the pidfile, logs a ready line, and
// blocks until ctx is cancelled (SIGTERM/SIGINT), then shuts down gracefully
// (specification section 2: foreground default, streaming). In managed mode it fails
// fast when the managed Postgres is not installed.
//
// Election runs alongside the listeners: the candidate contends for the leader
// advisory lock on a session-pinned meta connection, becomes the sole dispatcher on
// winning (re-checking the meta schema through the single-writer path), and reports
// the leader role so its listeners accept mutations; until then it is a standby that
// rejects mutations with leader guidance. This is where `iris engine start` becomes
// genuinely functional: it connects to a real database for the first time.
func Run(ctx context.Context, s config.Settings, logger *slog.Logger) error {
	if logger == nil {
		logger = slog.New(slog.NewTextHandler(io.Discard, nil))
	}
	if s.Managed() && !IsManagedInstalled(s) {
		return ErrManagedNotInstalled
	}

	// Bring up Postgres and resolve the admin DSN (managed subprocess or external),
	// then connect the meta client: ensure the meta database exists and open the
	// leader session (advisory lock + writes) and the reader pool.
	mgr := NewManager(s, EmbeddedSupervisor)
	adminDSN, err := mgr.Startup(ctx)
	if err != nil {
		return fmt.Errorf("daemon: bring up Postgres: %w", err)
	}
	defer func() { _ = mgr.Shutdown() }()

	client, err := store.Connect(ctx, adminDSN.Source())
	if err != nil {
		return fmt.Errorf("daemon: connect meta: %w", err)
	}
	defer func() { _ = client.Close(context.Background()) }()

	// The data-database client the control plane provisions schemas through: a pool on
	// the admin DSN's own database (where the declared tables and journal live), a peer
	// of the meta client store owns.
	data, err := pg.Connect(ctx, adminDSN.Source())
	if err != nil {
		return fmt.Errorf("daemon: connect data database: %w", err)
	}
	defer data.Close()

	// The leader's workspace tree: declarations and the schemas/ tree resolve against
	// it. The daemon runs in the workspace (its socket lives under <workspace>/.iris),
	// so its working directory is that tree.
	workspace, err := os.Getwd()
	if err != nil {
		return fmt.Errorf("daemon: resolve workspace: %w", err)
	}

	// The role state the mux consults and the control plane the mux routes apply/destroy
	// to: standby/unwired until election confirms leadership and installs the
	// orchestrator.
	role := api.NewRoleState()
	control := newControlPlane()
	// The pipeline plane serves iris pipeline list from the reader pool (any node) and,
	// once this daemon leads, POST /pipeline/run through the single writer and exec seam.
	pipelines := newPipelinePlane(client.PipelineLister(), logger)
	srv := NewServer(s, api.NewMux(api.WithRole(role), api.WithControl(control), api.WithPipelines(pipelines)), WithServerLogger(logger))
	if err := srv.Start(ctx); err != nil {
		return err
	}
	if err := WritePIDFile(s, os.Getpid()); err != nil {
		_ = srv.Shutdown()
		return err
	}
	// The pidfile removal is deliberately NOT deferred: a defer runs only at
	// function return, which is gated behind the election-lock release (<-electDone
	// below) and the deferred managed-Postgres and connection teardown -- seconds of
	// work on a loaded host. The pidfile is the workspace liveness marker, and the
	// daemon must disown it the moment it stops serving, so it is removed right after
	// srv.Serve returns (listeners drained, socket released), below.

	// Run the election in the background: it flips the role and drives the single
	// dispatcher. It returns when ctx is cancelled (having released the lock). On
	// winning, the leader runs startup crash reconciliation before any lane dispatch:
	// it reads leftover run records through the plain-MVCC reader, best-effort
	// SIGKILLs same-host survivors through the exec seam, and disposes of their runs
	// through the single writer (specification section 2 crash recovery).
	cand := NewCandidate(client.Lock(), role, client.WriteConn(), logger,
		WithReconciliation(client.Reader(), dispatch.RealGroupKiller(), dispatch.SingleHostMatcher()),
		WithControlPlane(control, workspace, client.RegistryReader(), client.AppliedHeadReader(), data),
		WithPipelinePlane(pipelines, workspace, client.RegistryReader(), client.ManualReader(), exec.NewOSRunner()))
	electDone := make(chan error, 1)
	go func() { electDone <- cand.Serve(ctx) }()

	logger.Info("iris daemon listening",
		"socket", srv.SocketPath(), "tcp", srv.TCPAddr(), "mode", modeLabel(s))
	serveErr := srv.Serve(ctx)

	// Serve returned because ctx was cancelled (SIGTERM/SIGINT): the listeners have
	// drained and the socket is released, so the daemon has stopped serving. Remove
	// the pidfile now -- promptly, before the election-lock release wait and the heavy
	// deferred teardown (managed Postgres, connection pools) -- because it is the
	// workspace liveness marker and a signalled daemon must disown it at once
	// (specification section 2: graceful shutdown). It is removed exactly once here,
	// not via defer: after Serve returns the socket is free, so a racing
	// `iris engine start` may bind and record its own pid while this daemon finishes
	// its slower teardown, and a deferred removal would then delete the successor's
	// pidfile. RemovePIDFile treats an absent file as success, so this stays clean.
	if err := RemovePIDFile(s); err != nil {
		logger.Warn("iris daemon shutdown: remove pidfile", "err", err)
	}

	// Record the graceful shutdown before the deferred managed-Postgres and
	// connection teardown run, so daemon.log carries a shutdown line for either signal
	// (specification section 2: graceful shutdown).
	logger.Info("iris daemon shut down", "socket", srv.SocketPath())

	// Wait for the candidate to release the leader lock (and demote) before the
	// deferred connection teardown runs. A clean shutdown returns nil, so any non-nil
	// error here is a genuine election fault.
	if electErr := <-electDone; electErr != nil {
		logger.Warn("iris daemon election ended with error", "err", electErr)
	}
	return serveErr
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
			// Grace elapsed: force the daemon down with SIGKILL. Kill is
			// fire-and-forget, so confirm the process has actually exited -- and thus
			// released its socket -- before reaping the pidfile, rather than reporting
			// a stopped daemon while it may still briefly hold the socket.
			_ = proc.Kill()
			killCtx, cancel := context.WithTimeout(context.Background(), killConfirmTimeout)
			waitProcessGone(killCtx, pid)
			cancel()
			return RemovePIDFile(s)
		case <-time.After(backoff):
		}
		if backoff < detachReadyPollBackoff {
			backoff *= 2
		}
	}
}

// waitProcessGone polls until the process with pid is no longer alive or ctx is
// done, so a caller can confirm a signalled process has actually exited before
// acting on its absence. It is best-effort: a deadline that passes while the
// process lingers returns anyway.
func waitProcessGone(ctx context.Context, pid int) {
	backoff := 10 * time.Millisecond
	for {
		if !processAlive(pid) {
			return
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(backoff):
		}
		if backoff < detachReadyPollBackoff {
			backoff *= 2
		}
	}
}

// processAlive reports whether a process with pid is still running, probed with
// the null signal (signal 0 delivers nothing but validates the target exists).
//
// Limitation: this cannot distinguish the original daemon from an unrelated
// process that has since been assigned the same pid (PID reuse), so a SIGKILL
// escalation in StopDaemon could in principle reach a recycled pid. The window is
// tiny -- StopDaemon signals within one bounded grace period of reading the
// pidfile -- and cannot be eliminated without a pidfd (Linux) or equivalent
// kernel process handle, which the standard library does not expose portably.
// Accepted for the minimal stop; a pidfd-based handle can close it when the
// platform surface allows.
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
