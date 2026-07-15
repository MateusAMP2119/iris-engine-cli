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

	"github.com/MateusAMP2119/iris-lakehouse/internal/api"
	"github.com/MateusAMP2119/iris-lakehouse/internal/config"
	"github.com/MateusAMP2119/iris-lakehouse/internal/dispatch"
	"github.com/MateusAMP2119/iris-lakehouse/internal/exec"
	"github.com/MateusAMP2119/iris-lakehouse/internal/pg"
	"github.com/MateusAMP2119/iris-lakehouse/internal/store"
)

// This file is the daemon's foreground/detached lifecycle at the process edge: a
// foreground daemon (Run) that serves the listeners and blocks until signalled; the
// detach re-exec (Detach) that backgrounds a session-leader copy of the binary with
// its output redirected to the daemon log; and the minimal stop (StopDaemon) that
// signals a detached daemon by its recorded pid. The listener wiring is server.go;
// this owns only the lifecycle.

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

// ErrManagedNotInstalled is returned by Run when the engine runs in
// managed-Postgres mode but the managed build has not been installed yet. `iris
// engine start` maps it to a fail-fast with install guidance (managed mode needs
// `iris engine install` first).
var ErrManagedNotInstalled = errors.New("daemon: the engine's managed Postgres is not installed; run \"iris engine install\" first")

// IsManagedInstalled reports whether the managed Postgres has been installed for
// these settings: its data directory records a Postgres major version (initdb has
// run under <engine home>/pg). It is the guard Run uses to fail fast rather
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
// (foreground default, streaming). In managed mode it fails fast when the managed
// Postgres is not installed.
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

	// Workspace tree is a per-host prerequisite for every candidate (S15): resolve
	// early from CWD (the tree the daemon was started in) so a host lacking the
	// pipelines/dev sources/env_files refuses before bringing up Postgres or listeners.
	workspace, err := os.Getwd()
	if err != nil {
		return fmt.Errorf("daemon: resolve workspace: %w", err)
	}
	if err := requireWorkspaceTree(workspace); err != nil {
		return err
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

	// Preflight the admin DSN's privileges before anything needs them: the meta
	// connect below lazily creates meta (CREATEDB) and the read-pool provisioning
	// mints the engine login (CREATEROLE), so a misconfigured role fails here with
	// the missing grant named instead of a raw Postgres permission error
	// mid-sequence.
	if err := CheckPrivileges(ctx, NewAdminPrivilegeReader(adminDSN.Source().ConnString())); err != nil {
		return err
	}

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

	// The lane and manual runs' scoped data connection targets the engine-owned data
	// database (where the declared tables and capture trigger live). Each run
	// authenticates as its pipeline's own least-privilege login role (credential
	// read from meta; the admin-derived DSN only donates host/port/database and
	// stands in as the fallback for a pipeline whose role is not yet provisioned).
	laneDataDSN, err := pg.DataDSN(adminDSN.Source().ConnString())
	if err != nil {
		return fmt.Errorf("daemon: derive lane run data DSN: %w", err)
	}
	runConn, err := newRunConnBuilder(laneDataDSN, client.RoleCredentialReader(), logger)
	if err != nil {
		return fmt.Errorf("daemon: build run connection builder: %w", err)
	}

	// The leader's workspace tree (already verified as a prerequisite above): declarations
	// and the schemas/ tree resolve against it. The daemon dispatches from the
	// directory it was started in; the engine home (socket, state) is fixed per user.
	// (No re-resolve or re-check: the early check already refused lacking trees.)

	// The declared read surface: the shared read pool on the data database, the live
	// endpoint registry, and the declared-table shape source. The read pool connects
	// as the engine's own least-privilege read-pool login, which holds no table grants
	// of its own -- every data-surface read runs as the calling PAT's role, assumed
	// via SET ROLE, so the pool login is a connection identity only.
	//
	// The read-pool credential is persisted create-once in engine-owned meta
	// (read_pool_credential, mirroring engine_key): every daemon start reads the ONE
	// stored secret back rather than minting a fresh one, so two daemons on one data
	// cluster (an HA standby, or a restart racing a live leader) converge on a single
	// credential. The login's password is set to that persisted secret on every start
	// -- an idempotent password-only ALTER by the role's creator (fine on PG16+, no
	// attribute assertion): because every node ALTERs to the SAME secret it never
	// invalidates another node's live pool (unlike the former per-start fresh mint,
	// where the last starter's secret won and an earlier node's pool then failed to
	// authenticate). Setting to the persisted secret every start also self-heals the
	// crash window between the create-once INSERT and the login ALTER.
	readSecret, err := client.EnsureReadPoolCredential(ctx)
	if err != nil {
		return fmt.Errorf("daemon: ensure read-pool credential: %w", err)
	}
	if err := pg.ProvisionReadPoolLogin(ctx, data, pg.ReadPoolLoginProvision{
		Role:          pg.EngineReadPoolRole,
		CredentialDDL: store.RenderSetRolePassword(pg.EngineReadPoolRole, readSecret),
		MetaDatabase:  store.MetaDatabase,
		DataDatabase:  pg.DataDatabase,
	}); err != nil {
		return fmt.Errorf("daemon: provision read-pool login: %w", err)
	}
	readPool, readPoolConns, err := store.NewDataReadPool(ctx, adminDSN.Source(), pg.DataDatabase, pg.EngineReadPoolRole, readSecret)
	if err != nil {
		return fmt.Errorf("daemon: open data read pool: %w", err)
	}
	defer readPoolConns.Close()

	// The live endpoint registry (shared by the serving mux and the leader's endpoint
	// applier: an apply that commits serves the next /q request with no restart), the
	// declared-table shape source for /data, and the TCP bearer-token verifier.
	endpointRegistry := dispatch.NewEndpointRegistry()

	// Reload the persisted endpoints into the live registry before serving, so a
	// restart or failover serves every applied endpoint with no re-apply. The
	// in-memory registry is empty each process start; the endpoints/endpoint_filters
	// meta rows are the truth of what was applied. This runs on every node (the read
	// pool serves /q from any node) and is best-effort: a reload fault is logged,
	// never fatal -- the control plane must serve even if a read-surface reload snags.
	if err := reloadEndpoints(ctx, client.EndpointReader(), endpointRegistry, workspace, logger); err != nil {
		logger.Warn("iris daemon: reload persisted endpoints", "err", err)
	}

	dataSource := newWorkspaceDataSource(workspace)
	verifier := newStoreVerifier(client.PATReader())
	endpointCtl := newEndpointPlane()
	patMint := newPATPlane()

	// The role state the mux consults and the control plane the mux routes apply/destroy
	// to: standby/unwired until election confirms leadership and installs the
	// orchestrator.
	role := api.NewRoleState()
	control := newControlPlane()
	// The pipeline plane serves iris pipeline list from the reader pool (any node) and,
	// once this daemon leads, POST /pipeline/run through the single writer and exec seam.
	pipelines := newPipelinePlane(client.PipelineLister(), logger)
	// The build plane serves POST /pipeline/build once this daemon leads: the pinned
	// recipe toolchain through the exec seam, bytes into the object store at
	// objects_path, the content hash through the single writer into artifacts.
	builds := newBuildPlane(logger)
	workload := NewWorkloadPlane(client.ShowReader(), logger)

	// The provenance plane is the live reader: journal stamps from the data
	// database + run/summary/input lineage from meta, run through the pure
	// pg.WalkProvenance. Archived-partition stamps resolve via the object store.
	objects := store.NewObjectStore(s.ObjectsPath)
	prov := NewProvenancePlane(client.Reader(), data, objects, client.CheckpointChainReader(), logger)

	// The wipe and promote planes serve POST /workload/wipe and POST
	// /pipeline/promote once this daemon leads: the journal-driven revert over
	// the data database and the promotion that ends wipe-eligibility.
	wipes := newWipePlane(logger)
	promos := newPromotePlane(logger)

	// The daemon's in-flight run registry: the production InflightKiller the
	// self-demotion kill acts through. Both the manual orchestrator and the perpetual
	// lane loop track each live run's process group in this ONE registry, so a
	// demotion (lost meta session) kills the daemon's own in-flight runs -- manual and
	// lane alike -- at once, writing nothing to meta.
	inflight := newInflightRuns()

	// The lane plane serves POST /run/cancel once this daemon leads: it reaches a
	// running lane run's process group through the shared in-flight registry and
	// dead-letters it as stopped through the single writer. The pass counter is the
	// leader-held per-lane loop count, reset each leadership term.
	lanes := newLanePlane(logger, inflight)
	passCounter := dispatch.NewPassCounter()

	// The stats plane serves GET /stats (and `iris engine stats`) on any node: the
	// meta-backed rollup over the reader pool composed with the leader-held per-lane
	// pass counts -- the same counter the lane loop increments and the candidate resets
	// each term, so the readout reports live loop passes. A standby answers with zero
	// passes (it has dispatched none).
	stats := NewStatsPlane(client.StatsSource(), passCounter, logger)

	// The dead-letter plane serves GET /dead_letters/{run}/impact (the blast readout
	// `iris deadletter show` renders) on any node from the reader pool, and POST
	// /deadletter/replay and /deadletter/drain once this daemon leads (its executor is
	// installed on winning leadership, cleared on demotion).
	deadletters := newDeadletterPlane(client.DeadLetterReader(), client.RegistryReader(), logger)

	// The engine-info plane serves GET /info (and `iris engine info`) on any node:
	// the daemon-held runtime facts -- the live leadership role, the resolved
	// listeners, the data/meta targets, the leader-held per-lane pass counts, and
	// the display-only uptime. The inspect plane serves GET /inspect: the embedded
	// engine-table DDL dump, a pure render that touches no database. The pipeline-show
	// plane serves GET /pipeline/show from the reader pool: the resolved declaration,
	// role grants, recent runs, and the depends_on gate ledger. All three are reads,
	// served on any role, mutating nothing.
	info := NewInfoPlane(role, passCounter, InfoConfig{Socket: s.Socket, TCP: s.TCP})
	inspect := NewInspectPlane()
	pipelineShow := NewShowPlane(client.ShowReader(), logger)

	// The E14 read-route planes serve the run-history, trace, and gate readouts on any
	// node from the reader pool. The runs collection (GET /runs[?include=inputs], GET
	// /runs/{id}) is the lineage rail `iris run list` draws; the run trace (GET
	// /runs/{id}/trace) walks run_inputs ancestry up or descendants down; the pipeline
	// gate (GET /pipelines/{name}/gate) is the depends_on gate ledger `iris pipeline
	// show` prints, served standalone.
	runs := newRunsPlane(client.RunLineageReader(), logger)
	// The per-run log writer: both run paths stream a run's stdout/stderr into
	// its run-id-keyed file (runs.log_ref), the post-pass pruner deletes the file
	// with the run row, and the run-logs plane serves it back (GET /runs/{id}/logs,
	// a read on any role -- the log lives on the node that executed the run).
	runLogs := NewRunLogWriter(s)
	runTrace := newRunTracePlane(client.Reader(), logger)
	pipelineGate := newPipelineGatePlane(client.ShowReader(), logger)

	srv := NewServer(s, api.NewMux(
		api.WithRole(role), api.WithControl(control), api.WithPipelines(pipelines),
		api.WithBuild(builds), api.WithWorkloadShow(workload), api.WithProvenance(prov),
		api.WithPromote(promos), api.WithWipe(wipes), api.WithRunCancel(lanes),
		api.WithEndpoints(endpointRegistry), api.WithEndpointReader(api.NewPoolReader(readPool)),
		api.WithDataSource(dataSource), api.WithReadExecutor(readPool),
		api.WithEndpointControl(endpointCtl), api.WithPATMint(patMint),
		api.WithStats(stats),
		api.WithInfo(info), api.WithInspect(inspect), api.WithPipelineShow(pipelineShow),
		api.WithDeadImpact(deadletters), api.WithReplay(deadletters), api.WithDrain(deadletters),
		api.WithRuns(runs), api.WithRunTrace(runTrace), api.WithPipelineGate(pipelineGate),
		api.WithRunLogs(NewRunLogsPlane(runLogs)),
	), WithServerLogger(logger), WithVerifier(verifier))
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
	// through the single writer (crash recovery). A lost meta session self-demotes:
	// dispatch stops, in-flight runs are killed (WithInflightKiller), and the daemon
	// re-enters standby on a FRESH session (WithFreshSessions), so one process
	// survives any number of demotions.
	//
	// The lane loop build closure composes the loop over the single dispatcher on
	// winning leadership: the walk read, the depends_on gate, the fresh cause=loop
	// run-start (run-scoped for capture attribution, tracked in the SHARED in-flight
	// registry so run cancel and the self-demotion kill reach it), and the post-pass
	// bookkeeping (failure propagation, then count-based retention pruning down to
	// the resolved retain, each pruned run's log dying with its row). The run's data
	// connection targets the engine-owned data database, retargeted from the admin DSN.
	laneBuild := func(submit dispatch.Submitter, events *dispatch.Events) *dispatch.Loop {
		return newLaneLoop(submit, inflight, workspace, client.RegistryReader(), client.ManualReader(),
			client.RootGateReader(), events,
			exec.NewOSRunner(), data, objects, runConn, passCounter,
			client.RetentionReader(), s.Retain, runLogs, logger)
	}

	cand := NewCandidate(client.Lock(), role, client.WriteConn(), logger,
		WithReconciliation(client.Reader(), dispatch.RealGroupKiller(), dispatch.SingleHostMatcher()),
		WithControlPlane(control, workspace, client.RegistryReader(), client.AppliedHeadReader(), data),
		WithPipelinePlane(pipelines, workspace, client.RegistryReader(), client.ManualReader(), objects, exec.NewOSRunner(), data, runConn),
		WithSealer(s.JournalPartitionRows, data, client.SealReader()),
		WithBuildPlane(builds, workspace, client.ManualReader(), objects, exec.NewOSRunner()),
		WithPromotePlane(promos, submitShim{}, client.PromoteStateReader(), &liveJournalPromoter{reader: client.Reader(), db: data}),
		WithWipePlane(wipes, client.Reader(), data),
		WithLaneLoop(laneBuild),
		WithLanePlane(lanes),
		WithTeardownSeams(client.RetentionReader()),
		WithGrantDrift(client.DataPATGrantsReader()),
		WithRunLogs(runLogs),
		WithPassCounter(passCounter),
		WithDeadletterPlane(deadletters),
		WithInflightKiller(inflight),
		WithFreshSessions(freshLeaderSession(ctx, client, logger)),
		WithEndpointPlane(endpointCtl, endpointRegistry, data, workspace),
		WithPATPlane(patMint, endpointRegistry, workspace),
		// Leader advertisement: on winning the lock this candidate advertises its TCP
		// listen address (empty when socket-only) into the leadership meta table, and
		// while a standby it polls that table to name the live leader for retargeting
		// (exit 6, GET /leader). srv.TCPAddr() is the resolved listen address (the real
		// port even when the configured address used port 0).
		WithLeaderAdvertiser(srv.TCPAddr()),
		WithLeaderAddrReader(client.LeaderAddrReader()))
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
	// (graceful shutdown). It is removed exactly once here, not via defer: after Serve
	// returns the socket is free, so a racing `iris engine start` may bind and record
	// its own pid while this daemon finishes its slower teardown, and a deferred
	// removal would then delete the successor's pidfile. RemovePIDFile treats an
	// absent file as success, so this stays clean.
	if err := RemovePIDFile(s); err != nil {
		logger.Warn("iris daemon shutdown: remove pidfile", "err", err)
	}

	// Record the graceful shutdown before the deferred managed-Postgres and connection
	// teardown run, so daemon.log carries a shutdown line for either signal (graceful
	// shutdown).
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
// (-d detaches; the daemon survives the CLI's exit). The child inherits
// DaemonizedEnv so it runs in the foreground of its new session and never detaches
// again. The parent does not reap the child: it outlives the CLI.
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
// (SIGTERM), waits until the process is gone, and escalates to SIGKILL if the grace
// deadline (ctx) passes. It removes the pidfile once the daemon is gone (engine
// stop stops a detached daemon).
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
