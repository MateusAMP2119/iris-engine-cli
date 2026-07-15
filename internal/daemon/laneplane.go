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
	"sync"

	"github.com/MateusAMP2119/iris-lakehouse/internal/dispatch"
	"github.com/MateusAMP2119/iris-lakehouse/internal/exec"
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

// lanePlane is the daemon's leader-gated run-cancel handler over the lane loop's runs.
// It reaches a running lane run's process group through the SHARED in-flight registry
// (the same one the manual orchestrator tracks into and the self-demotion kill acts
// through, so one registry serves both paths), and dead-letters it through the single
// writer. The submitter is installed on winning leadership and cleared on demotion, so
// a cancel racing a lost lock faults rather than dead-lettering off the single-writer
// path.
type lanePlane struct {
	logger   *slog.Logger
	inflight *inflightRuns // shared registry: lane runs are tracked here, cancel reaches them here

	mu     sync.Mutex
	submit dispatch.Submitter // installed on leadership, nil otherwise
}

// newLanePlane returns a lane plane over the shared in-flight registry. The cancel
// mutation faults until a leader installs the single-writer submitter.
func newLanePlane(logger *slog.Logger, inflight *inflightRuns) *lanePlane {
	if logger == nil {
		logger = slog.New(slog.NewTextHandler(io.Discard, nil))
	}
	return &lanePlane{logger: logger, inflight: inflight}
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
		return w.DeadLetterRun(ctx, runID, store.ReasonStopped, "run cancelled by iris run cancel")
	}); err != nil {
		return fmt.Errorf("cancel run %s: dead-letter: %w", runID, err)
	}
	return nil
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
		gate:  dispatch.NewGate(consumedReader{manual: manual}),
		edges: edgeReader{registry: registry, manual: manual},
	}
	runnerSeam := &laneExec{
		workspace: workspace,
		submit:    submit,
		inflight:  inflight,
		manual:    manual,
		runner:    runner,
		journal:   journal,
		objects:   objects,
		runConn:   runConn,
		runLogs:   runLogs,
		logger:    logger,
	}
	var deleteLog RunLogPruneFunc
	if runLogs != nil {
		deleteLog = runLogs.DeleteOnPrune
	}
	post := lanePostPass{
		workspace: workspace,
		submit:    submit,
		manual:    manual,
		retention: retention,
		retain:    retain,
		deleteLog: deleteLog,
		logger:    logger,
	}
	opts := []dispatch.LoopOption{dispatch.WithPostPass(post)}
	if passCounter != nil {
		opts = append(opts, dispatch.WithOnPass(passCounter.Hook()))
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
type lanePassGate struct {
	gate  *dispatch.Gate
	edges edgeReader
}

// Eligible resolves the pipeline's gate for this pass turn.
func (g lanePassGate) Eligible(ctx context.Context, pipeline string) (dispatch.Decision, error) {
	edges, err := g.edges.Edges(ctx, pipeline)
	if err != nil {
		return dispatch.Decision{}, err
	}
	return g.gate.Evaluate(ctx, pipeline, edges)
}

// laneExec mints, runs, and records the terminal state of a fresh cause=loop run. It
// fills the run record's declaration checksum, mints the queued run through the single
// writer, execs the subprocess in the pipeline folder with the run-scoped data
// connection (so the capture trigger attributes its writes), tracks it in-flight for
// cancellation, awaits its terminal exit, and records the terminal transition through
// the single writer.
type laneExec struct {
	workspace string
	submit    dispatch.Submitter
	inflight  *inflightRuns
	manual    store.ManualReader
	runner    exec.Runner
	journal   dispatch.JournalHighWatermark
	objects   *store.ObjectStore
	runConn   *runConnBuilder // per-run scoped data connection; nil leaves IRIS_DB_URL empty (shape tests)
	runLogs   *RunLogWriter   // per-run output capture; nil discards (shape tests)
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
	runID := strconv.FormatInt(info.ID, 10)

	// Open the per-run output capture: the run's stdout and stderr stream into
	// the run-id-keyed log under .iris/logs, and its path is recorded as
	// runs.log_ref. Capture is best-effort -- a failed open runs the pipeline
	// uncaptured (warned) rather than blocking dispatch.
	sink, logRef := openRunLog(m.runLogs, runID, m.logger)

	argv := dispatch.ResolveRunArgv(target.Argv, rec.ArtifactHash, m.objects)
	h, err := m.runner.Start(ctx, exec.Spec{
		Dir:    filepath.Join(m.workspace, target.Folder),
		Argv:   argv,
		Env:    m.childEnv(ctx, rec.Pipeline, info.ID),
		Stdout: sink,
		Stderr: sink,
	})
	if err != nil {
		closeRunLog(sink)
		// Nothing started: remove the queued run so meta carries no phantom.
		if derr := m.submit.Submit(ctx, func(w *store.Writer) error { return w.DeleteQueuedRun(ctx, runID) }); derr != nil {
			m.logger.Warn("lane run: could not delete queued run after start failure", "run", runID, "err", derr)
		}
		return dispatch.RunSucceeded, fmt.Errorf("lane run %q: start: %w", rec.Pipeline, err)
	}
	defer closeRunLog(sink)

	// Record the started run running with its process-group handle and log
	// reference. If that write fails, the subprocess is already running but
	// unrecorded -- kill its group and drain before returning, so no orphaned,
	// untracked process escapes.
	if err := m.submit.Submit(ctx, func(w *store.Writer) error { return w.MarkRunRunning(ctx, runID, h.PGID(), logRef) }); err != nil {
		_ = h.Kill()
		_, _ = h.Wait()
		return dispatch.RunSucceeded, fmt.Errorf("lane run %s: record running: %w", runID, err)
	}

	// Track in the shared in-flight registry so a cancel or self-demotion kill can reach
	// the group; drop it once the run is reaped below.
	m.inflight.track(runID, h)
	defer m.inflight.untrack(runID)

	status, waitErr := h.Wait()
	if waitErr != nil {
		m.logger.Warn("lane run: output capture bound reached", "run", runID, "err", waitErr)
	}

	if status.Signaled || status.Code != 0 {
		// The run failed (or was cancelled). DeadLetterRun is guarded on the running
		// state, so if a cancel already dead-lettered it as stopped this is a no-op and
		// the stopped reason stands. The lane proceeds -- composer order never gates.
		detail := fmt.Sprintf("lane run dead-lettered: %s", exitDetail(status))
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

	if serr := m.submit.Submit(ctx, func(w *store.Writer) error { return w.MarkRunSucceeded(ctx, runID) }); serr != nil {
		return dispatch.RunSucceeded, fmt.Errorf("lane run %s: record succeeded: %w", runID, serr)
	}
	_ = m.submit.Submit(ctx, func(w *store.Writer) error {
		return dispatch.StampTerminal(ctx, w, m.journal, runID)
	})
	return dispatch.RunSucceeded, nil
}

// childEnv builds a lane run's environment: the inherited daemon environment plus
// the run-scoped data connection under IRIS_DB_URL -- the pipeline's own
// least-privilege login role, carrying the run id as the per-session iris.run_id
// GUC so the capture trigger attributes every write to this run. A run with no
// data connection wired still receives the variable, empty, so a run resolves
// its connection from one place.
func (m *laneExec) childEnv(ctx context.Context, pipeline string, runID int64) []string {
	return append(os.Environ(), dispatch.DBConnEnvVar+"="+m.runConn.dsnFor(ctx, pipeline, runID))
}

// lanePostPass runs the dispatcher-owned bookkeeping after a lane pass completes,
// never mid-pass: failure propagation (for each member whose gate poisoned this
// pass, it mints a never-executed dead-lettered run (cause=propagated) recording
// the immediate failed_upstream and the poisoned upstream run(s) for lineage) and
// count-based retention pruning (each lane pipeline's runs beyond the newest
// `retain` are archived into run_summaries and deleted, sparing runs held by
// outstanding dead letters).
type lanePostPass struct {
	workspace string
	submit    dispatch.Submitter
	manual    store.ManualReader
	retention store.RetentionReader
	retain    int64
	deleteLog RunLogPruneFunc
	logger    *slog.Logger
}

// AfterPass propagates each poisoned member's failure to a downstream dead-letter,
// then prunes the lane's run history down to the retention count.
func (p lanePostPass) AfterPass(ctx context.Context, report dispatch.PassReport) error {
	if err := p.propagateFailures(ctx, report); err != nil {
		return err
	}
	return p.pruneRetention(ctx, report)
}

// propagateFailures mints the propagated dead-letter for each poisoned member.
func (p lanePostPass) propagateFailures(ctx context.Context, report dispatch.PassReport) error {
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
func (p lanePostPass) pruneRetention(ctx context.Context, report dispatch.PassReport) error {
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
