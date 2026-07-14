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

// leaderAdPollInterval bounds how often a contending standby re-reads the leader's
// advertised address from meta to refresh its guidance hint. A standby learns the
// current leader on its first read (the poll reads immediately, then on this
// interval); the interval only keeps a long-running standby current across a leader
// change while it stays blocked on the lock.
const leaderAdPollInterval = 500 * time.Millisecond

// This file is leader election and the leadership transitions around it: the step
// that turns a daemon candidate into the one leader, the sole dispatcher, and the
// step that takes leadership away again. Leadership is a Postgres session advisory
// lock: a candidate blocks acquiring it (standby), and the acquire returns only
// when it wins (leader). On winning, the candidate starts the single dispatcher
// goroutine, re-checks the meta schema through it (the leader-only write at
// election), runs startup reconciliation -- identical on cold start and failover
// promotion, so a newly promoted daemon reconciles exactly as a restarted one does
// -- and reports the leader role so its listeners accept mutations. Standbys reject
// mutations and serve reads.
//
// Losing the meta session is the other transition. Connection death releases the
// lock at Postgres and promotes the next standby, but connection death is not
// process death: the deposed daemon must demote ITSELF, explicitly and at once --
// stop dispatching (the lane loop is halted before anything else), kill its
// in-flight runs (their records are the new leader's to dead-letter; the new leader
// cannot reach the processes across hosts, so the kill is this side's duty), and
// re-enter standby on a FRESH session, because a dead Postgres session can never
// re-acquire the lock and its write guard refuses forever.

// Candidate is one daemon candidate for leadership. Serve blocks it as a standby
// until it acquires the leader lock, then runs it as the leader (sole dispatcher)
// until its context is cancelled or its lock session is lost.
type Candidate struct {
	lock      store.LeaderLock
	role      *api.RoleState
	writeConn store.MetaWriteConn
	logger    *slog.Logger

	// Startup reconciliation, run once on winning the lock before any lane dispatch
	// (crash recovery). reader is nil when reconciliation is not configured (the
	// election-only wiring the shape tests use), in which case the leader skips
	// reconciliation entirely.
	reader    store.Reader
	killer    dispatch.GroupKiller
	hostMatch dispatch.HostMatcher
	// onDispatchReady is the dispatch-ready latch fired once reconciliation completes,
	// before the leader role is reported and before the lane loop starts. It is an
	// observation hook: Serve's own ordering is what holds every lane until crash
	// reconciliation is done, so the daemon's real wiring leaves it nil and only the
	// tests that assert that ordering set it.
	onDispatchReady func()

	// Control-plane wiring, installed on winning leadership and cleared on demotion so
	// the api mux's apply/destroy routes reach the single meta writer only while this
	// daemon leads. Nil control skips it (the election-only wiring used by tests with
	// no control plane).
	control    *controlPlane
	workspace  string
	registry   store.RegistryReader
	appliedHds store.AppliedHeadReader
	data       dataPlane

	// Manual-run wiring, installed on winning leadership and cleared on demotion so
	// the api mux's POST /pipeline/run reaches the single meta writer and the exec
	// seam only while this daemon leads. Nil pipelines skips it.
	pipelines    *pipelinePlane
	manualReader store.ManualReader
	runner       exec.Runner
	// manualDataDSN is the base scoped data-database connection a manual run's
	// IRIS_DB_URL is derived from (the run id rides it), the same DSN the lane loop
	// injects; empty leaves a manual run without a data connection.
	manualDataDSN string

	// journalHM supplies the data journal high id for pin stamping (floor/ceiling)
	// and seal decisions after runs reach terminal.
	journalHM dispatch.JournalHighWatermark

	// Seal wiring: the opportunistic post-terminal seal step reads the resident
	// partition through sealData (the *pg.Client, satisfying sealDataStore), consults
	// the meta seal seam sealMeta (chain head, in-flight count, engine key), and seals
	// once the resident partition crosses sealThreshold rows. A zero threshold or nil
	// seam leaves sealing off (the shape/manual tests that wire no seal).
	sealThreshold int64
	sealData      sealDataStore
	sealMeta      store.JournalSealReader

	// Build wiring, installed on winning leadership and cleared on demotion so the api
	// mux's POST /pipeline/build reaches the single meta writer, the object store, and
	// the exec seam only while this daemon leads. Nil builds skips it.
	builds  *buildPlane
	objects *store.ObjectStore

	// Wipe wiring, installed on winning leadership and cleared on demotion so the api
	// mux's POST /workload/wipe reaches the attribution reader and the data database
	// ExecuteWipe only while leading.
	wipes *wipePlane

	// retention is the archival read seam the destroy teardown draws from (the
	// remaining-run records and artifact-hash census read before retirement). Nil
	// leaves the destroyer's no-op lister (shape-test compositions).
	retention store.RetentionReader

	// Promote wiring (for pipeline promote).
	promotes        *promotePlane
	promoteState    store.PromoteStateReader
	journalPromoter dispatch.JournalPromoter

	// Dead-letter wiring, installed on winning leadership and cleared on demotion so
	// the api mux's POST /deadletter/replay and /deadletter/drain reach the single
	// meta writer only while leading. The blast readout (GET
	// /dead_letters/{run}/impact) is a read served from the plane's reader on any
	// node, so it needs no leader install. Nil skips it.
	deadletters *deadletterPlane

	// Lane-loop wiring: builds the perpetual lane loop over the single dispatcher on
	// winning leadership. The leader drives it after reconciliation (so no lane
	// dispatches ahead of crash recovery) and stops it on demotion (so a deposed
	// leader never dispatches). Nil leaves the leader without a lane loop -- the
	// election-only, control-only, and manual-only wiring uses this, so existing
	// composition is unaffected.
	laneLoopBuild func(dispatch.Submitter) *dispatch.Loop

	// lanes is the leader-side run-cancel plane over the lane loop's in-flight runs: on
	// winning leadership the candidate installs the single-writer submitter so a POST
	// /run/cancel dead-letters a running lane run as stopped, clearing it on demotion so
	// a cancel racing a lost lock faults. Nil leaves the candidate without a cancel plane.
	lanes *lanePlane

	// Self-demotion wiring. inflight kills every in-flight run's process group when
	// the meta session is lost -- a process kill only, never a meta write (the deposed
	// session cannot carry one; the new leader dead-letters the records). fresh
	// returns a NEW lock handle and the meta write connection riding its session, so a
	// demoted daemon re-enters standby on a fresh session (a dead session can never
	// re-acquire). Nil inflight skips the kill; nil fresh means a demotion ends Serve
	// instead of re-entering standby (the election-only wiring tests use).
	inflight InflightKiller
	fresh    func() (store.LeaderLock, store.MetaWriteConn)

	// passCounter is the leader-held per-lane loop pass counter: the lane loop
	// increments it per completed pass, and the candidate resets it at the start of
	// each leadership term so counts never carry across a leader change (a restart
	// resets it by construction -- it is process memory). Nil leaves pass counting
	// unwired.
	passCounter *dispatch.PassCounter

	// Endpoint-apply wiring, installed on winning leadership and cleared on demotion
	// so POST /endpoint/apply reaches the single meta writer and the shared serving
	// registry only while leading. The registry is process-long (shared with the
	// serving mux); the applier is rebuilt each term over that term's dispatcher and
	// the data-database prepare-verifier. Nil skips it.
	endpointsPlane   *endpointPlane
	endpointRegistry *dispatch.EndpointRegistry
	prepareVerifier  dispatch.PrepareVerifier

	// PAT-mint wiring, installed on winning leadership and cleared on demotion so POST
	// /pat/create reaches the single meta writer and the data-database role
	// provisioner only while leading. It reuses the candidate's workspace, data
	// client, and the shared endpoint registry (--endpoint grant expansion). Nil skips
	// it.
	patsPlane *patPlane

	// Leader-advertisement wiring. advertiseAddr is this candidate's own address --
	// its TCP listen address, empty when socket-only -- written into the single-row
	// leadership meta table through the single writer on winning the lock, so a
	// standby can name the leader for retargeting (exit 6, GET /leader).
	// leaderAddrReader is the plain-MVCC read a standby polls while contending to
	// learn the current leader's advertised address; nil leaves a standby without a
	// hint (it reports "unknown"). Both are absent in the election-only wiring the
	// shape tests use.
	advertiseAddr    string
	leaderAddrReader store.LeaderAddrReader
}

// InflightKiller kills every in-flight run's process group, the self-demotion kill:
// a daemon losing its meta session stops dispatching and kills its in-flight runs
// at once, writing nothing to meta (the run records are the new leader's to
// dead-letter during its startup reconciliation). The dispatch.RunManager satisfies
// it; a test injects a recording fake.
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
// completes and before the leader role is reported and the lane loop starts. It is
// an observation hook on that ordering -- Serve's own sequencing is what holds every
// lane until crash reconciliation is done. A nil hook is ignored.
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

// WithDeadletterPlane wires the leader-side dead-letter plane: on winning leadership
// the candidate installs the replay/drain executor over the single dispatcher (the sole
// meta writer) into dp before reporting the leader role, and clears it on demotion so a
// replay or drain racing a lost lock faults rather than writing off-path. The plane's
// blast readout is reader-backed and served on any node, so it needs no install. A nil
// dp leaves the candidate without a dead-letter mutation plane (the shape tests use).
func WithDeadletterPlane(dp *deadletterPlane) CandidateOption {
	return func(c *Candidate) { c.deadletters = dp }
}

// WithPipelinePlane wires the leader-side manual-run plane: on winning leadership the
// candidate builds the manual-run orchestrator over the single dispatcher (the sole meta
// writer), the meta read seams, the object store (for built-run resolution from the
// leader's own objects_path), and the process runner, and installs it into pp before
// reporting the leader role, clearing it on demotion. workspace is the leader's workspace
// tree (pipeline folders resolve against it); manual is the plain-MVCC manual-run reader;
// objects is this candidate's own object store (built-run argv resolves from the leader's
// own objects_path); runner starts subprocesses. journal provides the data journal high id
// for terminal window stamping. dbURL is the base scoped data-database connection a manual
// run's IRIS_DB_URL is derived from (the same DSN the lane loop injects). A nil pp leaves
// the candidate without a manual-run plane (the shape tests use).
func WithPipelinePlane(pp *pipelinePlane, workspace string, reg store.RegistryReader, manual store.ManualReader, objects *store.ObjectStore, runner exec.Runner, journal dispatch.JournalHighWatermark, dbURL string) CandidateOption {
	return func(c *Candidate) {
		c.pipelines = pp
		c.workspace = workspace
		c.registry = reg
		c.manualReader = manual
		c.objects = objects
		c.runner = runner
		c.journalHM = journal
		c.manualDataDSN = dbURL
	}
}

// WithSealer wires the opportunistic post-terminal seal step: the
// resident-partition data store (the *pg.Client, satisfying sealDataStore), the
// meta seal read seam (chain head, in-flight run count, engine key), and the
// journal_partition_rows threshold the resident partition must cross before it
// seals. The checkpoint is signed with the engine key the seal loads from the
// engine_key meta table (minted create-once on first need). A zero threshold or nil
// seam leaves sealing off, so the manual/shape tests that wire no seal are
// unaffected.
func WithSealer(threshold int64, data sealDataStore, meta store.JournalSealReader) CandidateOption {
	return func(c *Candidate) {
		c.sealThreshold = threshold
		c.sealData = data
		c.sealMeta = meta
	}
}

// buildSealer builds the leader-side seal step over the single dispatcher (submit),
// or returns nil when no seal seam is wired (a zero threshold or nil data/meta/
// objects), leaving the manual orchestrator without a seal step exactly as the shape
// tests expect.
func (c *Candidate) buildSealer(submit dispatch.Submitter) *journalSealer {
	if c.sealThreshold <= 0 || c.sealData == nil || c.sealMeta == nil || c.objects == nil {
		return nil
	}
	return newJournalSealer(c.sealThreshold, c.sealData, c.sealMeta, submit, c.objects, c.logger)
}

// WithInflightKiller wires the self-demotion kill seam: on losing its meta session,
// the daemon kills every in-flight run's process group through k before re-entering
// standby (stops dispatching, kills in-flight runs). The kill writes nothing to
// meta -- the deposed session cannot carry a meta write, and the run records are
// the new leader's to dead-letter. A nil killer skips the kill (the election-only
// wiring).
func WithInflightKiller(k InflightKiller) CandidateOption {
	return func(c *Candidate) { c.inflight = k }
}

// WithFreshSessions wires standby re-entry after a self-demotion: fresh returns a
// NEW leader-lock handle and the meta write connection riding its session, re-entry
// into standby on a fresh session. It is called only after a demotion completes
// (dispatch stopped, in-flight runs killed, dead lock released), never for the
// first election, and each call must mint a genuinely fresh session -- a dead
// Postgres session can never re-acquire the lock, and its lock-guarded write
// connection refuses forever. Absent this option a demotion simply ends Serve
// rather than re-entering standby.
func WithFreshSessions(fresh func() (store.LeaderLock, store.MetaWriteConn)) CandidateOption {
	return func(c *Candidate) { c.fresh = fresh }
}

// WithBuildPlane wires the leader-side explicit-build plane: on winning leadership
// the candidate builds the build orchestrator over the single dispatcher (the sole
// meta writer), the run-target read seam, the content-addressed object store at
// objects_path, and the process runner, and installs it into bp before reporting
// the leader role, clearing it on demotion. workspace is the leader's workspace
// tree (pipeline folders resolve against it); manual supplies the run-target read;
// objects is the object store the binary bytes land in (and runs resolve from).
// A nil bp leaves the candidate without a build plane (the shape tests use).
func WithBuildPlane(bp *buildPlane, workspace string, manual store.ManualReader, objects *store.ObjectStore, runner exec.Runner) CandidateOption {
	return func(c *Candidate) {
		c.builds = bp
		c.workspace = workspace
		c.manualReader = manual
		c.objects = objects
		c.runner = runner
	}
}

// WithWipePlane wires the leader-side workload wipe plane: on winning leadership
// the candidate builds the wipe orchestrator over the single-writer submitter
// (for snapshots), the meta reader (for run->pipeline attribution), and the data
// client (ExecuteWipe), and installs it into wp before reporting leader role,
// clearing on demotion. reader supplies Runs() for attribution; data is the
// pg.Client.
func WithWipePlane(wp *wipePlane, reader store.Reader, data dataPlane) CandidateOption {
	return func(c *Candidate) {
		c.wipes = wp
		c.reader = reader
		c.data = data
	}
}

// WithTeardownSeams wires the destroy teardown's archival read: the retention
// reader supplies the remaining-run records (archival summaries) and the
// artifact-hash census a pipeline destroy reads before retiring the rows. With
// it absent, the destroyer falls back to its no-op lister -- no summaries, no
// freed bytes (the shape-test compositions).
func WithTeardownSeams(retention store.RetentionReader) CandidateOption {
	return func(c *Candidate) {
		c.retention = retention
	}
}

// WithPromotePlane wires the leader-side promote plane on winning leadership
// (before reporting leader role), using the submitter, promote state reader, and
// journal promoter (the data client acts as journal promoter via its impl).
func WithPromotePlane(pp *promotePlane, submit dispatch.Submitter, state store.PromoteStateReader, journal dispatch.JournalPromoter) CandidateOption {
	return func(c *Candidate) {
		c.promotes = pp
		c.promoteState = state
		c.journalPromoter = journal
		// submit and runner not needed beyond; the promoter uses submit internally.
		_ = submit
	}
}

// WithLaneLoop wires the leader-side perpetual lane loop: on winning leadership the
// candidate builds the loop over the single dispatcher (the sole meta writer) with
// build, starts it once startup reconciliation is done, and stops it on demotion.
// The loop reads the walk at each pass start and starts eligible pipelines in
// composer order, one goroutine per lane, distinct lanes in parallel. build
// receives the dispatcher as the single-writer submission seam and composes the
// walk, gate, run-start, and post-pass bookkeeping over it. A nil build (the
// default) leaves the leader without a lane loop.
func WithLaneLoop(build func(dispatch.Submitter) *dispatch.Loop) CandidateOption {
	return func(c *Candidate) { c.laneLoopBuild = build }
}

// WithLanePlane wires the leader-side run-cancel plane over the lane loop's in-flight
// runs: on winning leadership the candidate installs the single dispatcher (the sole
// meta writer) into lanes so a POST /run/cancel dead-letters a running lane run as
// stopped, clearing it on demotion so a cancel racing a lost lock faults rather than
// writing off-path. A nil plane leaves the candidate without a cancel plane.
func WithLanePlane(lanes *lanePlane) CandidateOption {
	return func(c *Candidate) { c.lanes = lanes }
}

// WithPassCounter wires the leader-held per-lane pass counter: the candidate resets
// it when it wins a leadership term, so `iris engine stats` never reports a
// previous term's pass counts after a leader change (a leader-held runtime counter,
// reset on restart and leader change; the restart half is structural -- the counter
// is process memory). The lane loop's build composes the counter's Hook into the
// loop (dispatch.WithOnPass); this option owns only the term reset. A nil counter
// is ignored.
func WithPassCounter(pc *dispatch.PassCounter) CandidateOption {
	return func(c *Candidate) { c.passCounter = pc }
}

// WithEndpointPlane wires the leader-side endpoint-apply plane: on winning
// leadership the candidate builds the endpoint applier over the single dispatcher
// (the sole meta writer), the process-long serving registry, and the data-database
// prepare-verifier, installs it into ep before reporting the leader role, and clears
// it on demotion. workspace is the leader's workspace tree (endpoints/ and schemas/
// resolve against it); registry is the shared live registry the serving mux reads;
// verifier prepare-verifies the derived SQL against the data database. A nil ep
// leaves the candidate without an endpoint plane.
func WithEndpointPlane(ep *endpointPlane, registry *dispatch.EndpointRegistry, verifier dispatch.PrepareVerifier, workspace string) CandidateOption {
	return func(c *Candidate) {
		c.endpointsPlane = ep
		c.endpointRegistry = registry
		c.prepareVerifier = verifier
		c.workspace = workspace
	}
}

// WithPATPlane wires the leader-side PAT-mint plane: on winning leadership the
// candidate builds the mint orchestrator over the single dispatcher (the sole meta
// writer), the data-database DDL client (data-PAT role provisioning), and the shared
// endpoint registry (--endpoint grant expansion), installs it into pp before
// reporting the leader role, and clears it on demotion. It reuses the candidate's
// workspace and data client. A nil pp leaves the candidate without a mint plane.
func WithPATPlane(pp *patPlane, registry *dispatch.EndpointRegistry, workspace string) CandidateOption {
	return func(c *Candidate) {
		c.patsPlane = pp
		c.endpointRegistry = registry
		c.workspace = workspace
	}
}

// WithLeaderAdvertiser sets the address this candidate advertises when it wins
// leadership: its own listen address (the TCP listen address an operator retargets
// to via --host), written into the single-row leadership meta table through the
// single writer so a standby can name it for retargeting. An empty address is
// advertised too (a socket-only leader clears any stale prior address); absent this
// option, a leader advertises the empty address.
func WithLeaderAdvertiser(addr string) CandidateOption {
	return func(c *Candidate) { c.advertiseAddr = addr }
}

// WithLeaderAddrReader wires the read a standby polls to learn the current leader's
// advertised address while it contends for the lock: each read updates the
// standby's leader hint so a not_leader rejection and GET /leader name the live
// leader. It reads plain MVCC off the reader pool, on any candidate. A nil reader
// leaves a standby without a hint (its guidance stays "unknown"), which is the
// election-only wiring the shape tests use.
func WithLeaderAddrReader(r store.LeaderAddrReader) CandidateOption {
	return func(c *Candidate) { c.leaderAddrReader = r }
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
// contends again, so one daemon process survives any number of demotions. A
// cancelled-before-acquire candidate returns nil without ever leading.
func (c *Candidate) Serve(ctx context.Context) error {
	for {
		c.role.SetStandby("")
		c.logger.Info("iris daemon standby: contending for leadership")

		// While contending, refresh the standby's leader hint from the meta advertisement
		// so a not_leader rejection names the live leader. The poll is bound to a child
		// context cancelled the instant the lock is acquired, so no stale SetStandby
		// races the SetLeader in lead().
		var stopPoll context.CancelFunc
		pollDone := make(chan struct{})
		if c.leaderAddrReader != nil {
			var pollCtx context.Context
			pollCtx, stopPoll = context.WithCancel(ctx)
			go func() { defer close(pollDone); c.pollLeaderAddress(pollCtx) }()
		} else {
			close(pollDone)
		}

		err := c.lock.Acquire(ctx)
		// Stop the advertisement poll before any leader transition, and wait for it to
		// exit, so it can never call SetStandby after lead() reports the leader role.
		if stopPoll != nil {
			stopPoll()
		}
		<-pollDone

		if err != nil {
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

// pollLeaderAddress refreshes the standby's leader hint from the meta advertisement
// while this candidate contends for the lock: each read updates the role's standby
// hint, so a not_leader rejection and GET /leader name the live leader. It reads
// immediately, then on leaderAdPollInterval, until ctx is cancelled (the lock was
// acquired, or the daemon is shutting down). A read error is non-fatal -- the hint
// keeps its last value, degrading to "unknown" rather than failing the standby --
// and it never flips the role, only refreshes the standby hint.
func (c *Candidate) pollLeaderAddress(ctx context.Context) {
	for {
		if addr, err := c.leaderAddrReader.LeaderAddr(ctx); err == nil {
			c.role.SetStandby(addr)
		} else if ctx.Err() == nil {
			c.logger.Debug("iris daemon standby: reading leader advertisement failed", "err", err)
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(leaderAdPollInterval):
		}
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
	// A new leadership term starts at zero passes: reset the leader-held pass counter
	// before any lane can complete a pass, so a re-elected leader never resumes a
	// previous term's counts.
	if c.passCounter != nil {
		c.passCounter.Reset()
	}

	d := dispatch.New(c.writeConn)
	d.Start(ctx)
	defer d.Stop()

	// The leader re-checks the meta schema at election, through the single-writer
	// dispatcher path: the first meta write only the leader performs.
	if err := d.EnsureSchema(ctx); err != nil {
		// Failed to establish the schema: relinquish leadership so another candidate
		// can try, rather than lead with an unverified meta.
		c.role.SetStandby("")
		return false, errors.Join(err, c.release())
	}

	// Startup reconciliation, before any lane dispatch and identical on cold start and
	// failover promotion (a newly promoted daemon runs the same startup reconciliation
	// as a restart): dead-letter leftover running runs, delete queued never-started
	// ones, and SIGKILL same-host survivors first -- cross-host, the matcher yields no
	// kill and the leftover runs are dead-lettered only (the deposed leader's
	// self-demotion kills its own). Disposals ride the single-writer dispatcher.
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
		// The destroyer's hard blockers ride live meta snapshots: a destroy refuses
		// while a registered pipeline declares depends_on the target, a downstream
		// run_inputs row names a target run, or an outstanding dead-letter entry
		// names the target as failed_upstream. No flag overrides these. They need
		// the run/lineage reader and the worklist reader; the shape compositions
		// that wire neither run with the open default, as before.
		var destroyOpts []dispatch.DestroyerOption
		if blocker := c.destroyBlocker(); blocker != nil {
			destroyOpts = append(destroyOpts, dispatch.WithDestroyBlocker(blocker))
		}
		// The teardown seams: the journal-driven revert of the target's un-promoted
		// disposable data (the same reverse-replay a scoped wipe runs), the archival
		// reads (remaining-run summaries + artifact-hash census), and the
		// content-addressed deletion of the artifact bytes. Each wires only when its
		// seams are present, leaving the no-op defaults for shape compositions.
		if c.reader != nil && c.data != nil {
			destroyOpts = append(destroyOpts, dispatch.WithDataReverter(destroyReverter{reader: c.reader, data: c.data}))
		}
		if c.retention != nil {
			destroyOpts = append(destroyOpts, dispatch.WithRunLister(destroyRunLister{retention: c.retention}))
		}
		if c.objects != nil {
			destroyOpts = append(destroyOpts, dispatch.WithObjectDeleter(destroyObjectDeleter{objects: c.objects}))
		}
		reg, _ := c.inflight.(*inflightRuns)
		orch := newControlOrchestrator(
			c.workspace,
			dispatch.NewApplier(c.registry, d),
			dispatch.NewDestroyer(c.registry, d, destroyOpts...),
			c.registry,
			c.data,
			dispatch.NewLedgerRecorder(d),
			c.appliedHds,
			destructiveGate{reader: c.reader, inflight: reg, submit: d},
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
		// Give the manual orchestrator the daemon's in-flight registry (when the
		// production InflightKiller is one) so each manual run's process group is
		// tracked and a self-demotion kills it; a test's fake killer is not a registry,
		// leaving manual tracking off (those tests do not exercise it).
		reg, _ := c.inflight.(*inflightRuns)
		// The opportunistic post-terminal seal step: threshold-gated, over the data
		// store (the *pg.Client that also serves as the journal high-watermark), the
		// meta seal read seam, the single dispatcher (checkpoint insert + archive
		// flip), and the object store. Nil seams (the shape tests) leave sealing off.
		mo := newManualOrchestrator(c.workspace, d, c.registry, c.manualReader, c.objects, c.runner, c.journalHM, c.manualDataDSN, reg, c.buildSealer(d), c.logger)
		c.pipelines.install(mo)
		defer c.pipelines.clear()
	}

	// Install the leader-side build orchestrator over the single dispatcher, the
	// run-target read, the object store, and the exec seam before reporting the
	// leader role, so a POST /pipeline/build that passes the mux's leader gate always
	// finds an installed orchestrator; clear it on demotion so a build racing a lost
	// lock faults rather than writing off-path.
	if c.builds != nil {
		bo := newBuildOrchestrator(c.workspace, d, c.manualReader, c.objects, c.runner, c.logger)
		c.builds.install(bo)
		defer c.builds.clear()
	}

	// Install promote before wipe (order doesn't matter).
	if c.promotes != nil {
		po := newPromoteOrchestrator(d, c.promoteState, c.journalPromoter, c.logger)
		c.promotes.install(po)
		defer c.promotes.clear()
	}

	// Install the leader-side wipe orchestrator over the attribution reader and
	// data client (ExecuteWipe) before reporting the leader role, so a POST
	// /workload/wipe that passes the mux's leader gate always finds an installed
	// handler; clear on demotion.
	if c.wipes != nil {
		reg, _ := c.inflight.(*inflightRuns)
		wo := newWipeOrchestrator(d, c.reader, c.data, reg, c.logger)
		c.wipes.install(wo)
		defer c.wipes.clear()
	}

	// Install the leader-side run-cancel plane over the single dispatcher before
	// reporting the leader role, so a POST /run/cancel that passes the mux's leader gate
	// finds an installed writer; clear it on demotion so a cancel racing a lost lock
	// faults rather than dead-lettering off the single-writer path.
	if c.lanes != nil {
		c.lanes.install(d)
		defer c.lanes.clear()
	}

	// Install the leader-side endpoint-apply orchestrator over this term's
	// dispatcher, the shared serving registry, and the data-database prepare-verifier
	// before reporting the leader role, so a POST /endpoint/apply that passes the
	// mux's leader gate always finds an installed orchestrator; clear it on demotion.
	if c.endpointsPlane != nil {
		applier := dispatch.NewEndpointApplier(c.prepareVerifier, d, c.endpointRegistry)
		c.endpointsPlane.install(newEndpointOrchestrator(c.workspace, applier, c.logger))
		defer c.endpointsPlane.clear()
	}

	// Install the leader-side PAT-mint orchestrator over this term's dispatcher, the
	// data-database DDL client, and the shared endpoint registry before reporting the
	// leader role, so a POST /pat/create that passes the mux's leader gate always
	// finds an installed orchestrator; clear it on demotion.
	if c.patsPlane != nil {
		c.patsPlane.install(newPATMintOrchestrator(c.workspace, d, c.data, c.endpointRegistry, c.logger))
		defer c.patsPlane.clear()
	}

	// Install the leader-side dead-letter replay/drain executor over the single
	// dispatcher before reporting the leader role, so a POST /deadletter/replay or
	// /deadletter/drain that passes the mux's leader gate always finds an installed
	// executor; clear it on demotion so a request racing a lost lock faults. The LSN
	// reader (fresh replacement snapshot pin) rides the journal high-watermark seam when
	// it is the live data client (*pg.Client satisfies both); a fake leaves it nil and
	// the replacement's snapshot pin empty.
	if c.deadletters != nil {
		var lsn dispatch.LSNReader
		if lr, ok := c.journalHM.(dispatch.LSNReader); ok {
			lsn = lr
		}
		reg, _ := c.inflight.(*inflightRuns)
		ex := &deadletterExec{
			submit:    d,
			manual:    c.manualReader,
			workspace: c.workspace,
			lsn:       lsn,
			journal:   c.journalHM,
			gate:      destructiveGate{reader: c.reader, inflight: reg, submit: d},
			logger:    c.logger,
		}
		c.deadletters.install(ex)
		defer c.deadletters.clear()
	}

	// Reconciliation is done: fire the dispatch-ready latch before reporting the
	// leader role and before the lane loop starts below, so no lane is ever
	// dispatched ahead of reconciliation. Advertise this leader's address into the
	// single-row leadership meta table through the single writer, so a standby
	// (sharing meta) can name it for retargeting. It rides the same pre-dispatch
	// establishment as the schema re-check and reconciliation -- before the
	// dispatch-ready latch and the leader role -- so the advertisement is in place by
	// the time this daemon reports itself leader. The upsert supersedes any prior
	// leader's address, so the advertisement converges on the live leader across a
	// failover, and a socket-only leader (empty address) clears a stale prior one. A
	// failure is non-fatal: the leader is still correct (mutations stay gated to it),
	// only the standby guidance degrades to "unknown", so a transient advertise error
	// must not cost leadership.
	if err := d.AdvertiseLeader(ctx, c.advertiseAddr); err != nil && ctx.Err() == nil {
		c.logger.Warn("iris daemon leader: advertising leader address failed; standby guidance degrades to unknown", "err", err)
	}

	if c.onDispatchReady != nil {
		c.onDispatchReady()
	}

	c.role.SetLeader()
	c.logger.Info("iris daemon leader: dispatching (sole meta writer)")

	// Drive the perpetual lane loop for the duration of leadership, over the single
	// dispatcher. It starts only now -- after crash reconciliation and after the
	// leader role is reported -- so no lane dispatches ahead of recovery, and it is
	// bound to a child context cancelled the moment leadership ends (ctx cancelled at
	// shutdown, or the lock session lost), so a demoted leader stops dispatching at
	// once. The join waits for Run to return, not for in-flight runs: a hung run holds
	// its lane but never delays demotion (the loop exits promptly on cancel).
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
		// Connection death released the lock at Postgres, but connection death is not
		// process death: demote explicitly, at once.
		demoted = true
		c.logger.Warn("iris daemon leader: lock session lost, demoting")
	}

	// Leadership is over -- shutdown or demotion. Stop dispatching FIRST: the lane
	// loop is halted before anything else, so a deposed leader never starts another
	// run (stops dispatching).
	if stopLaneLoop != nil {
		stopLaneLoop()
	}

	// On a demotion, kill the in-flight runs (kills in-flight runs). This is a process
	// kill only, never a meta write: the deposed session cannot carry one (its write
	// guard refuses), and the run records are the NEW leader's to dead-letter in its
	// startup reconciliation -- which cannot reach these processes across hosts,
	// making this kill the deposed side's half of the cross-host failover contract.
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

// freshSessionRetryBackoff caps the backoff between attempts to mint a fresh leader
// session after a demotion. A transient meta-database blip must not kill the daemon:
// it retries until a fresh session opens or the daemon is shutting down.
const freshSessionRetryBackoff = 500 * time.Millisecond

// leaderSessionMaker mints a fresh leader session: the store.Client seam
// (NewLeaderSession) satisfies it. Named so the fresh-session wiring is testable
// against a fake without a live database.
type leaderSessionMaker interface {
	NewLeaderSession(ctx context.Context) (store.LeaderLock, store.MetaWriteConn, error)
}

// freshLeaderSession builds the WithFreshSessions callback for production Run: on a
// self-demotion it mints a genuinely NEW leader session (a new session-pinned
// connection, a new advisory-lock handle, and its lock-guarded writer) so the
// demoted daemon re-enters standby and can lead again -- a dead session can never
// re-acquire the lock. Minting can fail transiently (a meta-database blip); rather
// than end the daemon, it retries with a bounded backoff until a session opens or
// ctx is cancelled (shutdown), in which case it returns a lock that refuses to
// acquire so Serve exits cleanly instead of spinning.
func freshLeaderSession(ctx context.Context, maker leaderSessionMaker, logger *slog.Logger) func() (store.LeaderLock, store.MetaWriteConn) {
	if logger == nil {
		logger = slog.New(slog.NewTextHandler(io.Discard, nil))
	}
	return func() (store.LeaderLock, store.MetaWriteConn) {
		for {
			if err := ctx.Err(); err != nil {
				return refusingLock{err: err}, nil
			}
			lock, writer, err := maker.NewLeaderSession(ctx)
			if err == nil {
				return lock, writer
			}
			logger.Warn("iris daemon demotion: minting a fresh leader session failed; retrying", "err", err)
			select {
			case <-ctx.Done():
				return refusingLock{err: ctx.Err()}, nil
			case <-time.After(freshSessionRetryBackoff):
			}
		}
	}
}

// refusingLock is the fresh-session fallback returned only when the daemon is shutting
// down: its Acquire refuses with the shutdown error, so Serve's re-entry loop returns
// cleanly instead of leading on a session that was never opened. It never carries a
// write connection (the paired writer is nil), which is safe because a failed Acquire
// means the candidate never reaches the leader path that would use one.
type refusingLock struct{ err error }

// compile-time proof the fallback satisfies the leader-lock seam.
var _ store.LeaderLock = refusingLock{}

func (l refusingLock) Acquire(context.Context) error { return l.err }
func (l refusingLock) Release(context.Context) error { return nil }

// SessionLost returns an already-closed channel: were the fallback ever consulted for
// liveness it would read as lost, never as a live session.
func (l refusingLock) SessionLost() <-chan struct{} {
	ch := make(chan struct{})
	close(ch)
	return ch
}
