package daemon

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"

	"github.com/MateusAMP2119/iris-lakehouse/internal/dispatch"
	"github.com/MateusAMP2119/iris-lakehouse/internal/exec"
	"github.com/MateusAMP2119/iris-lakehouse/internal/pg"
	"github.com/MateusAMP2119/iris-lakehouse/internal/store"
)

// This file is the daemon's leader-side perpetual lane-loop plane: the composition
// root that turns the persisted lane walk into runs. It builds the four production
// seams the dispatch.Loop composes -- the walk read (lanes + registered pipelines
// through BuildWalk), the depends_on pass gate (the same gate the manual path
// uses), the fresh cause=loop run-start (mint, exec run-scoped for capture
// attribution, record terminal), and the post-pass bookkeeping (failure propagation
// along depends_on) -- and wires the whole loop over the single dispatcher on
// winning leadership.
//
// Run execution here mirrors the manual plane's mint/exec/record shape, with two
// additions the lane loop needs that the manual path did not: the run's data
// connection carries the run id as the per-session iris.run_id GUC (pg.InjectRunID)
// so the capture trigger attributes every write to it, and every started run is
// tracked in an in-flight table so an operator `iris run cancel` and a self-demotion
// kill can reach its process group.

// runCancelDetail is the dead-letter text an operator cancel writes; the lane gate parks on exactly it (#192: a manual stop is not resurrected), while other stopped details (crash reconciliation) never park.
const runCancelDetail = "run cancelled by iris run cancel"

// lanePlane is the daemon's leader-gated run-cancel handler over the lane loop's runs.
// It reaches a running lane run's process group through the SHARED in-flight registry
// (the same one the manual orchestrator tracks into and the self-demotion kill acts
// through, so one registry serves both paths), and dead-letters it through the single
// writer. The submitter is installed on winning leadership and cleared on demotion, so
// a cancel racing a lost lock faults rather than dead-lettering off the single-writer
// path.
type lanePlane struct {
	logger   *slog.Logger
	inflight *inflightRuns      // shared registry: lane runs are tracked here, cancel reaches them here
	latest   store.ManualReader // latest-run resolution for the pipeline-level stop

	mu     sync.Mutex
	submit dispatch.Submitter // installed on leadership, nil otherwise
}

// newLanePlane returns a lane plane over the shared in-flight registry and the latest-run reader; cancel mutations fault until a leader installs the single-writer submitter.
func newLanePlane(logger *slog.Logger, inflight *inflightRuns, latest store.ManualReader) *lanePlane {
	if logger == nil {
		logger = slog.New(slog.NewTextHandler(io.Discard, nil))
	}
	return &lanePlane{logger: logger, inflight: inflight, latest: latest}
}

// install wires the single-writer submitter (on winning leadership), so a cancel
// dead-letters through the sole meta writer.
func (p *lanePlane) install(submit dispatch.Submitter) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.submit = submit
}

// clear removes the submitter (on demotion), so a cancel racing a lost lock faults
// rather than writing off the single-writer path.
func (p *lanePlane) clear() {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.submit = nil
}

// CancelRun kills a running lane run's process group and dead-letters it as
// stopped, touching nothing else (only an operator cancel frees a hung run). It is
// leader-only: with no submitter installed it reports the run is not cancellable
// here, and a run not tracked in the shared registry (already terminal or never
// started here) reports not in flight, so the CLI maps each to the right exit.
func (p *lanePlane) CancelRun(ctx context.Context, runID string) error {
	p.mu.Lock()
	submit := p.submit
	p.mu.Unlock()
	if submit == nil {
		return dispatch.ErrRunNotInFlight
	}
	// Kill the whole process group first, through the shared registry: the subprocess
	// (and every descendant that inherited the group) is gone or going before the run
	// is parked terminal. The run's own StartFresh goroutine observes the kill when Wait
	// returns and untracks it. A run absent from the registry is not in flight.
	if !p.inflight.kill(runID) {
		return fmt.Errorf("cancel run %s: %w", runID, dispatch.ErrRunNotInFlight)
	}
	// Dead-letter the run as stopped through the single writer -- one atomic CTE,
	// guarded on the running state, so it is a no-op if the run has already reached a
	// terminal state, and only this run is touched.
	if err := submit.Submit(ctx, func(w *store.Writer) error {
		return w.DeadLetterRun(ctx, runID, store.ReasonStopped, runCancelDetail)
	}); err != nil {
		return fmt.Errorf("cancel run %s: dead-letter: %w", runID, err)
	}
	return nil
}

// CancelPipeline parks a pipeline's loop: inside one single-writer submission it resolves the pipeline's latest run and dead-letters it as stopped (running or queued alike), so the lane gate refuses further loop runs until a manual run, replay, or drain supersedes the stop (#202). An already-parked pipeline succeeds idempotently; a pipeline with no live run and no park reports not in flight so the CLI can retry into the loop's next mint.
func (p *lanePlane) CancelPipeline(ctx context.Context, pipeline string) (string, error) {
	p.mu.Lock()
	submit := p.submit
	p.mu.Unlock()
	if submit == nil {
		return "", dispatch.ErrRunNotInFlight
	}
	var runID string
	var wasRunning bool
	err := submit.Submit(ctx, func(w *store.Writer) error {
		info, found, err := p.latest.LatestRun(ctx, pipeline)
		if err != nil {
			return fmt.Errorf("stop pipeline %q: read latest run: %w", pipeline, err)
		}
		if !found {
			return fmt.Errorf("stop pipeline %q: %w", pipeline, dispatch.ErrRunNotInFlight)
		}
		runID = fmt.Sprintf("%d", info.ID)
		switch {
		case info.State == store.RunRunning:
			wasRunning = true
			return w.DeadLetterRun(ctx, runID, store.ReasonStopped, runCancelDetail)
		case info.State == store.RunQueued:
			return w.DeadLetterQueuedRun(ctx, runID, store.ReasonStopped, runCancelDetail)
		case info.State == store.RunDeadLettered && info.DeadLetterReason == store.ReasonStopped && info.DeadLetterDetail == runCancelDetail:
			return nil // already parked: idempotent success
		default:
			return fmt.Errorf("stop pipeline %q: %w", pipeline, dispatch.ErrRunNotInFlight)
		}
	})
	if err != nil {
		return "", err
	}
	// Kill after the park is durable: the process group dies knowing its terminal record is already written, and laneExec's own guarded transition no-ops.
	if wasRunning {
		p.inflight.kill(runID)
	}
	return runID, nil
}

// newLaneLoop builds the perpetual lane loop over the single dispatcher (submit), the
// meta read seams, the process runner, and the data-database DSN a run's scoped
// connection is derived from. It composes the walk read, the depends_on pass gate, the
// fresh cause=loop run-start, and the failure-propagation post-pass, and installs the
// pass-count hook. The returned loop is driven for the duration of a leadership term.
func newLaneLoop(
	submit dispatch.Submitter,
	inflight *inflightRuns,
	workspace string,
	registry store.RegistryReader,
	manual store.ManualReader,
	queued store.QueuedManualReader,
	events *dispatch.Events,
	runner exec.Runner,
	journal dispatch.JournalHighWatermark,
	objects *store.ObjectStore,
	runConn *runConnBuilder,
	passCounter *dispatch.PassCounter,
	retention store.RetentionReader,
	retain int64,
	runLogs *RunLogWriter,
	logger *slog.Logger,
) *dispatch.Loop {
	if logger == nil {
		logger = slog.New(slog.NewTextHandler(io.Discard, nil))
	}
	walk := laneWalkReader{registry: registry, manual: manual}
	gate := lanePassGate{
		gate:   dispatch.NewGate(consumedReader{manual: manual}),
		edges:  edgeReader{registry: registry, manual: manual},
		latest: manual,
	}
	runnerSeam := &laneExec{
		workspace: workspace,
		submit:    submit,
		inflight:  inflight,
		manual:    manual,
		queued:    queued,
		runner:    runner,
		journal:   journal,
		objects:   objects,
		runConn:   runConn,
		runLogs:   runLogs,
		residents: newResidentRuns(),
		logger:    logger,
	}
	var deleteLog RunLogPruneFunc
	if runLogs != nil {
		deleteLog = runLogs.DeleteOnPrune
	}
	post := &lanePostPass{
		workspace:    workspace,
		submit:       submit,
		manual:       manual,
		retention:    retention,
		retain:       retain,
		deleteLog:    deleteLog,
		logger:       logger,
		startedSince: map[string]int{},
	}
	opts := []dispatch.LoopOption{dispatch.WithPostPass(post), dispatch.WithQueuedStarter(runnerSeam)}
	if passCounter != nil {
		opts = append(opts, dispatch.WithOnPass(passCounter.Hook()))
	}
	if events != nil {
		opts = append(opts, dispatch.WithEvents(events))
	}
	return dispatch.NewLoop(walk, gate, runnerSeam, logger, opts...)
}

// laneWalkReader reads the current lane walk from meta: the registered-pipeline set
// and the persisted lane rows, fed through the pure BuildWalk. It is read at each
// pass start, so a graph change lands only at the next pass.
type laneWalkReader struct {
	registry store.RegistryReader
	manual   store.ManualReader
}

// Walk returns the current per-lane runnable walk in BuildWalk's stable order.
func (r laneWalkReader) Walk(ctx context.Context) ([]dispatch.Lane, error) {
	names, err := r.registry.RegisteredPipelines(ctx)
	if err != nil {
		return nil, fmt.Errorf("lane loop: read registered pipelines: %w", err)
	}
	registered := make(map[string]bool, len(names))
	for _, n := range names {
		registered[n] = true
	}
	entries, err := r.manual.LaneRows(ctx)
	if err != nil {
		return nil, fmt.Errorf("lane loop: read lane rows: %w", err)
	}
	rows := make([]dispatch.LaneRow, len(entries))
	for i, e := range entries {
		rows[i] = dispatch.LaneRow{Lane: e.Lane, Pipeline: e.Pipeline, Pos: e.Pos}
	}
	return dispatch.BuildWalk(rows, registered), nil
}

// lanePassGate resolves a pipeline's depends_on eligibility at its turn in a pass,
// exactly like the manual path: it reads the pipeline's edges (each joined to its
// upstream's latest run) and evaluates them over the run_inputs consumed check.
//
// An edge-less pipeline re-runs indefinitely by design -- the perpetual for-loop
// -- with exactly one brake, the engine's own no-retry law: "a failed run is
// never retried; re-execution is only ever an explicit replay". A pipeline whose
// latest run carries an OUTSTANDING failed dead-letter starts no fresh loop run
// (re-running a known-broken script back to back is a crash-loop, not
// freshness); replay, drain, and a manual run are the surfaces that release the
// brake -- each removes or supersedes the worklist row the brake reads. A
// stopped run from crash reconciliation or wipe --force never parks (always-alive
// resumes), but an operator `iris run cancel` is a manual stop (#192) and parks
// until a manual run, replay, or drain supersedes it. A latest run still queued
// or running skips this turn (it is already in flight -- the queued-manual pickup
// runs ahead of this gate at the same turn).
type lanePassGate struct {
	gate   *dispatch.Gate
	edges  edgeReader
	latest store.ManualReader // latest-run state for the edge-less no-retry brake; nil = always eligible (shape tests)
}

// Eligible resolves the pipeline's gate for this pass turn.
func (g lanePassGate) Eligible(ctx context.Context, pipeline string) (dispatch.Decision, error) {
	edges, err := g.edges.Edges(ctx, pipeline)
	if err != nil {
		return dispatch.Decision{}, err
	}
	if len(edges) == 0 && g.latest != nil {
		info, found, err := g.latest.LatestRun(ctx, pipeline)
		if err != nil {
			return dispatch.Decision{}, fmt.Errorf("lane gate %q: read latest run: %w", pipeline, err)
		}
		switch {
		case found && (info.State == store.RunQueued || info.State == store.RunRunning):
			// Already in flight: wait for its terminal transition.
			return dispatch.Decision{}, nil
		case found && info.State == store.RunDeadLettered && info.DeadLetterReason == store.ReasonFailed:
			// Outstanding failure: never retried on its own -- replay, drain, or
			// a manual run releases the brake. A stopped run or a drained
			// failure carries no outstanding failed reason and falls through.
			return dispatch.Decision{}, nil
		case found && info.State == store.RunDeadLettered && info.DeadLetterReason == store.ReasonStopped && info.DeadLetterDetail == runCancelDetail:
			// Operator cancel is a manual stop (#192): the loop does not resurrect the pipeline until a manual run, replay, or drain supersedes the stop; a crash-reconciliation stop carries a different detail and falls through (always-alive resumes).
			return dispatch.Decision{}, nil
		}
		return dispatch.Decision{Run: true}, nil
	}
	return g.gate.Evaluate(ctx, pipeline, edges)
}

// laneExec mints, runs, and records the terminal state of a fresh cause=loop run,
// and starts enqueued lane-member manual runs at their lane boundary. It fills a
// fresh run record's declaration checksum, mints the queued run through the single
// writer, execs the subprocess in the pipeline folder with the run-scoped data
// connection (so the capture trigger attributes its writes), tracks it in-flight for
// cancellation, awaits its terminal exit, and records the terminal transition through
// the single writer; a queued manual run skips the mint (the manual path minted it,
// gate applied and consumption recorded) and rides the same execution body.
type laneExec struct {
	workspace string
	submit    dispatch.Submitter
	inflight  *inflightRuns
	manual    store.ManualReader
	queued    store.QueuedManualReader // enqueued lane-member manual runs; nil skips pickup (shape tests)
	runner    exec.Runner
	journal   dispatch.JournalHighWatermark
	objects   *store.ObjectStore
	runConn   *runConnBuilder // per-run scoped data connection; nil leaves IRIS_DB_URL empty (shape tests)
	runLogs   *RunLogWriter   // per-run output capture; nil discards (shape tests)
	residents *residentRuns   // live resident sessions, one per pipeline, per leadership term
	logger    *slog.Logger
}

// StartFresh mints and runs rec (cause=loop), blocking until it is terminal. A run
// that executes and then dead-letters is not an error -- it returns (RunDeadLettered,
// nil) and the lane proceeds; an error means the run could not be carried out at all
// (ctx cancellation or a start/record fault) and stops the lane's pass.
func (m *laneExec) StartFresh(ctx context.Context, rec store.RunRecord) (dispatch.RunOutcome, error) {
	target, found, err := m.manual.PipelineRunTarget(ctx, rec.Pipeline)
	if err != nil {
		return dispatch.RunSucceeded, fmt.Errorf("lane run %q: read run target: %w", rec.Pipeline, err)
	}
	if !found {
		return dispatch.RunSucceeded, fmt.Errorf("lane run %q: pipeline is not registered", rec.Pipeline)
	}
	sum, err := declarationChecksum(m.workspace, target.Folder)
	if err != nil {
		return dispatch.RunSucceeded, fmt.Errorf("lane run %q: %w", rec.Pipeline, err)
	}
	rec.DeclarationChecksum = sum

	// Mint the queued cause=loop run through the single writer, then read back its
	// meta-assigned id (the highest run for the pipeline: one goroutine per lane
	// serializes mints within a lane, and lanes never share a pipeline).
	if err := m.submit.Submit(ctx, func(w *store.Writer) error { return w.CreateRun(ctx, rec) }); err != nil {
		return dispatch.RunSucceeded, fmt.Errorf("lane run %q: mint: %w", rec.Pipeline, err)
	}
	info, ok, err := m.manual.LatestRun(ctx, rec.Pipeline)
	if err != nil {
		return dispatch.RunSucceeded, fmt.Errorf("lane run %q: read minted run: %w", rec.Pipeline, err)
	}
	if !ok {
		return dispatch.RunSucceeded, fmt.Errorf("lane run %q: minted run vanished", rec.Pipeline)
	}
	return m.runToTerminal(ctx, rec.Pipeline, target, info.ID, rec.ArtifactHash)
}

// StartQueued starts and awaits pipeline's enqueued cause=manual runs, oldest
// first, at the member's turn in a lane pass -- the pickup the manual path's
// RunQueue promised. Each queued run was minted by the manual orchestrator with
// its gate applied and consumption recorded, so the pickup only executes. A run
// whose pipeline unregistered since enqueue is deleted as a phantom (it can never
// start); a run that executes and dead-letters is not an error, the next queued
// run (and the lane) proceeds.
func (m *laneExec) StartQueued(ctx context.Context, pipeline string) error {
	if m.queued == nil {
		return nil
	}
	runs, err := m.queued.QueuedManualRuns(ctx, pipeline)
	if err != nil {
		return err
	}
	for _, q := range runs {
		target, found, err := m.manual.PipelineRunTarget(ctx, pipeline)
		if err != nil {
			return fmt.Errorf("queued manual run %d: read run target: %w", q.ID, err)
		}
		if !found {
			// Unregistered since enqueue: the run can never start; remove the
			// phantom so the gate stops reading it as in flight.
			runID := strconv.FormatInt(q.ID, 10)
			if derr := m.submit.Submit(ctx, func(w *store.Writer) error { return w.DeleteQueuedRun(ctx, runID) }); derr != nil {
				m.logger.Warn("queued manual run: could not delete unregistered phantom", "run", runID, "err", derr)
			}
			continue
		}
		if _, err := m.runToTerminal(ctx, pipeline, target, q.ID, q.ArtifactHash); err != nil {
			return err
		}
	}
	return nil
}

// runToTerminal executes an already-minted queued run to its terminal state through the pipeline's resident session (#192): it reuses the live process when one is resident, spawns one otherwise, sends "go <run_id>", and records the outcome from either a "done <status>" answer (process stays for the next run) or a process exit (legacy one-shot, today's semantics). Shared by StartFresh and StartQueued.
func (m *laneExec) runToTerminal(ctx context.Context, pipeline string, target store.PipelineRunTarget, id int64, artifactHash *string) (dispatch.RunOutcome, error) {
	runID := strconv.FormatInt(id, 10)

	// Per-run output capture, best-effort: a failed open runs the pipeline uncaptured (warned) rather than blocking dispatch.
	sink, logRef := openRunLog(m.runLogs, runID, m.logger)

	argv := dispatch.ResolveRunArgv(target.Argv, artifactHash, m.objects)
	dir := filepath.Join(m.workspace, target.Folder)
	// The scoped-connection base joins the session key: a credential landing after a fallback spawn (or rotating) recycles the session onto the right identity at the next run.
	base := m.runConn.baseFor(ctx, pipeline)
	key := residentKey(dir, argv, base)

	// A stale session (declared argv, artifact, or folder changed; or the process died between runs) is ended and replaced.
	ses := m.residents.get(pipeline)
	if ses != nil && (ses.dead() || ses.key != key) {
		ses.end()
		m.residents.drop(pipeline)
		ses = nil
	}
	if ses == nil {
		var err error
		// The env (and the DSN's spawn-time run id GUC) is fixed at spawn; a resident script re-attributes each iteration itself via SET iris.run_id.
		ses, err = spawnResident(ctx, m.runner, key, dir, argv, residentEnv(base, id))
		if err != nil {
			closeRunLog(sink)
			// Nothing started: remove the queued run so meta carries no phantom.
			if derr := m.submit.Submit(ctx, func(w *store.Writer) error { return w.DeleteQueuedRun(ctx, runID) }); derr != nil {
				m.logger.Warn("lane run: could not delete queued run after start failure", "run", runID, "err", derr)
			}
			return dispatch.RunSucceeded, fmt.Errorf("lane run %q: start: %w", pipeline, err)
		}
		m.residents.put(pipeline, ses)
	}
	ses.out.Set(sink)
	defer func() {
		ses.out.Set(nil)
		closeRunLog(sink)
	}()

	// Record running with the session's group handle; on a record failure end the session so no unrecorded process escapes.
	if err := m.submit.Submit(ctx, func(w *store.Writer) error { return w.MarkRunRunning(ctx, runID, ses.handle.PGID(), logRef) }); err != nil {
		ses.end()
		m.residents.drop(pipeline)
		return dispatch.RunSucceeded, fmt.Errorf("lane run %s: record running: %w", runID, err)
	}

	// Track in the shared in-flight registry so a cancel or self-demotion kill reaches the group; untracked once terminal.
	m.inflight.track(runID, ses.handle)
	defer m.inflight.untrack(runID)

	// Start the iteration; a write error means the process is gone and its exit reports through the select below.
	ses.drainDone()
	_ = ses.sendGo(id)

	select {
	case ev := <-ses.scanner.done:
		// Resident answer: the process stays for the next run; a non-zero status dead-letters exactly like a non-zero exit.
		if ev.code != 0 {
			detail := fmt.Sprintf("lane run dead-lettered: resident iteration status %d", ev.code)
			if derr := m.submit.Submit(ctx, func(w *store.Writer) error {
				return w.DeadLetterRun(ctx, runID, store.ReasonFailed, detail)
			}); derr != nil {
				return dispatch.RunSucceeded, fmt.Errorf("lane run %s: dead-letter: %w", runID, derr)
			}
			_ = m.submit.Submit(ctx, func(w *store.Writer) error {
				return dispatch.StampTerminal(ctx, w, m.journal, runID)
			})
			return dispatch.RunDeadLettered, nil
		}
	case <-ses.exited:
		// Process exit: legacy one-shot semantics, and the session is over.
		m.residents.drop(pipeline)
		if ses.waitErr != nil {
			m.logger.Warn("lane run: output capture bound reached", "run", runID, "err", ses.waitErr)
		}
		if ses.status.Signaled || ses.status.Code != 0 {
			// DeadLetterRun is guarded on the running state, so a cancel's stopped reason stands; the lane proceeds.
			detail := fmt.Sprintf("lane run dead-lettered: %s", exitDetail(ses.status))
			if derr := m.submit.Submit(ctx, func(w *store.Writer) error {
				return w.DeadLetterRun(ctx, runID, store.ReasonFailed, detail)
			}); derr != nil {
				return dispatch.RunSucceeded, fmt.Errorf("lane run %s: dead-letter: %w", runID, derr)
			}
			_ = m.submit.Submit(ctx, func(w *store.Writer) error {
				return dispatch.StampTerminal(ctx, w, m.journal, runID)
			})
			return dispatch.RunDeadLettered, nil
		}
	case <-ctx.Done():
		// Term over: end the session outright (the runner's ctx watcher kills the group too); the row is the next leader's to reconcile.
		m.residents.drop(pipeline)
		ses.end()
		return dispatch.RunSucceeded, ctx.Err()
	}

	if serr := m.submit.Submit(ctx, func(w *store.Writer) error { return w.MarkRunSucceeded(ctx, runID) }); serr != nil {
		return dispatch.RunSucceeded, fmt.Errorf("lane run %s: record succeeded: %w", runID, serr)
	}
	_ = m.submit.Submit(ctx, func(w *store.Writer) error {
		return dispatch.StampTerminal(ctx, w, m.journal, runID)
	})
	return dispatch.RunSucceeded, nil
}

// residentKey fingerprints what a resident process was spawned as (folder, argv, connection base); a changed key recycles the session at the next run.
func residentKey(dir string, argv []string, base string) string {
	return dir + "\x00" + strings.Join(argv, "\x00") + "\x00" + base
}

// residentEnv builds the spawn environment: the inherited daemon environment plus IRIS_DB_URL over base carrying the spawn-time run id GUC (an empty base stays empty).
func residentEnv(base string, runID int64) []string {
	v := ""
	if base != "" {
		v = pg.InjectRunID(base, runID)
	}
	return append(os.Environ(), dispatch.DBConnEnvVar+"="+v)
}

// pruneEveryRuns is the count-based prune cadence: a lane's retention prune runs
// once per this many started runs (accumulated across its passes), batching what
// per-pass pruning paid one census read and one writer transaction each for. The
// cadence is a run count, never a clock (retention is count-based, doctrine), and
// a lane's first started pass of a leadership term always prunes, so a backlog
// left by a prior term drains immediately. Between prunes at most this many runs
// linger beyond retain -- retention converges, it just amortizes.
const pruneEveryRuns = 16

// lanePostPass runs the dispatcher-owned bookkeeping after a lane pass completes,
// never mid-pass: failure propagation (for each member whose gate poisoned this
// pass, it mints a never-executed dead-lettered run (cause=propagated) recording
// the immediate failed_upstream and the poisoned upstream run(s) for lineage) and
// count-based retention pruning (each lane pipeline's runs beyond the newest
// `retain` are archived into run_summaries and deleted, sparing runs held by
// outstanding dead letters). A pass that started nothing runs no retention work
// at all -- the run set it prunes cannot have grown -- so an empty pass costs the
// post-pass zero reads and zero writes; started runs accumulate per lane and the
// prune runs on the pruneEveryRuns cadence.
type lanePostPass struct {
	workspace string
	submit    dispatch.Submitter
	manual    store.ManualReader
	retention store.RetentionReader
	retain    int64
	deleteLog RunLogPruneFunc
	logger    *slog.Logger

	// mu guards startedSince: lanes post-pass concurrently (one goroutine each).
	mu sync.Mutex
	// startedSince counts each lane's started runs since its last prune; a lane
	// absent from the map has not pruned this term (its first started pass is due).
	startedSince map[string]int
}

// AfterPass propagates each poisoned member's failure to a downstream dead-letter,
// then prunes the lane's run history down to the retention count on the
// count-based cadence.
func (p *lanePostPass) AfterPass(ctx context.Context, report dispatch.PassReport) error {
	if err := p.propagateFailures(ctx, report); err != nil {
		return err
	}
	if len(report.Started) == 0 {
		// Nothing started: the lane's run set did not grow, so there is nothing
		// new to prune. Zero retention reads, zero writes.
		return nil
	}
	p.mu.Lock()
	n, pruned := p.startedSince[report.Lane]
	n += len(report.Started)
	due := !pruned || n >= pruneEveryRuns
	if due {
		n = 0
	}
	p.startedSince[report.Lane] = n
	p.mu.Unlock()
	if !due {
		return nil
	}
	return p.pruneRetention(ctx, report)
}

// propagateFailures mints the propagated dead-letter for each poisoned member.
func (p *lanePostPass) propagateFailures(ctx context.Context, report dispatch.PassReport) error {
	for _, m := range report.Poisoned {
		plan := dispatch.PlanPropagation(m.Decision)
		if !plan.Propagate {
			continue
		}
		target, found, err := p.manual.PipelineRunTarget(ctx, m.Pipeline)
		if err != nil {
			return fmt.Errorf("lane post-pass %q: read run target: %w", m.Pipeline, err)
		}
		var sum string
		if found {
			if sum, err = declarationChecksum(p.workspace, target.Folder); err != nil {
				return fmt.Errorf("lane post-pass %q: %w", m.Pipeline, err)
			}
		}
		rec := store.PropagatedRun{
			Pipeline:               m.Pipeline,
			DeclarationChecksum:    sum,
			FailedUpstream:         plan.FailedUpstream,
			PoisonedUpstreamRunIDs: plan.PoisonedUpstreamRunIDs,
			Detail:                 fmt.Sprintf("upstream %s dead-lettered", plan.FailedUpstream),
		}
		if err := p.submit.Submit(ctx, func(w *store.Writer) error { return w.DeadLetterPropagated(ctx, rec) }); err != nil {
			return fmt.Errorf("lane post-pass %q: propagate: %w", m.Pipeline, err)
		}
	}
	return nil
}

// pruneBatchSize bounds how many runs one prune transaction archives and
// deletes. Chunking keeps a backlog drain (hundreds of thousands of runs beyond
// retain after a long session) from paying one commit per run — the difference
// between seconds and tens of minutes — while bounding transaction size so the
// single writer is never held by one enormous batch.
const pruneBatchSize = 256

// pruneRetention enforces count-based retention over THIS lane's pipelines: it
// reads the run census and the dead-letter-held set, selects the runs beyond the
// newest `retain` per pipeline (dispatch.SelectPrunable, sparing held runs), and
// prunes them through the single writer in chunks of pruneBatchSize (each chunk
// one atomic meta transaction of per-run archival summary + delete, then the
// per-run logs). Scoping to the lane's own pipelines keeps
// concurrent lane post-passes from pruning the same run: lanes never share a
// pipeline. A pass with nothing beyond retain writes nothing. An error is
// returned to the loop, which logs it and retries at the next pass boundary --
// retention is opportunistic, never fatal to dispatch.
func (p *lanePostPass) pruneRetention(ctx context.Context, report dispatch.PassReport) error {
	if p.retention == nil {
		return nil // retention read seam not wired (walk-only test composition)
	}
	member := make(map[string]bool, len(report.Pipelines))
	for _, name := range report.Pipelines {
		member[name] = true
	}

	census, err := p.retention.RetentionRuns(ctx)
	if err != nil {
		return fmt.Errorf("lane post-pass %q: %w", report.Lane, err)
	}
	var runs []dispatch.RetentionRun
	for _, ref := range census {
		if member[ref.Pipeline] {
			runs = append(runs, dispatch.RetentionRun{RunID: ref.RunID, Pipeline: ref.Pipeline})
		}
	}
	held, err := p.retention.OutstandingDeadLetterRuns(ctx)
	if err != nil {
		return fmt.Errorf("lane post-pass %q: %w", report.Lane, err)
	}

	ids := dispatch.SelectPrunable(runs, int(p.retain), held)
	if len(ids) == 0 {
		return nil
	}
	records, err := p.retention.PrunableRunsByID(ctx, ids)
	if err != nil {
		return fmt.Errorf("lane post-pass %q: %w", report.Lane, err)
	}
	for start := 0; start < len(records); start += pruneBatchSize {
		batch := records[start:min(start+pruneBatchSize, len(records))]
		if err := p.submit.Submit(ctx, func(w *store.Writer) error {
			return w.PruneRuns(ctx, batch, p.deleteLog)
		}); err != nil {
			return fmt.Errorf("lane post-pass %q: prune batch of %d runs: %w", report.Lane, len(batch), err)
		}
	}
	p.logger.Info("lane post-pass: pruned runs beyond retention", "lane", report.Lane, "count", len(records), "retain", p.retain)
	return nil
}

// declarationChecksum reads the pipeline's declaration file and returns its SHA-256 hex
// digest, the value stamped as a run's declaration_checksum (recorded on every run,
// including a never-executed propagated one).
func declarationChecksum(workspace, folder string) (string, error) {
	path := filepath.Join(workspace, folder, "iris-declare.yaml")
	raw, err := os.ReadFile(path) //nolint:gosec // G304: the declaration is an engine-registered pipeline folder under the leader's own workspace.
	if err != nil {
		return "", fmt.Errorf("read declaration for checksum (%s): %w", path, err)
	}
	sum := sha256.Sum256(raw)
	return hex.EncodeToString(sum[:]), nil
}
