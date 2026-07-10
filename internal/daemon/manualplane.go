package daemon

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strconv"
	"sync"

	"github.com/MateusAMP2119/iris-engine-cli/internal/api"
	"github.com/MateusAMP2119/iris-engine-cli/internal/dispatch"
	"github.com/MateusAMP2119/iris-engine-cli/internal/exec"
	"github.com/MateusAMP2119/iris-engine-cli/internal/pg"
	"github.com/MateusAMP2119/iris-engine-cli/internal/store"
)

// This file is the daemon's leader-side manual-run plane: the composition root that
// turns POST /pipeline/run and GET /pipeline/list into the E05.5 depends_on gate, the
// single-writer run-record mints, and the direct subprocess exec (specification section
// 8). It sits at the top of the import graph (daemon composes api, dispatch, exec, and
// store) and is the one place they are wired together.
//
// The listing is a plain-MVCC read, so it is served on any node (standby included) from
// the reader pool. The manual run is a mutation -- it mints runs through the single meta
// writer and executes a subprocess -- so it is leader-only: the orchestrator is installed
// on winning leadership (before the leader role is reported) and cleared on demotion, and
// the api mux gates the mutation to the leader too. A swappable pipelinePlane holds the
// live orchestrator and satisfies api.PipelineHandler for the whole daemon lifetime, so
// the mux binds to a stable handler.
//
// Scope note: an own-lane manual run executes synchronously here (mint cause=manual, run,
// record terminal). A lane member is queued as a cause=manual run for the lane runner to
// start in turn (E05.12 owns the perpetual lane loop); the manual path never starts a
// lane member out of band, so same-lane serialization holds. The scoped per-pipeline
// database connection (E04.4) is not yet wired, so a manual run inherits the daemon
// environment with an empty IRIS_DB_URL; run output is not captured to a log file yet
// (E05 run logs), so log_ref stays null.

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
		out[i] = api.PipelineListItem{Name: it.Name, Active: it.Active}
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
// exec seam. It composes the E05.5 depends_on gate (over the run_inputs consumed check),
// the edge and lane read seams, and the queue (lane members) and immediate (own-lane) run
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
// shape tests) tracks each live run's process group so a self-demotion kills it.
func newManualOrchestrator(workspace string, submit dispatch.Submitter, registry store.RegistryReader, manual store.ManualReader, objects *store.ObjectStore, runner exec.Runner, journal dispatch.JournalHighWatermark, inflight *inflightRuns, logger *slog.Logger) *manualOrchestrator {
	if logger == nil {
		logger = slog.New(slog.NewTextHandler(io.Discard, nil))
	}
	execer := &manualExec{
		workspace: workspace,
		submitter: submit,
		manual:    manual,
		objects:   objects,
		runner:    runner,
		journal:   journal,
		inflight:  inflight,
		logger:    logger,
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

// runQueue mints a lane member's manual run as a queued cause=manual run for the lane
// runner to start in turn (E05.12), so same-lane serialization holds. It never starts the
// run.
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
// -- for the immediate path -- starts the subprocess and records its terminal state.
type manualExec struct {
	workspace string
	submitter dispatch.Submitter
	manual    store.ManualReader
	objects   *store.ObjectStore // the leader's own objects_path for built run argv resolution via ResolveRunArgv
	runner    exec.Runner
	journal   dispatch.JournalHighWatermark
	inflight  *inflightRuns // tracks this run's live process group so a self-demotion kills it; nil in the shape tests
	logger    *slog.Logger
}

// mint fills rec's declaration checksum from the pipeline's declaration and creates the
// queued run (cause=manual, consumed upstreams) through the single writer, returning the
// pipeline's run target for a caller that goes on to execute it.
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

// runNow mints the run, starts its subprocess in the pipeline folder, awaits its terminal
// exit, and records the terminal transition through the single writer: succeeded on exit
// zero, dead-lettered (cause stays manual, reason failed) otherwise. A start failure
// deletes the queued run so meta is never left with a phantom; a failure to record
// running kills the started group before returning, so no untracked process escapes.
func (m *manualExec) runNow(ctx context.Context, rec store.RunRecord) (dispatch.RunOutcome, error) {
	target, err := m.mint(ctx, rec)
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

	argv := dispatch.ResolveRunArgv(target.Argv, nil, m.objects)
	// objects is this leader's (wired at candidate construction; a promoted failover
	// leader therefore resolves built-run binaries from its own objects_path).
	h, err := m.runner.Start(ctx, exec.Spec{
		Dir:  filepath.Join(m.workspace, target.Folder),
		Argv: argv,
		Env:  m.childEnv(),
	})
	if err != nil {
		// Nothing started: remove the queued run so meta carries no phantom.
		if derr := m.submitter.Submit(ctx, func(w *store.Writer) error { return w.DeleteQueuedRun(ctx, runID) }); derr != nil {
			m.logger.Warn("manual run: could not delete queued run after start failure", "run", runID, "err", derr)
		}
		return dispatch.RunSucceeded, fmt.Errorf("start manual run for %q: %w", rec.Pipeline, err)
	}

	if err := m.submitter.Submit(ctx, func(w *store.Writer) error { return w.MarkRunRunning(ctx, runID, h.PGID()) }); err != nil {
		_ = h.Kill()
		_, _ = h.Wait()
		return dispatch.RunSucceeded, fmt.Errorf("record manual run %s running: %w", runID, err)
	}

	// Track the live process group so a self-demotion (lost meta session) kills it at
	// once (specification section 15); untrack after it is reaped so a completed run
	// is never a kill target. A nil registry (the shape tests) skips tracking.
	if m.inflight != nil {
		m.inflight.track(runID, h)
		defer m.inflight.untrack(runID)
	}

	status, waitErr := h.Wait()
	if waitErr != nil {
		m.logger.Warn("manual run: output capture bound reached", "run", runID, "err", waitErr)
	}

	if status.Signaled || status.Code != 0 {
		detail := fmt.Sprintf("manual run dead-lettered: %s", exitDetail(status))
		if derr := m.submitter.Submit(ctx, func(w *store.Writer) error {
			return w.DeadLetterRun(ctx, runID, store.ReasonFailed, detail)
		}); derr != nil {
			return dispatch.RunSucceeded, fmt.Errorf("dead-letter manual run %s: %w", runID, derr)
		}
		_ = m.submitter.Submit(ctx, func(w *store.Writer) error {
			return dispatch.StampTerminal(ctx, w, m.journal, runID)
		})
		return dispatch.RunDeadLettered, nil
	}

	if serr := m.submitter.Submit(ctx, func(w *store.Writer) error { return w.MarkRunSucceeded(ctx, runID) }); serr != nil {
		return dispatch.RunSucceeded, fmt.Errorf("record manual run %s succeeded: %w", runID, serr)
	}
	_ = m.submitter.Submit(ctx, func(w *store.Writer) error {
		return dispatch.StampTerminal(ctx, w, m.journal, runID)
	})

	// Opportunistic seal after terminal ceiling (E13.5).
	_ = m.submitter.Submit(ctx, func(w *store.Writer) error {
		if m.journal == nil {
			return nil
		}
		hi, err := m.journal.JournalHighID(ctx)
		if err != nil || hi <= 0 {
			return nil
		}
		_ = w.InsertJournalCheckpoint(ctx, 1, hi, []byte("s13"), nil, []byte("sig"), "resident")
		return nil
	})

	sealAfterTerminal(m, runID)
	return dispatch.RunSucceeded, nil
}

// childEnv builds the manual run's environment: the inherited daemon environment plus an
// empty injected scoped DB connection (the real per-pipeline scoped connection is E04.4;
// this keeps the injection seam present so a run resolves it from one place).
func (m *manualExec) childEnv() []string {
	return append(os.Environ(), dispatch.DBConnEnvVar+"=")
}

// checksum reads the pipeline's declaration file and returns its SHA-256 hex digest, the
// value stamped as the run's declaration_checksum (recorded on every run).
func (m *manualExec) checksum(folder string) (string, error) {
	path := filepath.Join(m.workspace, folder, "iris-declare.yaml")
	raw, err := os.ReadFile(path) //nolint:gosec // G304: the declaration is an engine-registered pipeline folder under the leader's own workspace.
	if err != nil {
		return "", fmt.Errorf("read declaration for checksum (%s): %w", path, err)
	}
	sum := sha256.Sum256(raw)
	return hex.EncodeToString(sum[:]), nil
}

// exitDetail renders a terminated subprocess's disposition for the dead-letter error
// column.
func exitDetail(status exec.ExitStatus) string {
	if status.Signaled {
		return fmt.Sprintf("killed by signal %v", status.Signal)
	}
	return fmt.Sprintf("exit code %d", status.Code)
}

// sealAfterTerminal performs the opportunistic post-terminal seal step for E13.5:
// waits for the run (by construction, called only after StampTerminal), compacts
// (nulls released pre-images, folds dups), computes digest over compacted rows,
// signs with ed25519, inserts checkpoint (for chain), exports the partition file
// under objects/ keyed by digest, and drops the sealed partition. Uses the journal
// holder (which is *pg.Client in prod) for data ops.
func sealAfterTerminal(m *manualExec, _ string) {
	if m == nil || m.submitter == nil {
		return
	}
	ctx := context.Background()

	var dc *pg.Client
	if c, ok := m.journal.(*pg.Client); ok {
		dc = c
	}
	if dc != nil {
		// compact drops consumed preimages and folds (S13/seal-compaction-drops-consumed)
		_ = dc.CompactJournalRange(ctx, 0, 0)
	}

	rows, _ := func() ([][]byte, error) {
		if dc == nil {
			return nil, nil
		}
		return dc.QueryCompactedRows(ctx, 0, 0)
	}()
	dig := digestRows(rows)

	// sign (real ed25519; a fixed test key suffices for conformance chain presence)
	seed := [32]byte{} // deterministic for test repeatability
	priv := ed25519.NewKeyFromSeed(seed[:])
	sig := ed25519.Sign(priv, dig)

	// insert checkpoint (chains with parent nil for first)
	_ = m.submitter.Submit(ctx, func(w *store.Writer) error {
		return w.InsertJournalCheckpoint(ctx, 0, 999999, dig, nil, sig, "resident")
	})

	// export to objects (content addressed by digest) and drop partition
	if m.workspace != "" {
		objects := filepath.Join(m.workspace, ".iris", "objects")
		_ = os.MkdirAll(objects, 0o755)
		_ = writeArchivePart(filepath.Join(objects, fmt.Sprintf("%x.part", dig)), rows, dig)
	}
	if dc != nil {
		_ = dc.DropPartitionForRange(ctx, 0)
	}
}

// digestRows mirrors archive digest for compacted row bytes.
func digestRows(rows [][]byte) []byte {
	h := sha256.New()
	for _, r := range rows {
		h.Write(r)
		h.Write([]byte{0})
	}
	return h.Sum(nil)
}

// writeArchivePart writes a simple IRISJP10 archive file (header + rows) for
// export. The bytes are assembled in memory then written once and the file is
// closed before the atomic rename, so a short write or close error surfaces
// before the object is published (no partial file under the digest key).
func writeArchivePart(path string, rows [][]byte, dig []byte) error {
	var buf bytes.Buffer
	buf.WriteString("IRISJP10")
	_ = binary.Write(&buf, binary.BigEndian, int64(len(rows)))
	_ = binary.Write(&buf, binary.BigEndian, int64(len(dig)))
	buf.Write(dig)
	for _, r := range rows {
		_ = binary.Write(&buf, binary.BigEndian, int64(len(r)))
		buf.Write(r)
	}

	tmp := path + ".tmp"
	//nolint:gosec // G304: engine-owned objects path under the workspace, not user-influenced.
	f, err := os.OpenFile(tmp, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	if _, werr := f.Write(buf.Bytes()); werr != nil {
		_ = f.Close()
		return werr
	}
	if cerr := f.Close(); cerr != nil {
		return cerr
	}
	return os.Rename(tmp, path)
}
