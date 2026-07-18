package daemon

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strconv"
	"sync"

	"github.com/MateusAMP2119/iris-lakehouse/internal/api"
	"github.com/MateusAMP2119/iris-lakehouse/internal/dispatch"
	"github.com/MateusAMP2119/iris-lakehouse/internal/exec"
	"github.com/MateusAMP2119/iris-lakehouse/internal/pg"
	"github.com/MateusAMP2119/iris-lakehouse/internal/store"
)

// This file is the daemon's leader-side manual-run plane: the composition root that
// turns POST /pipeline/run and GET /pipeline/list into the depends_on gate, the
// single-writer run-record mints, and the direct subprocess exec. It sits at the
// top of the import graph (daemon composes api, dispatch, exec, and store) and is
// the one place they are wired together.
//
// The listing is a plain-MVCC read, so it is served on any node (standby included) from
// the reader pool. The manual run is a mutation -- it mints runs through the single meta
// writer and executes a subprocess -- so it is leader-only: the orchestrator is installed
// on winning leadership (before the leader role is reported) and cleared on demotion, and
// the api mux gates the mutation to the leader too. A swappable pipelinePlane holds the
// live orchestrator and satisfies api.PipelineHandler for the whole daemon lifetime, so
// the mux binds to a stable handler.
//
// Scope note: an own-lane manual run executes synchronously here (mint cause=manual
// directly running, run one protocol turn, record terminal). A lane member is queued as
// a cause=manual run for the lane runner to start in turn -- the perpetual lane loop
// (dispatch.Loop, built in laneplane.go) owns starting it -- so the manual path never
// starts a lane member out of band and same-lane serialization holds. The run's
// subprocess holds no database credentials (#206): the engine feeds the declared-read
// delta and commits the declared writes itself with the run's exact attribution. Run
// stderr streams into the per-run log (RunLogWriter, the same sink the lane loop
// wires), recorded as runs.log_ref; stdout is protocol-only.

// pipelinePlane is the daemon's api.PipelineHandler: it serves the pipeline listing from
// the reader pool always, and delegates the manual run to the live orchestrator when the
// daemon leads (faulting otherwise). It is a stable handle the mux binds to for the
// daemon's whole life.
type pipelinePlane struct {
	lister store.PipelineLister
	logger *slog.Logger

	mu   sync.RWMutex
	live *manualOrchestrator
}

// compile-time proof the pipeline plane is the mux's pipeline handler.
var _ api.PipelineHandler = (*pipelinePlane)(nil)

// newPipelinePlane returns a pipeline plane over the plain-MVCC listing reader. The run
// mutation faults until a leader installs an orchestrator; the listing works at once.
func newPipelinePlane(lister store.PipelineLister, logger *slog.Logger) *pipelinePlane {
	if logger == nil {
		logger = slog.New(slog.NewTextHandler(io.Discard, nil))
	}
	return &pipelinePlane{lister: lister, logger: logger}
}

// install wires the live manual orchestrator (on winning leadership).
func (p *pipelinePlane) install(o *manualOrchestrator) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.live = o
}

// clear removes the orchestrator (on demotion), so a run racing a lost lock faults
// rather than minting off the single-writer path.
func (p *pipelinePlane) clear() {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.live = nil
}

// orchestrator returns the installed orchestrator, or nil when not leading.
func (p *pipelinePlane) orchestrator() *manualOrchestrator {
	p.mu.RLock()
	defer p.mu.RUnlock()
	return p.live
}

// ListPipelines serves iris pipeline list from the reader pool: active-only by default,
// every registered pipeline with --all. It is a read, so it works on any node.
func (p *pipelinePlane) ListPipelines(ctx context.Context, all bool) (api.PipelineListResult, error) {
	if p.lister == nil {
		return api.PipelineListResult{}, api.ErrControlUnavailable
	}
	var items []store.PipelineListing
	var err error
	if all {
		items, err = p.lister.AllPipelines(ctx)
	} else {
		items, err = p.lister.ActivePipelines(ctx)
	}
	if err != nil {
		return api.PipelineListResult{}, err
	}
	out := make([]api.PipelineListItem, len(items))
	for i, it := range items {
		out[i] = api.PipelineListItem{Name: it.Name, Active: it.Active, Lane: it.Lane}
	}
	return api.PipelineListResult{Pipelines: out}, nil
}

// RunPipeline routes to the live orchestrator, or faults when none is installed (the mux
// gates the mutation to the leader too).
func (p *pipelinePlane) RunPipeline(ctx context.Context, req api.PipelineRunRequest) (api.PipelineRunResult, error) {
	o := p.orchestrator()
	if o == nil {
		return api.PipelineRunResult{}, api.ErrControlUnavailable
	}
	return o.run(ctx, req)
}

// manualOrchestrator runs the leader-side manual `iris pipeline run` against meta and the
// exec seam. It composes the depends_on gate (over the run_inputs consumed check), the
// edge and lane read seams, and the queue (lane members) and immediate (own-lane) run
// seams into the dispatch.ManualRunner, then translates the run's terminal state to the
// wire outcome the CLI maps to an exit code.
type manualOrchestrator struct {
	runner *dispatch.ManualRunner
	logger *slog.Logger
}

// newManualOrchestrator wires the manual-run op over the single dispatcher (the sole meta
// writer), the meta read seams, the process runner, and the object store at objects_path
// (for resolving built-run argv from artifact hashes). Resolves pipeline folders under
// workspace. A nil logger discards output. The objects is the candidate's own at
// construction time, so a promoted leader dispatches using its own objects_path. journal
// provides the data journal high id for terminal window stamping. inflight (nil in the
// shape tests) tracks each live run's process group so a self-demotion kills it. data
// is the turn seam (#206): an immediate manual run executes as one
// protocol turn -- the engine feeds the declared-read delta and performs the declared
// writes itself with the run's exact attribution; the subprocess holds no credentials.
func newManualOrchestrator(workspace, pluginsRoot string, services *pluginServices, submit dispatch.Submitter, registry store.RegistryReader, manual store.ManualReader, objects *store.ObjectStore, runner exec.Runner, journal dispatch.JournalHighWatermark, data turnData, inflight *inflightRuns, sealer *journalSealer, runLogs *RunLogWriter, logger *slog.Logger) *manualOrchestrator {
	if logger == nil {
		logger = slog.New(slog.NewTextHandler(io.Discard, nil))
	}
	execer := &manualExec{
		workspace:   workspace,
		pluginsRoot: pluginsRoot,
		services:    services,
		submitter:   submit,
		manual:      manual,
		objects:     objects,
		runner:      runner,
		journal:     journal,
		data:        data,
		access:      newAccessCache(),
		inflight:    inflight,
		sealer:      sealer,
		runLogs:     runLogs,
		logger:      logger,
	}
	mr := dispatch.NewManualRunner(
		dispatch.NewGate(consumedReader{manual: manual}),
		edgeReader{registry: registry, manual: manual},
		laneReader{manual: manual},
		runQueue{exec: execer},
		immediateRunner{exec: execer},
	)
	return &manualOrchestrator{runner: mr, logger: logger}
}

// run performs one manual run and maps its terminal state to the wire outcome.
func (o *manualOrchestrator) run(ctx context.Context, req api.PipelineRunRequest) (api.PipelineRunResult, error) {
	if req.Pipeline == "" {
		return api.PipelineRunResult{}, fmt.Errorf("pipeline run: missing pipeline name")
	}
	state, reason, err := o.runner.Run(ctx, req.Pipeline)
	if err != nil {
		return api.PipelineRunResult{}, err
	}
	return api.PipelineRunResult{Pipeline: req.Pipeline, State: manualStateToken(state), Reason: reason}, nil
}

// manualStateToken maps the dispatcher's manual-run state to the wire token the CLI reads
// to pick an exit code.
func manualStateToken(s dispatch.ManualRunState) string {
	switch s {
	case dispatch.ManualRunQueued:
		return api.PipelineRunQueued
	case dispatch.ManualRunSucceeded:
		return api.PipelineRunSucceeded
	case dispatch.ManualRunDeadLettered:
		return api.PipelineRunDeadLettered
	default:
		return api.PipelineRunIneligible
	}
}

// edgeReader resolves a pipeline's depends_on edges from meta for the manual gate: the
// dependency edges from the registry, each joined to its upstream's most recent run. The
// awaited-from baseline is zero: a manual run consumes the upstream's latest success when
// it has not already been consumed (the run_inputs check decides), rather than treating a
// pre-existing success as un-awaited history.
type edgeReader struct {
	registry store.RegistryReader
	manual   store.ManualReader
}

// Edges returns pipeline's depends_on edges resolved against each upstream's latest run.
func (e edgeReader) Edges(ctx context.Context, pipeline string) ([]dispatch.Edge, error) {
	all, err := e.registry.DependencyEdges(ctx)
	if err != nil {
		return nil, fmt.Errorf("manual run %q: read dependency edges: %w", pipeline, err)
	}
	var edges []dispatch.Edge
	for _, dep := range all {
		if dep.From != pipeline {
			continue
		}
		info, found, err := e.manual.LatestRun(ctx, dep.To)
		if err != nil {
			return nil, fmt.Errorf("manual run %q: read upstream %q latest run: %w", pipeline, dep.To, err)
		}
		edges = append(edges, dispatch.Edge{
			Upstream:    dep.To,
			Latest:      upstreamState(found, info.State),
			LatestRunID: info.ID,
		})
	}
	return edges, nil
}

// upstreamState maps an upstream's latest run (or its absence) to the gate's
// upstream-state enum.
func upstreamState(found bool, s store.RunState) dispatch.UpstreamState {
	if !found {
		return dispatch.UpstreamNone
	}
	switch s {
	case store.RunQueued, store.RunRunning:
		return dispatch.UpstreamPending
	case store.RunSucceeded:
		return dispatch.UpstreamSucceeded
	case store.RunDeadLettered:
		return dispatch.UpstreamDeadLettered
	default:
		return dispatch.UpstreamNone
	}
}

// consumedReader adapts the meta run_inputs consumed check to the gate's ConsumedReader.
type consumedReader struct {
	manual store.ManualReader
}

// Consumed answers the run_inputs already-consumed check.
func (c consumedReader) Consumed(ctx context.Context, dependent string, upstreamRunID int64) (bool, error) {
	return c.manual.Consumed(ctx, dependent, upstreamRunID)
}

// laneReader adapts the meta lane roster to the manual router's LaneReader.
type laneReader struct {
	manual store.ManualReader
}

// LaneRows returns the persisted lane rows.
func (l laneReader) LaneRows(ctx context.Context) ([]dispatch.LaneRow, error) {
	rows, err := l.manual.LaneRows(ctx)
	if err != nil {
		return nil, err
	}
	out := make([]dispatch.LaneRow, len(rows))
	for i, r := range rows {
		out[i] = dispatch.LaneRow{Lane: r.Lane, Pipeline: r.Pipeline, Pos: r.Pos}
	}
	return out, nil
}

// runQueue mints a lane member's manual run as a queued cause=manual run for the
// perpetual lane loop to start in turn, so same-lane serialization holds. It never starts
// the run.
type runQueue struct {
	exec *manualExec
}

// Enqueue mints rec (cause=manual) as a queued run.
func (q runQueue) Enqueue(ctx context.Context, _ string, rec store.RunRecord) error {
	_, err := q.exec.mint(ctx, rec)
	return err
}

// immediateRunner mints and runs an own-lane manual run synchronously, recording its
// terminal state through the single writer.
type immediateRunner struct {
	exec *manualExec
}

// RunNow mints, starts, and awaits an own-lane manual run.
func (r immediateRunner) RunNow(ctx context.Context, rec store.RunRecord) (dispatch.RunOutcome, error) {
	return r.exec.runNow(ctx, rec)
}

// manualExec is the shared execution machinery the queue and immediate seams drive: it
// fills a run record's declaration checksum, mints the run through the single writer, and
// -- for the immediate path -- runs the subprocess as one protocol turn and records its
// terminal state.
type manualExec struct {
	workspace   string
	pluginsRoot string          // installed-plugins root (~/.iris/plugins); "" refuses plugin-declaring runs
	services    *pluginServices // service-instance supervisor (#215 stage 3); nil refuses service bindings
	submitter   dispatch.Submitter
	manual      store.ManualReader
	objects     *store.ObjectStore // the leader's own objects_path for built run argv resolution via ResolveRunArgv
	runner      exec.Runner
	journal     dispatch.JournalHighWatermark
	data        turnData       // data-database turn seam (#206); nil composes shape tests (no feed, producing turns dead-letter)
	access      *accessCache   // per-pipeline declared-access cache keyed by declaration checksum
	inflight    *inflightRuns  // tracks this run's live process group so a self-demotion kills it; nil in the shape tests
	sealer      *journalSealer // the opportunistic post-pass seal step; nil in the shape tests leaves sealing off
	runLogs     *RunLogWriter  // per-run output capture; nil discards (shape tests)
	logger      *slog.Logger
}

// mint fills rec's declaration checksum from the pipeline's declaration and creates the
// queued run (cause=manual, consumed upstreams) through the single writer, returning the
// pipeline's run target for the lane pickup that goes on to execute it.
func (m *manualExec) mint(ctx context.Context, rec store.RunRecord) (store.PipelineRunTarget, error) {
	target, found, err := m.manual.PipelineRunTarget(ctx, rec.Pipeline)
	if err != nil {
		return store.PipelineRunTarget{}, err
	}
	if !found {
		return store.PipelineRunTarget{}, fmt.Errorf("pipeline %q is not registered", rec.Pipeline)
	}
	sum, err := m.checksum(target.Folder)
	if err != nil {
		return store.PipelineRunTarget{}, err
	}
	rec.DeclarationChecksum = sum
	if err := m.submitter.Submit(ctx, func(w *store.Writer) error { return w.CreateRun(ctx, rec) }); err != nil {
		return store.PipelineRunTarget{}, fmt.Errorf("mint manual run for %q: %w", rec.Pipeline, err)
	}
	return target, nil
}

// mintRunning mints an immediate manual run directly in the RUNNING state. A
// queued mint here would race the lane loop's queued-manual pickup -- the loop
// walks every pipeline, and a queued row visible between this mint and its
// running transition would be executed twice, once by each path. Minting
// running closes that window structurally: the pickup only ever sees queued
// rows, and this run is never one.
func (m *manualExec) mintRunning(ctx context.Context, rec store.RunRecord) (store.PipelineRunTarget, error) {
	target, found, err := m.manual.PipelineRunTarget(ctx, rec.Pipeline)
	if err != nil {
		return store.PipelineRunTarget{}, err
	}
	if !found {
		return store.PipelineRunTarget{}, fmt.Errorf("pipeline %q is not registered", rec.Pipeline)
	}
	sum, err := m.checksum(target.Folder)
	if err != nil {
		return store.PipelineRunTarget{}, err
	}
	trec := store.TurnRunRecord{
		Pipeline:               rec.Pipeline,
		Cause:                  rec.Cause,
		DeclarationChecksum:    sum,
		ArtifactHash:           rec.ArtifactHash,
		ConsumedUpstreamRunIDs: rec.ConsumedUpstreamRunIDs,
	}
	if err := m.submitter.Submit(ctx, func(w *store.Writer) error { return w.CreateTurnRun(ctx, trec) }); err != nil {
		return store.PipelineRunTarget{}, fmt.Errorf("mint manual run for %q: %w", rec.Pipeline, err)
	}
	return target, nil
}

// runNow mints the run directly RUNNING (a queued mint would race the lane
// loop's queued-manual pickup into a double execution), runs its subprocess in
// the pipeline folder as ONE protocol turn (#206), and records the terminal
// transition through the single writer:
// succeeded on a done terminal (any produced rows and the consumed feed position
// committed atomically first), dead-lettered (cause stays manual, reason failed) on
// a pipeline-declared error, a protocol violation, or a process death. An own-lane
// manual run never keeps a session: the process is ended once the turn ends. A
// start failure deletes the queued run so meta is never left with a phantom; a
// failure to record running kills the started group before returning, so no
// untracked process escapes.
func (m *manualExec) runNow(ctx context.Context, rec store.RunRecord) (dispatch.RunOutcome, error) {
	target, err := m.mintRunning(ctx, rec)
	if err != nil {
		return dispatch.RunSucceeded, err
	}

	info, ok, err := m.manual.LatestRun(ctx, rec.Pipeline)
	if err != nil {
		return dispatch.RunSucceeded, fmt.Errorf("read minted manual run for %q: %w", rec.Pipeline, err)
	}
	if !ok {
		return dispatch.RunSucceeded, fmt.Errorf("manual run for %q vanished after mint", rec.Pipeline)
	}
	runID := strconv.FormatInt(info.ID, 10)

	// The turn's inputs: declared access from the declaration source, and the
	// declared-read delta past the pipeline's consumed feed position.
	raw, sum, err := declarationSource(m.workspace, target.Folder)
	if err != nil {
		return dispatch.RunSucceeded, err
	}
	acc, err := m.access.resolve(rec.Pipeline, sum, raw)
	if err != nil {
		return dispatch.RunSucceeded, err
	}
	var feed pg.TurnFeed
	if m.data != nil && len(acc.reads) > 0 {
		if feed, err = m.data.ReadTurnFeed(ctx, rec.Pipeline, acc.reads); err != nil {
			return dispatch.RunSucceeded, err
		}
	}

	// Open the per-run output capture: the run's stderr streams into the
	// run-id-keyed log under .iris/logs (stdout is protocol-only), framed per the
	// pipeline's declared recording contract, and its path is recorded as
	// runs.log_ref. Capture is best-effort -- a failed open runs the pipeline
	// uncaptured (warned) rather than blocking the run.
	sink, logRef := openRunLog(m.runLogs, runID, rec.Pipeline, target.LogSplit, target.LogStamp, m.logger)

	argv := dispatch.ResolveRunArgv(target.Argv, nil, m.objects)
	// objects is this leader's (wired at candidate construction; a promoted failover
	// leader therefore resolves built-run binaries from its own objects_path). The
	// child environment carries no database credentials (#206).
	ses, err := spawnResident(ctx, m.runner, "", filepath.Join(m.workspace, target.Folder), argv, os.Environ())
	if err != nil {
		closeRunLog(sink)
		// Nothing started: the run was minted running, so it dead-letters with the
		// start refusal (a failed run always records; there is no queued phantom).
		detail := fmt.Sprintf("manual run dead-lettered: start failed: %v", err)
		if derr := m.submitter.Submit(ctx, func(w *store.Writer) error {
			return w.DeadLetterRun(ctx, runID, store.ReasonFailed, detail)
		}); derr != nil {
			m.logger.Warn("manual run: could not dead-letter after start failure", "run", runID, "err", derr)
		}
		return dispatch.RunSucceeded, fmt.Errorf("start manual run for %q: %w", rec.Pipeline, err)
	}
	ses.out.Set(sink)
	defer func() {
		ses.end() // one-shot: the manual path never keeps a session
		ses.out.Set(nil)
		closeRunLog(sink)
	}()

	if err := m.submitter.Submit(ctx, func(w *store.Writer) error { return w.StampRunStarted(ctx, runID, ses.handle.PGID(), logRef) }); err != nil {
		return dispatch.RunSucceeded, fmt.Errorf("record manual run %s started: %w", runID, err)
	}

	// Track the live process group so a self-demotion (lost meta session) kills it at
	// once; untrack after it is reaped so a completed run is never a kill target. A
	// nil registry (the shape tests) skips tracking.
	if m.inflight != nil {
		m.inflight.track(runID, ses.handle)
		defer m.inflight.untrack(runID)
	}

	deadLetter := func(detail string) (dispatch.RunOutcome, error) {
		sink.SetOutcome("dead_lettered")
		// Guarded on the running state, so a cancel's stopped reason stands.
		if derr := m.submitter.Submit(ctx, func(w *store.Writer) error {
			return w.DeadLetterRun(ctx, runID, store.ReasonFailed, detail)
		}); derr != nil {
			return dispatch.RunSucceeded, fmt.Errorf("dead-letter manual run %s: %w", runID, derr)
		}
		_ = m.submitter.Submit(ctx, func(w *store.Writer) error {
			return dispatch.StampTerminal(ctx, w, m.journal, runID)
		})
		m.sealAfterPass(ctx)
		return dispatch.RunDeadLettered, nil
	}

	// Declared plugins pin at run start; a resolution failure refuses the run (#215).
	rp, perr := resolveTurnPlugins(m.pluginsRoot, acc.plugins, m.runner, sink, m.services,
		serviceScope{Pipeline: rec.Pipeline, Lane: target.Lane, SpillDir: payloadsDir(m.workspace)})
	if perr != nil {
		return deadLetter("manual run dead-lettered: plugin resolution failed: " + perr.Error())
	}
	defer rp.end()

	res := driveTurn(ctx, ses, ses.nextTurn(), feed.Rows, acc.writes, rp, sink)
	if res.kind != turnShutdown && rp != nil {
		prec := store.TurnRunRecord{Plugins: rp.pins, Calls: res.calls}
		if lerr := m.submitter.Submit(ctx, func(w *store.Writer) error { return w.RecordRunPlugins(ctx, runID, prec) }); lerr != nil {
			m.logger.Warn("manual run: could not record plugin ledger", "run", runID, "err", lerr)
		}
	}

	// The declared stamp's close line carries the run's ending (the deferred
	// close writes it); a dead-letter on any later path overwrites it below.
	if res.kind == turnShutdown {
		sink.SetOutcome("shutdown")
	}
	switch res.kind {
	case turnShutdown:
		return dispatch.RunSucceeded, ctx.Err()
	case turnErrored:
		return deadLetter("manual run dead-lettered: pipeline error: " + errorTurnDetail(res.end))
	case turnViolated:
		return deadLetter("manual run dead-lettered: " + res.violation.Error())
	case turnDied:
		return deadLetter("manual run dead-lettered: " + deathTurnDetail(res.status, ses.out.Tail()))
	}

	// Done: commit any produced rows (and the consumed feed position) atomically,
	// attributed to this run, before the terminal transition.
	if len(res.rows) > 0 || feed.Advanced {
		if m.data == nil {
			return deadLetter(fmt.Sprintf("manual run dead-lettered: turn produced %d rows with no data seam wired", len(res.rows)))
		}
		if _, cerr := m.data.CommitTurn(ctx, pg.TurnCommit{
			Pipeline:        rec.Pipeline,
			RunID:           info.ID,
			Writes:          turnWrites(res.rows),
			Position:        feed.Position,
			AdvancePosition: feed.Advanced,
		}); cerr != nil {
			return deadLetter(fmt.Sprintf("manual run dead-lettered: turn commit failed: %v", cerr))
		}
	}

	sink.SetOutcome("succeeded")
	if serr := m.submitter.Submit(ctx, func(w *store.Writer) error { return w.MarkRunSucceeded(ctx, runID) }); serr != nil {
		return dispatch.RunSucceeded, fmt.Errorf("record manual run %s succeeded: %w", runID, serr)
	}
	_ = m.submitter.Submit(ctx, func(w *store.Writer) error {
		return dispatch.StampTerminal(ctx, w, m.journal, runID)
	})

	m.sealAfterPass(ctx)
	return dispatch.RunSucceeded, nil
}

// sealAfterPass runs the opportunistic post-terminal seal step: it is invoked once
// the run is recorded terminal and its journal ceiling stamped, so the
// just-finished run never counts itself as in-flight. A nil sealer (the shape
// tests) or a not-due partition leaves the journal untouched.
func (m *manualExec) sealAfterPass(ctx context.Context) {
	if m.sealer == nil {
		return
	}
	m.sealer.sealAfterPass(ctx)
}

// checksum reads the pipeline's declaration file and returns its SHA-256 hex digest, the
// value stamped as the run's declaration_checksum (recorded on every run). It shares the
// declaration-checksum helper the replay path uses, so a manual run and its replay record
// the same current declaration's checksum.
func (m *manualExec) checksum(folder string) (string, error) {
	return pipelineDeclChecksum(m.workspace, folder)
}

// exitDetail renders a terminated subprocess's disposition for the dead-letter error
// column.
func exitDetail(status exec.ExitStatus) string {
	if status.Signaled {
		return fmt.Sprintf("killed by signal %v", status.Signal)
	}
	return fmt.Sprintf("exit code %d", status.Code)
}
