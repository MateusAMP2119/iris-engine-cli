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
// root that turns the persisted lane walk into turns (#206). It builds the seams
// the dispatch.Loop composes -- the walk read (lanes + registered pipelines through
// BuildWalk), the depends_on pass gate (the same gate the manual path uses), the
// fresh cause=loop turn (the resident session's protocol drive), and the post-pass
// bookkeeping (failure propagation along depends_on, retention pruning) -- and
// wires the whole loop over the single dispatcher on winning leadership.
//
// Under the turn protocol a fresh loop turn mints its run record only when it has
// something to record: a producing turn mints running, commits its rows and feed
// position in one data transaction, and completes the run with the turn's stamps;
// a failed turn mints its run directly dead-lettered (the worklist is the
// product); a quiet turn writes nothing at all -- no run row, no watermark bump --
// so the lane parks until the next cause lands. The pipeline process receives no
// database credentials: the engine feeds declared-read input over stdin and
// performs declared-write output itself with exact run attribution.

// runCancelDetail is the dead-letter text an operator cancel writes; the lane gate parks on exactly it (#192: a manual stop is not resurrected), while other stopped details (crash reconciliation) never park.
const runCancelDetail = "run cancelled by iris run cancel"

// lanePlane is the daemon's leader-gated run-cancel handler over the lane loop's runs.
// It reaches a running lane run's process group through the SHARED in-flight registry
// (the same one the manual orchestrator tracks into and the self-demotion kill acts
// through, so one registry serves both paths) and the resident-session registry (a
// loop turn in flight has no run row to cancel by id; the pipeline-level stop kills
// its worker there), and dead-letters through the single writer. The submitter is
// installed on winning leadership and cleared on demotion, so a cancel racing a lost
// lock faults rather than dead-lettering off the single-writer path.
type lanePlane struct {
	logger    *slog.Logger
	inflight  *inflightRuns      // shared registry: pre-minted runs are tracked here, cancel reaches them here
	residents *residentRuns      // shared resident-session registry: the pipeline stop kills a live worker here
	latest    store.ManualReader // latest-run resolution for the pipeline-level stop

	mu     sync.Mutex
	submit dispatch.Submitter // installed on leadership, nil otherwise
}

// newLanePlane returns a lane plane over the shared in-flight and resident
// registries and the latest-run reader; cancel mutations fault until a leader
// installs the single-writer submitter.
func newLanePlane(logger *slog.Logger, inflight *inflightRuns, residents *residentRuns, latest store.ManualReader) *lanePlane {
	if logger == nil {
		logger = slog.New(slog.NewTextHandler(io.Discard, nil))
	}
	return &lanePlane{logger: logger, inflight: inflight, residents: residents, latest: latest}
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

// CancelRun kills a running run's process group and dead-letters it as stopped,
// touching nothing else (only an operator cancel frees a hung run). It is
// leader-only: with no submitter installed it reports the run is not cancellable
// here, and a run not tracked in the shared registry (already terminal, never
// started here, or a loop turn -- which has no run row while in flight; use the
// pipeline-level stop) reports not in flight, so the CLI maps each to the right
// exit.
func (p *lanePlane) CancelRun(ctx context.Context, runID string) error {
	p.mu.Lock()
	submit := p.submit
	p.mu.Unlock()
	if submit == nil {
		return dispatch.ErrRunNotInFlight
	}
	// Kill the whole process group first, through the shared registry: the subprocess
	// (and every descendant that inherited the group) is gone or going before the run
	// is parked terminal. A run absent from the registry is not in flight.
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

// CancelPipeline parks a pipeline's loop: inside one single-writer submission it
// resolves the pipeline's latest run and dead-letters it as stopped (running or
// queued alike); a pipeline whose latest run is terminal -- or which has NO runs
// at all, the quiet-loop normal under the turn protocol -- gets a park row minted
// directly dead-lettered (a never-executed stopped run the lane gate reads), so
// stop always parks. The live resident worker, if any, is killed after the park
// is durable, marked cancelled so the turn driver mints nothing for the kill. An
// already-parked pipeline succeeds idempotently.
func (p *lanePlane) CancelPipeline(ctx context.Context, pipeline string) (string, error) {
	p.mu.Lock()
	submit := p.submit
	p.mu.Unlock()
	if submit == nil {
		return "", dispatch.ErrRunNotInFlight
	}
	var runID string
	var wasRunning, minted bool
	err := submit.Submit(ctx, func(w *store.Writer) error {
		info, found, err := p.latest.LatestRun(ctx, pipeline)
		if err != nil {
			return fmt.Errorf("stop pipeline %q: read latest run: %w", pipeline, err)
		}
		if found {
			runID = fmt.Sprintf("%d", info.ID)
		}
		switch {
		case found && info.State == store.RunRunning:
			wasRunning = true
			return w.DeadLetterRun(ctx, runID, store.ReasonStopped, runCancelDetail)
		case found && info.State == store.RunQueued:
			return w.DeadLetterQueuedRun(ctx, runID, store.ReasonStopped, runCancelDetail)
		case found && info.State == store.RunDeadLettered && info.DeadLetterReason == store.ReasonStopped && info.DeadLetterDetail == runCancelDetail:
			return nil // already parked: idempotent success
		default:
			// Terminal latest, or no runs at all (a quiet loop records nothing):
			// mint the park row directly dead-lettered, the never-executed stopped
			// run the lane gate refuses to resurrect.
			minted = true
			rec := store.TurnRunRecord{Pipeline: pipeline, Cause: store.CauseManual}
			return w.DeadLetterTurnRun(ctx, rec, store.ReasonStopped, runCancelDetail)
		}
	})
	if err != nil {
		return "", err
	}
	if minted {
		if info, found, rerr := p.latest.LatestRun(ctx, pipeline); rerr == nil && found {
			runID = fmt.Sprintf("%d", info.ID)
		}
	}
	// Kill after the park is durable: the worker dies knowing its terminal record
	// is already written. The cancelled mark keeps the turn driver from minting a
	// failed dead letter for the kill; a pre-minted run's own guarded transition
	// no-ops against the park.
	if p.residents != nil {
		p.residents.cancel(pipeline)
	}
	if wasRunning {
		p.inflight.kill(runID)
	}
	return runID, nil
}

// newLaneLoop builds the perpetual lane loop over the single dispatcher (submit),
// the meta read seams, the process runner, and the data-database turn seam. It
// composes the walk read, the depends_on pass gate, the fresh cause=loop turn
// drive, and the failure-propagation post-pass, and installs the pass-count hook.
// The returned loop is driven for the duration of a leadership term.
func newLaneLoop(
	submit dispatch.Submitter,
	inflight *inflightRuns,
	residents *residentRuns,
	workspace string,
	pluginsRoot string,
	registry store.RegistryReader,
	manual store.ManualReader,
	queued store.QueuedManualReader,
	events *dispatch.Events,
	runner exec.Runner,
	journal dispatch.JournalHighWatermark,
	data turnData,
	objects *store.ObjectStore,
	counters *turnCounters,
	passCounter *dispatch.PassCounter,
	retention store.RetentionReader,
	retain int64,
	runLogs *RunLogWriter,
	logger *slog.Logger,
) *dispatch.Loop {
	if logger == nil {
		logger = slog.New(slog.NewTextHandler(io.Discard, nil))
	}
	if residents == nil {
		residents = newResidentRuns()
	}
	walk := laneWalkReader{registry: registry, manual: manual}
	gate := lanePassGate{
		gate:   dispatch.NewGate(consumedReader{manual: manual}),
		edges:  edgeReader{registry: registry, manual: manual},
		latest: manual,
	}
	runnerSeam := &laneExec{
		workspace:   workspace,
		pluginsRoot: pluginsRoot,
		submit:      submit,
		inflight:    inflight,
		manual:      manual,
		queued:      queued,
		runner:      runner,
		journal:     journal,
		data:        data,
		access:      newAccessCache(),
		objects:     objects,
		counters:    counters,
		runLogs:     runLogs,
		residents:   residents,
		logger:      logger,
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
// An edge-less pipeline gets a turn every pass by design -- the perpetual
// for-loop -- with exactly one brake, the engine's own no-retry law: "a failed
// run is never retried; re-execution is only ever an explicit replay". A pipeline
// whose latest run carries an OUTSTANDING failed dead-letter starts no fresh loop
// turn (re-running a known-broken script back to back is a crash-loop, not
// freshness); replay, drain, and a manual run are the surfaces that release the
// brake -- each removes or supersedes the worklist row the brake reads. A
// stopped run from crash reconciliation or wipe --force never parks (always-alive
// resumes), but an operator stop is a manual stop (#192) and parks until a manual
// run, replay, or drain supersedes it. A latest run still queued or running skips
// this turn (it is already in flight -- the queued-manual pickup runs ahead of
// this gate at the same turn).
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

// laneExec drives fresh cause=loop turns and starts enqueued lane-member manual
// runs at their lane boundary. A fresh turn reuses the pipeline's live resident
// session (spawning one otherwise), feeds the declared-read delta, collects the
// output against the declared writes, and records only what happened: a
// producing turn mints running, commits data atomically, and completes with the
// turn's stamps; a failed turn mints directly dead-lettered; a quiet turn
// records nothing. A queued manual run rides the same protocol drive against its
// pre-minted row.
type laneExec struct {
	workspace   string
	pluginsRoot string // installed-plugins root (~/.iris/plugins); "" refuses plugin-declaring turns
	submit      dispatch.Submitter
	inflight    *inflightRuns
	manual      store.ManualReader
	queued      store.QueuedManualReader // enqueued lane-member manual runs; nil skips pickup (shape tests)
	runner      exec.Runner
	journal     dispatch.JournalHighWatermark
	data        turnData     // data-database turn seam; nil composes shape tests (no feed, producing turns fault)
	access      *accessCache // per-pipeline declared-access cache keyed by declaration checksum
	objects     *store.ObjectStore
	counters    *turnCounters // resident turn tallies for the ps readout; nil skips
	runLogs     *RunLogWriter // per-run output capture; nil discards (shape tests)
	residents   *residentRuns // live resident sessions, one per pipeline
	logger      *slog.Logger
}

// StartFresh runs one fresh cause=loop turn for rec's pipeline, blocking until
// the turn ends. A quiet turn (done, no output, no feed consumed) returns
// RunQuiet and writes nothing anywhere; a producing turn returns RunSucceeded
// once its data transaction and run record are durable; a failed turn returns
// (RunDeadLettered, nil) and always records. An error means the turn could not
// be carried out at all (ctx cancellation or a start/record fault) and stops the
// lane's pass.
func (m *laneExec) StartFresh(ctx context.Context, rec store.RunRecord) (dispatch.RunOutcome, error) {
	target, found, err := m.manual.PipelineRunTarget(ctx, rec.Pipeline)
	if err != nil {
		return dispatch.RunSucceeded, fmt.Errorf("lane turn %q: read run target: %w", rec.Pipeline, err)
	}
	if !found {
		return dispatch.RunSucceeded, fmt.Errorf("lane turn %q: pipeline is not registered", rec.Pipeline)
	}
	raw, sum, err := declarationSource(m.workspace, target.Folder)
	if err != nil {
		return dispatch.RunSucceeded, fmt.Errorf("lane turn %q: %w", rec.Pipeline, err)
	}

	ses, err := m.ensureSession(ctx, rec.Pipeline, target, rec.ArtifactHash)
	if err != nil {
		return dispatch.RunSucceeded, fmt.Errorf("lane turn %q: start: %w", rec.Pipeline, err)
	}
	acc, err := m.access.resolve(rec.Pipeline, sum, raw)
	if err != nil {
		return dispatch.RunSucceeded, fmt.Errorf("lane turn %q: %w", rec.Pipeline, err)
	}
	var feed pg.TurnFeed
	if m.data != nil && len(acc.reads) > 0 {
		if feed, err = m.data.ReadTurnFeed(ctx, rec.Pipeline, acc.reads); err != nil {
			return dispatch.RunSucceeded, fmt.Errorf("lane turn %q: %w", rec.Pipeline, err)
		}
	}

	// Per-turn stderr capture: buffered in memory and flushed into the run-id-keyed
	// log only if the turn records (a quiet turn's log dies with the turn). A
	// declared transcript buffers the turn's frame traffic the same way.
	buf := &turnLogBuffer{}
	var tr *turnTranscript
	if target.LogSplit {
		tr = &turnTranscript{}
	}
	ses.drainFrames()
	ses.out.Set(buf)
	defer ses.out.Set(nil)

	trec := store.TurnRunRecord{
		Pipeline:               rec.Pipeline,
		Cause:                  store.CauseLoop,
		DeclarationChecksum:    sum,
		ArtifactHash:           rec.ArtifactHash,
		Handle:                 ses.handle.PGID(),
		ConsumedUpstreamRunIDs: rec.ConsumedUpstreamRunIDs,
	}

	// Declared plugins pin at turn start; a resolution failure refuses the turn
	// (dead-lettered, always recorded) before any frame is sent (#215).
	rp, perr := resolveTurnPlugins(m.pluginsRoot, acc.plugins, m.runner, buf)
	if perr != nil {
		return m.deadLetterFreshTurn(ctx, trec, target, buf, tr, "plugin resolution failed: "+perr.Error())
	}
	if rp != nil {
		trec.Plugins = rp.pins
	}

	res := driveTurn(ctx, ses, ses.nextTurn(), feed.Rows, acc.writes, rp, tr)
	trec.Calls = res.calls
	switch res.kind {
	case turnShutdown:
		// Term over: end the session outright (the runner's ctx watcher kills the
		// group too); nothing was recorded, nothing needs reconciling.
		m.residents.drop(rec.Pipeline)
		ses.end()
		return dispatch.RunSucceeded, ctx.Err()

	case turnDone:
		if len(res.rows) == 0 && !feed.Advanced {
			m.counters.bump(rec.Pipeline, false)
			return dispatch.RunQuiet, nil // the quiet turn: two JSON lines, zero writes
		}
		if len(res.rows) == 0 {
			// Input consumed, nothing produced: persist only the feed position (one
			// small data transaction, no run row -- nothing happened worth a record).
			if _, err := m.data.CommitTurn(ctx, pg.TurnCommit{Pipeline: rec.Pipeline, Position: feed.Position, AdvancePosition: true}); err != nil {
				return dispatch.RunSucceeded, fmt.Errorf("lane turn %q: advance feed position: %w", rec.Pipeline, err)
			}
			m.counters.bump(rec.Pipeline, false)
			return dispatch.RunQuiet, nil
		}
		if m.data == nil {
			return dispatch.RunSucceeded, fmt.Errorf("lane turn %q: produced %d rows with no data seam wired", rec.Pipeline, len(res.rows))
		}
		// Producing turn: mint running, commit data atomically, complete with the
		// turn's stamps. A crash between mint and complete leaves a running run
		// for the next leader's reconciliation -- never data without a record.
		m.counters.bump(rec.Pipeline, true)
		if err := m.submit.Submit(ctx, func(w *store.Writer) error { return w.CreateTurnRun(ctx, trec) }); err != nil {
			return dispatch.RunSucceeded, fmt.Errorf("lane turn %q: mint: %w", rec.Pipeline, err)
		}
		id, err := m.mintedRunID(ctx, rec.Pipeline)
		if err != nil {
			return dispatch.RunSucceeded, err
		}
		runID := strconv.FormatInt(id, 10)
		ref := m.flushTurnLog(runID, rec.Pipeline, target, buf, tr, "succeeded")
		stamps, cerr := m.data.CommitTurn(ctx, pg.TurnCommit{
			Pipeline:        rec.Pipeline,
			RunID:           id,
			Writes:          turnWrites(res.rows),
			Position:        feed.Position,
			AdvancePosition: feed.Advanced,
		})
		if cerr != nil {
			// The data transaction rolled back whole: rows, stamps, and position
			// alike. The minted run dead-letters with the refusal (always records).
			detail := fmt.Sprintf("turn commit failed: %v", cerr)
			if derr := m.submit.Submit(ctx, func(w *store.Writer) error {
				return w.DeadLetterRun(ctx, runID, store.ReasonFailed, detail)
			}); derr != nil {
				return dispatch.RunSucceeded, fmt.Errorf("lane turn %s: dead-letter: %w", runID, derr)
			}
			return dispatch.RunDeadLettered, nil
		}
		if err := m.submit.Submit(ctx, func(w *store.Writer) error {
			return w.CompleteTurnRun(ctx, runID, stamps.SnapshotLSN, stamps.JournalFloor, stamps.JournalCeiling, ref)
		}); err != nil {
			return dispatch.RunSucceeded, fmt.Errorf("lane turn %s: record succeeded: %w", runID, err)
		}
		return dispatch.RunSucceeded, nil

	case turnErrored:
		return m.deadLetterFreshTurn(ctx, trec, target, buf, tr, "pipeline error: "+errorTurnDetail(res.end))

	case turnViolated:
		// The worker's protocol state is untrusted after a violation: record, then
		// recycle the session so the next turn starts clean.
		out, err := m.deadLetterFreshTurn(ctx, trec, target, buf, tr, res.violation.Error())
		m.residents.drop(rec.Pipeline)
		ses.end()
		return out, err

	case turnDied:
		m.residents.drop(rec.Pipeline)
		if ses.wasCancelled() {
			// Operator stop: the park row is already durable (the cancel plane
			// writes before it kills); minting a failed dead letter here would
			// bury the park under a retryable failure.
			return dispatch.RunDeadLettered, nil
		}
		return m.deadLetterFreshTurn(ctx, trec, target, buf, tr, "worker died mid-turn: "+deathTurnDetail(res.status, ses.out.Tail()))
	}
	return dispatch.RunSucceeded, fmt.Errorf("lane turn %q: unknown turn ending", rec.Pipeline)
}

// deadLetterFreshTurn mints a failed fresh turn's run directly dead-lettered
// (reason failed, the detail carrying the pipeline's error, the violation with
// its offending line quoted, or the death disposition) and flushes the turn's
// buffered log against the minted id. A failed turn always records.
func (m *laneExec) deadLetterFreshTurn(ctx context.Context, trec store.TurnRunRecord, target store.PipelineRunTarget, buf *turnLogBuffer, tr *turnTranscript, detail string) (dispatch.RunOutcome, error) {
	m.counters.bump(trec.Pipeline, true)
	if err := m.submit.Submit(ctx, func(w *store.Writer) error {
		return w.DeadLetterTurnRun(ctx, trec, store.ReasonFailed, detail)
	}); err != nil {
		return dispatch.RunSucceeded, fmt.Errorf("lane turn %q: dead-letter: %w", trec.Pipeline, err)
	}
	id, err := m.mintedRunID(ctx, trec.Pipeline)
	if err != nil {
		return dispatch.RunSucceeded, err
	}
	runID := strconv.FormatInt(id, 10)
	if ref := m.flushTurnLog(runID, trec.Pipeline, target, buf, tr, "dead_lettered"); ref != "" {
		if err := m.submit.Submit(ctx, func(w *store.Writer) error { return w.StampRunLogRef(ctx, runID, ref) }); err != nil {
			m.logger.Warn("lane turn: could not stamp log ref", "run", runID, "err", err)
		}
	}
	return dispatch.RunDeadLettered, nil
}

// mintedRunID reads back the run id the pipeline's latest mint was assigned (one
// goroutine per lane serializes a pipeline's mints, and lanes never share a
// pipeline, so the latest run is the one just minted).
func (m *laneExec) mintedRunID(ctx context.Context, pipeline string) (int64, error) {
	info, ok, err := m.manual.LatestRun(ctx, pipeline)
	if err != nil {
		return 0, fmt.Errorf("lane turn %q: read minted run: %w", pipeline, err)
	}
	if !ok {
		return 0, fmt.Errorf("lane turn %q: minted run vanished", pipeline)
	}
	return info.ID, nil
}

// flushTurnLog writes a recorded turn's buffered capture into the run-id-keyed
// log under the pipeline's declared recording contract and returns the log
// reference ("" when capture is off or the open failed). A declared transcript
// flushes as a frames section ahead of the log section -- the two buffers are
// separate, so within-turn chronology holds inside each section, not across.
func (m *laneExec) flushTurnLog(runID, pipeline string, target store.PipelineRunTarget, buf *turnLogBuffer, tr *turnTranscript, outcome string) string {
	sink, ref := openRunLog(m.runLogs, runID, pipeline, target.LogSplit, target.LogStamp, m.logger)
	if sink == nil {
		return ""
	}
	tr.flushTo(sink)
	buf.flushTo(sink)
	sink.SetOutcome(outcome)
	closeRunLog(sink)
	return ref
}

// ensureSession returns the pipeline's live resident session, recycling a stale
// one (declared argv, artifact, or folder changed; or the process died between
// turns) and spawning a fresh one when needed. The child environment is the
// inherited daemon environment only: no database credentials of any kind (#206).
func (m *laneExec) ensureSession(ctx context.Context, pipeline string, target store.PipelineRunTarget, artifactHash *string) (*residentSession, error) {
	argv := dispatch.ResolveRunArgv(target.Argv, artifactHash, m.objects)
	dir := filepath.Join(m.workspace, target.Folder)
	key := residentKey(dir, argv)

	ses := m.residents.get(pipeline)
	if ses != nil && (ses.dead() || ses.key != key) {
		ses.end()
		m.residents.drop(pipeline)
		ses = nil
	}
	if ses == nil {
		var err error
		ses, err = spawnResident(ctx, m.runner, key, dir, argv, os.Environ())
		if err != nil {
			return nil, err
		}
		m.residents.put(pipeline, ses)
	}
	return ses, nil
}

// StartQueued starts and awaits pipeline's enqueued cause=manual runs, oldest
// first, at the member's turn in a lane pass -- the pickup the manual path's
// RunQueue promised. Each queued run was minted by the manual orchestrator with
// its gate applied and consumption recorded, so the pickup only executes -- as
// one protocol turn against the pre-minted row. A run whose pipeline
// unregistered since enqueue is deleted as a phantom (it can never start); a run
// that executes and dead-letters is not an error, the next queued run (and the
// lane) proceeds.
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

// runToTerminal executes an already-minted queued run as one protocol turn: it
// reuses the pipeline's live resident session (spawning one otherwise), records
// running, drives the turn, commits any produced rows atomically, and records
// the terminal transition through the single writer. Shared by the queued-manual
// pickup; the fresh loop path mints at commit instead (StartFresh).
func (m *laneExec) runToTerminal(ctx context.Context, pipeline string, target store.PipelineRunTarget, id int64, artifactHash *string) (dispatch.RunOutcome, error) {
	runID := strconv.FormatInt(id, 10)

	ses, err := m.ensureSession(ctx, pipeline, target, artifactHash)
	if err != nil {
		// Nothing started: remove the queued run so meta carries no phantom.
		if derr := m.submit.Submit(ctx, func(w *store.Writer) error { return w.DeleteQueuedRun(ctx, runID) }); derr != nil {
			m.logger.Warn("lane run: could not delete queued run after start failure", "run", runID, "err", derr)
		}
		return dispatch.RunSucceeded, fmt.Errorf("lane run %q: start: %w", pipeline, err)
	}
	raw, sum, err := declarationSource(m.workspace, target.Folder)
	if err != nil {
		return dispatch.RunSucceeded, fmt.Errorf("lane run %q: %w", pipeline, err)
	}
	acc, err := m.access.resolve(pipeline, sum, raw)
	if err != nil {
		return dispatch.RunSucceeded, fmt.Errorf("lane run %q: %w", pipeline, err)
	}
	var feed pg.TurnFeed
	if m.data != nil && len(acc.reads) > 0 {
		if feed, err = m.data.ReadTurnFeed(ctx, pipeline, acc.reads); err != nil {
			return dispatch.RunSucceeded, fmt.Errorf("lane run %q: %w", pipeline, err)
		}
	}

	// Pre-minted run: the id exists, so the log opens up front and stderr streams
	// into it live for the duration of the turn, framed per the declared contract.
	sink, logRef := openRunLog(m.runLogs, runID, pipeline, target.LogSplit, target.LogStamp, m.logger)
	ses.drainFrames()
	ses.out.Set(sink)
	defer func() {
		ses.out.Set(nil)
		closeRunLog(sink)
	}()

	// Record running with the session's group handle; on a record failure end the
	// session so no unrecorded process escapes.
	if err := m.submit.Submit(ctx, func(w *store.Writer) error { return w.MarkRunRunning(ctx, runID, ses.handle.PGID(), logRef) }); err != nil {
		ses.end()
		m.residents.drop(pipeline)
		return dispatch.RunSucceeded, fmt.Errorf("lane run %s: record running: %w", runID, err)
	}

	// Track in the shared in-flight registry so a cancel or self-demotion kill
	// reaches the group; untracked once terminal.
	m.inflight.track(runID, ses.handle)
	defer m.inflight.untrack(runID)

	deadLetter := func(detail string) (dispatch.RunOutcome, error) {
		sink.SetOutcome("dead_lettered")
		// Guarded on the running state, so a cancel's stopped reason stands.
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

	// Declared plugins pin at run start; a resolution failure refuses the run (#215).
	rp, perr := resolveTurnPlugins(m.pluginsRoot, acc.plugins, m.runner, sink)
	if perr != nil {
		m.counters.bump(pipeline, true)
		return deadLetter("plugin resolution failed: " + perr.Error())
	}

	res := driveTurn(ctx, ses, ses.nextTurn(), feed.Rows, acc.writes, rp, sink)
	if res.kind != turnShutdown {
		m.counters.bump(pipeline, true) // a pre-minted run's row always records
		if rp != nil {
			prec := store.TurnRunRecord{Plugins: rp.pins, Calls: res.calls}
			if lerr := m.submit.Submit(ctx, func(w *store.Writer) error { return w.RecordRunPlugins(ctx, runID, prec) }); lerr != nil {
				m.logger.Warn("lane run: could not record plugin ledger", "run", runID, "err", lerr)
			}
		}
	}
	// The declared stamp's close line carries the run's ending (the deferred
	// close writes it); a dead-letter on any later path overwrites it below.
	if res.kind == turnShutdown {
		sink.SetOutcome("shutdown")
	}
	switch res.kind {
	case turnShutdown:
		// Term over: end the session outright; the row is the next leader's to reconcile.
		m.residents.drop(pipeline)
		ses.end()
		return dispatch.RunSucceeded, ctx.Err()
	case turnErrored:
		return deadLetter("pipeline error: " + errorTurnDetail(res.end))
	case turnViolated:
		out, err := deadLetter(res.violation.Error())
		m.residents.drop(pipeline)
		ses.end()
		return out, err
	case turnDied:
		m.residents.drop(pipeline)
		return deadLetter("worker died mid-turn: " + deathTurnDetail(res.status, ses.out.Tail()))
	}

	// Done: commit any produced rows (and the consumed feed position) atomically,
	// attributed to the pre-minted run, then record the terminal transition.
	if len(res.rows) > 0 || feed.Advanced {
		if m.data == nil {
			return deadLetter(fmt.Sprintf("turn produced %d rows with no data seam wired", len(res.rows)))
		}
		if _, cerr := m.data.CommitTurn(ctx, pg.TurnCommit{
			Pipeline:        pipeline,
			RunID:           id,
			Writes:          turnWrites(res.rows),
			Position:        feed.Position,
			AdvancePosition: feed.Advanced,
		}); cerr != nil {
			return deadLetter(fmt.Sprintf("turn commit failed: %v", cerr))
		}
	}
	sink.SetOutcome("succeeded")
	if serr := m.submit.Submit(ctx, func(w *store.Writer) error { return w.MarkRunSucceeded(ctx, runID) }); serr != nil {
		return dispatch.RunSucceeded, fmt.Errorf("lane run %s: record succeeded: %w", runID, serr)
	}
	_ = m.submit.Submit(ctx, func(w *store.Writer) error {
		return dispatch.StampTerminal(ctx, w, m.journal, runID)
	})
	return dispatch.RunSucceeded, nil
}

// residentKey fingerprints what a resident process was spawned as (folder,
// argv); a changed key recycles the session at the next turn.
func residentKey(dir string, argv []string) string {
	return dir + "\x00" + strings.Join(argv, "\x00")
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
// outstanding dead letters). A pass that recorded nothing runs no retention work
// at all -- the run set it prunes cannot have grown -- so a quiet pass costs the
// post-pass zero reads and zero writes; recorded runs accumulate per lane and the
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
		// Nothing recorded: the lane's run set did not grow, so there is nothing
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

// declarationSource reads the pipeline's declaration file and returns its raw
// bytes plus their SHA-256 hex digest -- the value stamped as a run's
// declaration_checksum, and the key under which the turn driver caches the
// declaration's resolved access (the two always move together).
func declarationSource(workspace, folder string) ([]byte, string, error) {
	path := filepath.Join(workspace, folder, "iris-declare.yaml")
	raw, err := os.ReadFile(path) //nolint:gosec // G304: the declaration is an engine-registered pipeline folder under the leader's own workspace.
	if err != nil {
		return nil, "", fmt.Errorf("read declaration for checksum (%s): %w", path, err)
	}
	sum := sha256.Sum256(raw)
	return raw, hex.EncodeToString(sum[:]), nil
}

// declarationChecksum reads the pipeline's declaration file and returns its SHA-256 hex
// digest, the value stamped as a run's declaration_checksum (recorded on every run,
// including a never-executed propagated one).
func declarationChecksum(workspace, folder string) (string, error) {
	_, sum, err := declarationSource(workspace, folder)
	return sum, err
}
