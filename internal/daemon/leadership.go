package daemon

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"time"

	"github.com/MateusAMP2119/iris-engine-cli/internal/api"
	"github.com/MateusAMP2119/iris-engine-cli/internal/dispatch"
	"github.com/MateusAMP2119/iris-engine-cli/internal/exec"
	"github.com/MateusAMP2119/iris-engine-cli/internal/store"
)

// lockReleaseGrace bounds the explicit advisory-lock release (pg_advisory_unlock +
// session close) run on a detached context during demotion, so the release can
// complete even though the daemon's own context is already cancelled. Closing the
// session releases the lock regardless, so this only bounds the courteous explicit
// unlock.
const lockReleaseGrace = 5 * time.Second

// This file is leader election and the leadership transitions around it: the step
// that turns a daemon candidate into the one leader, the sole dispatcher, and the
// step that takes leadership away again (specification sections 2 and 15).
// Leadership is a Postgres session advisory lock: a candidate blocks acquiring it
// (standby), and the acquire returns only when it wins (leader). On winning, the
// candidate starts the single dispatcher goroutine, re-checks the meta schema
// through it (the leader-only write at election, specification section 4), runs
// startup reconciliation -- identical on cold start and failover promotion, so a
// newly promoted daemon reconciles exactly as a restarted one does -- and reports
// the leader role so its listeners accept mutations. Standbys reject mutations
// and serve reads.
//
// Losing the meta session is the other transition (E11.3). Connection death
// releases the lock at Postgres and promotes the next standby, but connection
// death is not process death: the deposed daemon must demote ITSELF, explicitly
// and at once -- stop dispatching (the lane loop is halted before anything else),
// kill its in-flight runs (their records are the new leader's to dead-letter; the
// new leader cannot reach the processes across hosts, so the kill is this side's
// duty), and re-enter standby on a FRESH session, because a dead Postgres session
// can never re-acquire the lock and its write guard refuses forever.

// Candidate is one daemon candidate for leadership. Serve blocks it as a standby
// until it acquires the leader lock, then runs it as the leader (sole dispatcher)
// until its context is cancelled or its lock session is lost.
type Candidate struct {
	lock      store.LeaderLock
	role      *api.RoleState
	writeConn store.MetaWriteConn
	logger    *slog.Logger

	// Startup reconciliation, run once on winning the lock before any lane dispatch
	// (specification section 2 crash recovery). reader is nil when reconciliation is
	// not configured (the election-only wiring E02.6 tests use), in which case the
	// leader skips reconciliation entirely.
	reader    store.Reader
	killer    dispatch.GroupKiller
	hostMatch dispatch.HostMatcher
	// onDispatchReady is the dispatch-ready latch fired once reconciliation completes,
	// before the leader role is reported: the seam the E05 lane dispatcher waits on so
	// no lane is dispatched until crash reconciliation is done. Nil until E05 wires it.
	onDispatchReady func()

	// Control-plane wiring, installed on winning leadership and cleared on demotion so
	// the api mux's apply/destroy routes reach the single meta writer only while this
	// daemon leads (specification sections 3, 6.3, and 12). Nil control skips it (the
	// election-only wiring used by tests with no control plane).
	control    *controlPlane
	workspace  string
	registry   store.RegistryReader
	appliedHds store.AppliedHeadReader
	data       dataPlane

	// Manual-run wiring, installed on winning leadership and cleared on demotion so the
	// api mux's POST /pipeline/run reaches the single meta writer and the exec seam only
	// while this daemon leads (specification section 8). Nil pipelines skips it.
	pipelines    *pipelinePlane
	manualReader store.ManualReader
	runner       exec.Runner

	// Lane-loop wiring: builds the perpetual lane loop over the single dispatcher on
	// winning leadership. The leader drives it after reconciliation (so no lane
	// dispatches ahead of crash recovery) and stops it on demotion (so a deposed leader
	// never dispatches). Nil leaves the leader without a lane loop -- the election-only,
	// control-only, and manual-only wiring uses this, so existing composition is
	// unaffected (specification sections 6.1 and 6.3).
	laneLoopBuild func(dispatch.Submitter) *dispatch.Loop

	// Self-demotion wiring (specification section 15). inflight kills every in-flight
	// run's process group when the meta session is lost -- a process kill only, never a
	// meta write (the deposed session cannot carry one; the new leader dead-letters the
	// records). fresh returns a NEW lock handle and the meta write connection riding its
	// session, so a demoted daemon re-enters standby on a fresh session (a dead session
	// can never re-acquire). Nil inflight skips the kill; nil fresh means a demotion
	// ends Serve instead of re-entering standby (the election-only wiring tests use).
	inflight InflightKiller
	fresh    func() (store.LeaderLock, store.MetaWriteConn)
}

// InflightKiller kills every in-flight run's process group, the self-demotion kill
// of specification section 15: a daemon losing its meta session stops dispatching
// and kills its in-flight runs at once, writing nothing to meta (the run records
// are the new leader's to dead-letter during its startup reconciliation). The
// dispatch.RunManager satisfies it; a test injects a recording fake.
type InflightKiller interface {
	// KillInflight best-effort SIGKILLs every in-flight run's process group and
	// returns how many groups it signalled.
	KillInflight() int
}

// CandidateOption configures a Candidate at construction.
type CandidateOption func(*Candidate)

// WithReconciliation configures the leader's startup crash reconciliation: the meta
// reader it draws leftover run records from, the process-group killer it SIGKILLs
// survivors through, and the host-identity predicate that decides which survivors
// are killable here (a nil matcher defaults to single-host). Absent this option, a
// leader performs no reconciliation.
func WithReconciliation(reader store.Reader, killer dispatch.GroupKiller, matcher dispatch.HostMatcher) CandidateOption {
	return func(c *Candidate) {
		c.reader = reader
		c.killer = killer
		c.hostMatch = matcher
	}
}

// WithDispatchReady sets the dispatch-ready latch, fired once reconciliation
// completes and before the leader role is reported: the hook the E05 lane
// dispatcher consumes to hold every lane until crash reconciliation is done. A nil
// hook is ignored.
func WithDispatchReady(hook func()) CandidateOption {
	return func(c *Candidate) { c.onDispatchReady = hook }
}

// WithControlPlane wires the leader-side control plane: on winning leadership the
// candidate builds the apply/destroy orchestrator over the single dispatcher (the sole
// meta writer) and installs it into cp before reporting the leader role, and clears it
// on demotion. workspace is the leader's workspace tree (declarations and schemas/
// resolve against it); reg and heads are the plain-MVCC meta readers the apply path
// uses; data is the data-database plane provisioning runs against. A nil cp leaves the
// candidate election-only (no control plane), the shape tests use.
func WithControlPlane(cp *controlPlane, workspace string, reg store.RegistryReader, heads store.AppliedHeadReader, data dataPlane) CandidateOption {
	return func(c *Candidate) {
		c.control = cp
		c.workspace = workspace
		c.registry = reg
		c.appliedHds = heads
		c.data = data
	}
}

// WithPipelinePlane wires the leader-side manual-run plane: on winning leadership the
// candidate builds the manual-run orchestrator over the single dispatcher (the sole meta
// writer), the meta read seams, and the process runner, and installs it into pp before
// reporting the leader role, clearing it on demotion. workspace is the leader's workspace
// tree (pipeline folders resolve against it); manual is the plain-MVCC manual-run reader;
// runner starts subprocesses. A nil pp leaves the candidate without a manual-run plane
// (the shape tests use).
func WithPipelinePlane(pp *pipelinePlane, workspace string, reg store.RegistryReader, manual store.ManualReader, runner exec.Runner) CandidateOption {
	return func(c *Candidate) {
		c.pipelines = pp
		c.workspace = workspace
		c.registry = reg
		c.manualReader = manual
		c.runner = runner
	}
}

// WithInflightKiller wires the self-demotion kill seam: on losing its meta
// session, the daemon kills every in-flight run's process group through k before
// re-entering standby (specification section 15: "stops dispatching, kills
// in-flight runs"). The kill writes nothing to meta -- the deposed session cannot
// carry a meta write, and the run records are the new leader's to dead-letter. A
// nil killer skips the kill (the election-only wiring).
func WithInflightKiller(k InflightKiller) CandidateOption {
	return func(c *Candidate) { c.inflight = k }
}

// WithFreshSessions wires standby re-entry after a self-demotion: fresh returns a
// NEW leader-lock handle and the meta write connection riding its session, the
// spec's "re-enters standby on a fresh session" (specification section 15). It is
// called only after a demotion completes (dispatch stopped, in-flight runs killed,
// dead lock released), never for the first election, and each call must mint a
// genuinely fresh session -- a dead Postgres session can never re-acquire the
// lock, and its lock-guarded write connection refuses forever. Absent this option
// a demotion ends Serve, which is how the pre-E11.3 wiring behaved.
func WithFreshSessions(fresh func() (store.LeaderLock, store.MetaWriteConn)) CandidateOption {
	return func(c *Candidate) { c.fresh = fresh }
}

// WithLaneLoop wires the leader-side perpetual lane loop: on winning leadership the
// candidate builds the loop over the single dispatcher (the sole meta writer) with build,
// starts it once startup reconciliation is done, and stops it on demotion. The loop reads
// the walk at each pass start and starts eligible pipelines in composer order, one
// goroutine per lane, distinct lanes in parallel (specification sections 6.1 and 6.3).
// build receives the dispatcher as the single-writer submission seam and composes the
// walk, gate, run-start, and post-pass bookkeeping over it. A nil build (the default)
// leaves the leader without a lane loop.
func WithLaneLoop(build func(dispatch.Submitter) *dispatch.Loop) CandidateOption {
	return func(c *Candidate) { c.laneLoopBuild = build }
}

// NewCandidate builds a leadership candidate over the leader lock, the role state
// its listeners consult, and the leader's meta write connection (which the
// dispatcher wraps in the single Writer on winning). A nil logger discards output.
// Options add startup reconciliation and the dispatch-ready latch.
func NewCandidate(lock store.LeaderLock, role *api.RoleState, writeConn store.MetaWriteConn, logger *slog.Logger, opts ...CandidateOption) *Candidate {
	if logger == nil {
		logger = slog.New(slog.NewTextHandler(io.Discard, nil))
	}
	c := &Candidate{lock: lock, role: role, writeConn: writeConn, logger: logger}
	for _, o := range opts {
		o(c)
	}
	return c
}

// Serve runs the candidate: it reports the standby role, blocks acquiring the
// leader lock, and -- once it wins -- starts the single dispatcher, re-checks the
// meta schema through it, and reports the leader role. It then blocks until ctx is
// cancelled or the lock session is lost. A shutdown (ctx cancelled) stops
// dispatching, releases the lock so the next standby is promoted, and returns. A
// lost session instead SELF-DEMOTES (stop dispatching, kill in-flight runs) and --
// when fresh sessions are wired -- re-enters standby on a fresh session and
// contends again, so one daemon process survives any number of demotions
// (specification section 15). A cancelled-before-acquire candidate returns nil
// without ever leading.
func (c *Candidate) Serve(ctx context.Context) error {
	for {
		c.role.SetStandby("")
		c.logger.Info("iris daemon standby: contending for leadership")

		if err := c.lock.Acquire(ctx); err != nil {
			if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
				// Cancelled while still a standby (never won the lock): a clean shutdown,
				// not an error.
				return nil
			}
			return fmt.Errorf("daemon: acquire leader lock: %w", err)
		}

		// Won the lock: become the leader and the sole dispatcher, until shutdown or
		// session loss.
		demoted, err := c.lead(ctx)
		if !demoted || c.fresh == nil || ctx.Err() != nil {
			return err
		}

		// Self-demotion complete: dispatch is stopped, in-flight runs are killed, and
		// the dead session's lock is relinquished (its release error, if any, is
		// non-fatal by design -- the dead session frees the lock at Postgres anyway).
		// Re-enter standby on a FRESH session: a new lock handle and the write
		// connection riding it, because the dead session can never re-acquire and its
		// write guard refuses forever.
		if err != nil {
			c.logger.Warn("iris daemon demotion: releasing the dead session's lock failed", "err", err)
		}
		c.lock, c.writeConn = c.fresh()
		c.logger.Info("iris daemon demoted: re-entering standby on a fresh session")
	}
}

// lead runs the leader loop: start the dispatcher, re-check the meta schema through
// it (a leader-only meta write), run startup crash reconciliation before any lane
// dispatch (release the dispatch-ready latch once it completes), report the leader
// role, and block until ctx is cancelled or the lock session dies -- then stop
// dispatching, kill in-flight runs on a demotion, and release the lock. It reports
// whether leadership ended by session loss (a demotion, which Serve may follow with
// a fresh-session standby re-entry) as opposed to a shutdown or a startup fault.
func (c *Candidate) lead(ctx context.Context) (demoted bool, err error) {
	d := dispatch.New(c.writeConn)
	d.Start(ctx)
	defer d.Stop()

	// The leader re-checks the meta schema at election (specification section 4),
	// through the single-writer dispatcher path: the first meta write only the
	// leader performs.
	if err := d.EnsureSchema(ctx); err != nil {
		// Failed to establish the schema: relinquish leadership so another candidate
		// can try, rather than lead with an unverified meta.
		c.role.SetStandby("")
		return false, errors.Join(err, c.release())
	}

	// Startup reconciliation, before any lane dispatch and identical on cold start
	// and failover promotion (specification sections 2 and 15: a newly promoted
	// daemon "runs the same startup reconciliation as a restart"): dead-letter
	// leftover running runs, delete queued never-started ones, and SIGKILL same-host
	// survivors first -- cross-host, the matcher yields no kill and the leftover runs
	// are dead-lettered only (the deposed leader's self-demotion kills its own).
	// Disposals ride the single-writer dispatcher.
	if c.reader != nil {
		rec := dispatch.NewReconciler(c.reader, d, c.killer, c.hostMatch, c.logger)
		if err := rec.Reconcile(ctx); err != nil {
			// Reconciliation failed: relinquish leadership rather than dispatch on an
			// unreconciled meta.
			c.role.SetStandby("")
			return false, errors.Join(err, c.release())
		}
	}

	// Install the leader-side control plane over the single dispatcher (the sole meta
	// writer) before reporting the leader role, so a POST /apply or /destroy that
	// passes the mux's leader gate always finds an installed orchestrator; clear it on
	// demotion so a request racing a lost lock faults rather than writing off-path.
	if c.control != nil {
		orch := newControlOrchestrator(
			c.workspace,
			dispatch.NewApplier(c.registry, d),
			dispatch.NewDestroyer(c.registry, d),
			c.registry,
			c.data,
			dispatch.NewLedgerRecorder(d),
			c.appliedHds,
			c.logger,
		)
		c.control.install(orch)
		defer c.control.clear()
	}

	// Install the leader-side manual-run orchestrator over the single dispatcher, the
	// meta read seams, and the process runner before reporting the leader role, so a
	// POST /pipeline/run that passes the mux's leader gate always finds an installed
	// orchestrator; clear it on demotion so a run racing a lost lock faults rather than
	// minting off-path.
	if c.pipelines != nil {
		mo := newManualOrchestrator(c.workspace, d, c.registry, c.manualReader, c.runner, c.logger)
		c.pipelines.install(mo)
		defer c.pipelines.clear()
	}

	// Reconciliation is done: release the dispatch-ready latch (the E05 lane
	// dispatcher waits on it) before reporting the leader role, so no lane is ever
	// dispatched ahead of reconciliation.
	if c.onDispatchReady != nil {
		c.onDispatchReady()
	}

	c.role.SetLeader()
	c.logger.Info("iris daemon leader: dispatching (sole meta writer)")

	// Drive the perpetual lane loop for the duration of leadership, over the single
	// dispatcher (specification sections 6.1 and 6.3). It starts only now -- after crash
	// reconciliation and after the leader role is reported -- so no lane dispatches ahead
	// of recovery, and it is bound to a child context cancelled the moment leadership ends
	// (ctx cancelled at shutdown, or the lock session lost), so a demoted leader stops
	// dispatching at once. The join waits for Run to return, not for in-flight runs: a hung
	// run holds its lane but never delays demotion (the loop exits promptly on cancel).
	var stopLaneLoop func()
	if c.laneLoopBuild != nil {
		loop := c.laneLoopBuild(d)
		loopCtx, stopLoop := context.WithCancel(ctx)
		loopDone := make(chan struct{})
		go func() {
			defer close(loopDone)
			if err := loop.Run(loopCtx); err != nil && ctx.Err() == nil {
				c.logger.Warn("iris daemon leader: lane loop stopped", "err", err)
			}
		}()
		// Idempotent stop: cancelling twice and re-receiving from a closed channel are
		// both safe, so the explicit demotion-path call below and the deferred
		// backstop coexist.
		stopLaneLoop = func() {
			stopLoop()
			<-loopDone
		}
		defer stopLaneLoop()
	}

	select {
	case <-ctx.Done():
	case <-c.lock.SessionLost():
		// Connection death released the lock at Postgres (specification section 15),
		// but connection death is not process death: demote explicitly, at once.
		demoted = true
		c.logger.Warn("iris daemon leader: lock session lost, demoting")
	}

	// Leadership is over -- shutdown or demotion. Stop dispatching FIRST: the lane
	// loop is halted before anything else, so a deposed leader never starts another
	// run (specification section 15: "stops dispatching").
	if stopLaneLoop != nil {
		stopLaneLoop()
	}

	// On a demotion, kill the in-flight runs (specification section 15: "kills
	// in-flight runs"). This is a process kill only, never a meta write: the deposed
	// session cannot carry one (its write guard refuses), and the run records are
	// the NEW leader's to dead-letter in its startup reconciliation -- which cannot
	// reach these processes across hosts, making this kill the deposed side's half
	// of the cross-host failover contract.
	if demoted && c.inflight != nil {
		killed := c.inflight.KillInflight()
		c.logger.Warn("iris daemon demotion: killed in-flight runs", "count", killed)
	}

	c.role.SetStandby("")
	// A clean shutdown (ctx cancelled) or a demotion (session lost) is not an error;
	// only a failed lock release is.
	return demoted, c.release()
}

// release relinquishes the leader lock on a detached, time-bounded context: the
// daemon's own context is cancelled at shutdown, but the explicit pg_advisory_unlock
// should still run (closing the session releases the lock regardless).
func (c *Candidate) release() error {
	ctx, cancel := context.WithTimeout(context.Background(), lockReleaseGrace)
	defer cancel()
	return c.lock.Release(ctx)
}
