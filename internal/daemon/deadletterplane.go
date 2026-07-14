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

	"github.com/MateusAMP2119/iris-engine-cli/internal/api"
	"github.com/MateusAMP2119/iris-engine-cli/internal/dispatch"
	"github.com/MateusAMP2119/iris-engine-cli/internal/store"
)

// This file is the daemon's dead-letter plane: the composition root that turns the
// three dead-letter HTTP surfaces into the pure dispatch resolution and the single
// writer. GET /dead_letters/{run}/impact -- the blast readout `iris deadletter
// show` renders -- is a plain-MVCC read served on any node from the reader pool.
// POST /deadletter/replay and POST /deadletter/drain are leader-only mutations:
// their executor is installed on winning leadership (before the leader role is
// reported) and cleared on demotion, and the api mux gates the mutation to the
// leader too, so a request racing a lost lock faults rather than writing off-path.
//
// The plane owns only the wiring: the failed_upstream root walk, the supersession
// rule, the blast classification, and the drain scope resolution are dispatch's pure
// functions (replay.go, drain.go); the replacement mint and the worklist deletes are
// the single Writer's (store); this file pairs them.

// deadletterPlane is the daemon's dead-letter handler: it serves the blast readout
// from the reader pool always, and delegates replay and drain to the leader-installed
// executor when the daemon leads (faulting otherwise). It is a stable handle the mux
// binds to for the daemon's whole life.
type deadletterPlane struct {
	reader   store.DeadLetterReader
	registry store.RegistryReader
	logger   *slog.Logger

	mu   sync.RWMutex
	exec *deadletterExec
}

// compile-time proof the plane satisfies the mux's three dead-letter seams.
var (
	_ api.DeadImpactHandler = (*deadletterPlane)(nil)
	_ api.ReplayHandler     = (*deadletterPlane)(nil)
	_ api.DrainHandler      = (*deadletterPlane)(nil)
)

// newDeadletterPlane returns a dead-letter plane over the plain-MVCC worklist and
// registry readers. The blast readout works at once (any node); replay and drain fault
// until a leader installs an executor.
func newDeadletterPlane(reader store.DeadLetterReader, registry store.RegistryReader, logger *slog.Logger) *deadletterPlane {
	if logger == nil {
		logger = slog.New(slog.NewTextHandler(io.Discard, nil))
	}
	return &deadletterPlane{reader: reader, registry: registry, logger: logger}
}

// install wires the leader-side replay/drain executor (on winning leadership).
func (p *deadletterPlane) install(ex *deadletterExec) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.exec = ex
}

// clear removes the executor (on demotion), so a replay/drain racing a lost lock
// faults rather than minting off the single-writer path.
func (p *deadletterPlane) clear() {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.exec = nil
}

// executor returns the installed executor, or nil when not leading.
func (p *deadletterPlane) executor() *deadletterExec {
	p.mu.RLock()
	defer p.mu.RUnlock()
	return p.exec
}

// Impact serves GET /dead_letters/{run}/impact: it walks the entry to its root cause
// and classifies the blast neighborhood (dispatch.ClassifyBlastRadius). A plain-MVCC
// read, so it is served on any node.
func (p *deadletterPlane) Impact(ctx context.Context, runID string) (any, error) {
	seedID, err := strconv.ParseInt(runID, 10, 64)
	if err != nil {
		return nil, fmt.Errorf("deadletter show: %q is not a run id", runID)
	}
	entries, pipelineOf, err := p.worklist(ctx)
	if err != nil {
		return nil, err
	}
	seed, found := findEntry(entries, seedID)
	if !found {
		return nil, fmt.Errorf("deadletter show: run %s is not a dead-lettered run", runID)
	}

	edges, err := p.blastEdges(ctx)
	if err != nil {
		return nil, err
	}

	// Resolve the root cause the entry walks to (reusing the replay resolver).
	roots, err := dispatch.ResolveReplayTargets(entries, []int64{seedID})
	if err != nil {
		return nil, fmt.Errorf("deadletter show: %w", err)
	}
	rootRunID := roots[0]
	rootPipeline := pipelineOf[rootRunID]

	laneMembers, err := p.rootLaneMembers(ctx, rootPipeline)
	if err != nil {
		return nil, err
	}
	shielded, err := p.shieldedDownstreams(ctx, rootPipeline, rootRunID)
	if err != nil {
		return nil, err
	}

	impacts, err := dispatch.ClassifyBlastRadius(seed, entries, edges, laneMembers, shielded)
	if err != nil {
		return nil, fmt.Errorf("deadletter show: %w", err)
	}
	items := make([]api.DeadImpactItem, 0, len(impacts))
	for _, im := range impacts {
		items = append(items, api.DeadImpactItem{Pipeline: im.Pipeline, Class: string(im.Class)})
	}
	return api.DeadImpactPayload{
		Run:       runID,
		Reason:    string(seed.Reason),
		RootCause: api.DeadImpactRoot{Run: strconv.FormatInt(rootRunID, 10), Pipeline: rootPipeline},
		Impacts:   items,
	}, nil
}

// Replay serves POST /deadletter/replay: it resolves the scope to root causes,
// mints each a replacement on current data, and discards as superseded every
// propagated entry that walked to a replayed root. Leader-only.
func (p *deadletterPlane) Replay(ctx context.Context, req api.ReplayRequest) (api.ReplayResult, error) {
	ex := p.executor()
	if ex == nil {
		return api.ReplayResult{}, api.ErrReplayUnavailable
	}
	entries, pipelineOf, err := p.worklist(ctx)
	if err != nil {
		return api.ReplayResult{}, err
	}
	selected, err := selectScope(entries, req.Run, req.Pipeline, req.All)
	if err != nil {
		return api.ReplayResult{}, err
	}
	roots, err := dispatch.ResolveReplayTargets(entries, selected)
	if err != nil {
		return api.ReplayResult{}, err
	}

	replayedRoots := make(map[int64]bool, len(roots))
	replayed := make([]api.ReplayedRun, 0, len(roots))
	for _, root := range roots {
		replacement, err := ex.mintReplacement(ctx, root, pipelineOf[root])
		if err != nil {
			return api.ReplayResult{}, err
		}
		replayedRoots[root] = true
		replayed = append(replayed, api.ReplayedRun{
			ReplacedRun:    strconv.FormatInt(root, 10),
			ReplacementRun: strconv.FormatInt(replacement, 10),
			ReplayedFrom:   strconv.FormatInt(root, 10),
		})
	}

	// Discard the propagated entries that walk to a replayed root as superseded: the
	// replay minted a fresh root run their dependent will consume next pass, so their
	// rejection is superseded now. The root entries were already removed when their
	// replacements minted.
	var superseded []int64
	for _, e := range entries {
		if !e.IsPropagated() {
			continue
		}
		r, rerr := dispatch.ResolveReplayTargets(entries, []int64{e.RunID})
		if rerr == nil && len(r) > 0 && replayedRoots[r[0]] {
			superseded = append(superseded, e.RunID)
		}
	}
	if len(superseded) > 0 {
		if err := ex.submit.Submit(ctx, func(w *store.Writer) error {
			return w.DrainDeadLetters(ctx, superseded)
		}); err != nil {
			return api.ReplayResult{}, fmt.Errorf("deadletter replay: discard superseded entries: %w", err)
		}
	}

	// A minted replacement is queued, not yet executed, so no replay re-dead-letters at
	// replay time: the batch is clean (exit 0). A replacement that later fails parks its
	// own fresh entry through the lane loop's terminal path.
	return api.ReplayResult{Replayed: replayed, DeadLettered: nil}, nil
}

// Drain serves POST /deadletter/drain: it resolves the scope to the exact
// outstanding entries and discards their worklist rows through the single writer.
// Leader-only. The confirm gate is the mux's; the run rows stay in runs (a worklist
// exit never deletes run history) and a drained run can never be replayed.
func (p *deadletterPlane) Drain(ctx context.Context, req api.DrainRequest) (api.DrainResult, error) {
	ex := p.executor()
	if ex == nil {
		return api.DrainResult{}, api.ErrDrainUnavailable
	}
	entries, pipelineOf, err := p.worklist(ctx)
	if err != nil {
		return api.DrainResult{}, err
	}
	var runScope int64
	if req.Run != "" {
		runScope, err = strconv.ParseInt(req.Run, 10, 64)
		if err != nil {
			return api.DrainResult{}, fmt.Errorf("deadletter drain: %q is not a run id", req.Run)
		}
	}
	targets, err := dispatch.ResolveDrainTargets(entries, dispatch.DrainScope{Run: runScope, Pipeline: req.Pipeline, All: req.All})
	if err != nil {
		return api.DrainResult{}, err
	}

	// The destructive-op gate: an in-flight run on the drain's scope refuses a
	// --yes invocation with guidance; --force cancels the runs and proceeds. The
	// scope is the named pipeline, engine-wide for --all, or -- for a single-run
	// drain -- the drained run's own pipeline.
	scope := dispatch.GateScope{Pipeline: req.Pipeline}
	if req.Run != "" && req.Pipeline == "" {
		scope.Pipeline = pipelineOf[runScope]
	}
	if err := ex.gate.enforce(ctx, dispatch.OpDeadletterDrain, scope, req.Force, 0); err != nil {
		return api.DrainResult{}, err
	}

	if len(targets) > 0 {
		if err := ex.submit.Submit(ctx, func(w *store.Writer) error {
			return w.DrainDeadLetters(ctx, targets)
		}); err != nil {
			return api.DrainResult{}, fmt.Errorf("deadletter drain: %w", err)
		}
	}
	drained := make([]string, 0, len(targets))
	for _, t := range targets {
		drained = append(drained, strconv.FormatInt(t, 10))
	}
	return api.DrainResult{Drained: drained}, nil
}

// worklist reads the outstanding worklist and converts it to the dispatcher's pure
// entry type, plus a run-id -> pipeline index.
func (p *deadletterPlane) worklist(ctx context.Context) ([]dispatch.DeadLetterEntry, map[int64]string, error) {
	rows, err := p.reader.Worklist(ctx)
	if err != nil {
		return nil, nil, fmt.Errorf("deadletter: read worklist: %w", err)
	}
	entries := make([]dispatch.DeadLetterEntry, 0, len(rows))
	pipelineOf := make(map[int64]string, len(rows))
	for _, r := range rows {
		entries = append(entries, dispatch.DeadLetterEntry{
			RunID:               r.RunID,
			Pipeline:            r.Pipeline,
			Reason:              r.Reason,
			FailedUpstreamRunID: r.FailedUpstreamRunID,
		})
		pipelineOf[r.RunID] = r.Pipeline
	}
	return entries, pipelineOf, nil
}

// blastEdges reads the depends_on edges as forward blast edges (dependent -> upstream).
func (p *deadletterPlane) blastEdges(ctx context.Context) ([]dispatch.BlastEdge, error) {
	deps, err := p.registry.DependencyEdges(ctx)
	if err != nil {
		return nil, fmt.Errorf("deadletter: read dependency edges: %w", err)
	}
	edges := make([]dispatch.BlastEdge, 0, len(deps))
	for _, d := range deps {
		edges = append(edges, dispatch.BlastEdge{Dependent: d.From, Upstream: d.To})
	}
	return edges, nil
}

// rootLaneMembers returns the members of the root pipeline's lane (for the untouched
// composer neighbors). A root pipeline in no lane yields no members.
func (p *deadletterPlane) rootLaneMembers(ctx context.Context, rootPipeline string) ([]string, error) {
	members, err := p.reader.LaneMembers(ctx)
	if err != nil {
		return nil, fmt.Errorf("deadletter: read lane members: %w", err)
	}
	laneOf := make(map[string]string, len(members))
	byLane := make(map[string][]string, len(members))
	for _, m := range members {
		laneOf[m.Pipeline] = m.Lane
		byLane[m.Lane] = append(byLane[m.Lane], m.Pipeline)
	}
	return byLane[laneOf[rootPipeline]], nil
}

// shieldedDownstreams returns the set of pipelines that have consumed a run of the
// root pipeline later than the poisoned root run: their rejection is superseded, so
// the blast radius classifies them shielded rather than poisoned.
func (p *deadletterPlane) shieldedDownstreams(ctx context.Context, rootPipeline string, rootRunID int64) (map[string]bool, error) {
	cons, err := p.reader.Consumptions(ctx)
	if err != nil {
		return nil, fmt.Errorf("deadletter: read consumption edges: %w", err)
	}
	shielded := make(map[string]bool)
	for _, c := range cons {
		if c.UpstreamPipeline == rootPipeline && c.UpstreamRunID > rootRunID {
			shielded[c.Dependent] = true
		}
	}
	return shielded, nil
}

// deadletterExec is the leader-side replay/drain executor: it mints replacements and
// discards worklist rows through the single writer, resolving pipeline folders under
// the workspace and pinning the replacement to current data.
type deadletterExec struct {
	submit    dispatch.Submitter
	manual    store.ManualReader
	workspace string
	lsn       dispatch.LSNReader            // data-database current LSN (fresh snapshot pin); nil leaves it empty
	journal   dispatch.JournalHighWatermark // data journal high id (journal floor); nil leaves it zero
	gate      destructiveGate               // the drain's soft-block gate (--yes refuses on in-flight runs, --force cancels)
	logger    *slog.Logger
}

// mintReplacement mints a fresh replacement run for a dead-lettered root cause on
// current data and returns the replacement's id. store.ReplayRun removes the replaced
// run's worklist row in the same atomic transaction, so the root entry exits the
// worklist as the replacement mints. The replacement is a dev run (no artifact hash) on
// the pipeline's current declaration, pinned to the data database's current LSN and
// journal high id; its consumed upstreams are recorded when the lane loop dispatches it.
func (ex *deadletterExec) mintReplacement(ctx context.Context, replacedRun int64, pipeline string) (int64, error) {
	if pipeline == "" {
		return 0, fmt.Errorf("deadletter replay: run %d has no pipeline", replacedRun)
	}
	target, found, err := ex.manual.PipelineRunTarget(ctx, pipeline)
	if err != nil {
		return 0, fmt.Errorf("deadletter replay: read run target for %q: %w", pipeline, err)
	}
	if !found {
		return 0, fmt.Errorf("deadletter replay: pipeline %q is not registered", pipeline)
	}
	sum, err := pipelineDeclChecksum(ex.workspace, target.Folder)
	if err != nil {
		return 0, fmt.Errorf("deadletter replay: %w", err)
	}
	var lsn string
	if ex.lsn != nil {
		lsn, _ = ex.lsn.CurrentLSN(ctx) // best-effort pin; empty is a valid (nullable) snapshot
	}
	var floor int64
	if ex.journal != nil {
		floor, _ = ex.journal.JournalHighID(ctx)
	}
	rec := store.ReplayRecord{
		ReplacedRunID:       replacedRun,
		Pipeline:            pipeline,
		DeclarationChecksum: sum,
		SnapshotLSN:         lsn,
		JournalFloor:        floor,
	}
	if err := ex.submit.Submit(ctx, func(w *store.Writer) error { return w.ReplayRun(ctx, rec) }); err != nil {
		return 0, fmt.Errorf("deadletter replay: mint replacement for run %d: %w", replacedRun, err)
	}
	// The replacement is now the pipeline's most recent run (meta assigns its identity
	// at mint), so LatestRun resolves it.
	info, ok, err := ex.manual.LatestRun(ctx, pipeline)
	if err != nil {
		return 0, fmt.Errorf("deadletter replay: read replacement for %q: %w", pipeline, err)
	}
	if !ok {
		return 0, fmt.Errorf("deadletter replay: replacement for %q vanished after mint", pipeline)
	}
	return info.ID, nil
}

// selectScope resolves the replay scope to the selected worklist run ids: a single run
// (parsed), one pipeline's outstanding entries, or every entry. Exactly one form is set
// (the CLI and the mux both refuse a bare scope before this).
func selectScope(entries []dispatch.DeadLetterEntry, run, pipeline string, all bool) ([]int64, error) {
	switch {
	case all:
		out := make([]int64, 0, len(entries))
		for _, e := range entries {
			out = append(out, e.RunID)
		}
		return out, nil
	case pipeline != "":
		var out []int64
		for _, e := range entries {
			if e.Pipeline == pipeline {
				out = append(out, e.RunID)
			}
		}
		return out, nil
	case run != "":
		id, err := strconv.ParseInt(run, 10, 64)
		if err != nil {
			return nil, fmt.Errorf("deadletter replay: %q is not a run id", run)
		}
		return []int64{id}, nil
	default:
		return nil, fmt.Errorf("deadletter replay requires a scope: <run>, --pipeline, or --all")
	}
}

// findEntry returns the worklist entry for runID, if present.
func findEntry(entries []dispatch.DeadLetterEntry, runID int64) (dispatch.DeadLetterEntry, bool) {
	for _, e := range entries {
		if e.RunID == runID {
			return e, true
		}
	}
	return dispatch.DeadLetterEntry{}, false
}

// pipelineDeclChecksum reads a pipeline folder's declaration and returns its SHA-256
// hex digest: the value stamped as a run's declaration_checksum. It mirrors the manual
// plane's checksum so a replay records the current declaration's checksum (a fresh run
// on current data).
func pipelineDeclChecksum(workspace, folder string) (string, error) {
	path := filepath.Join(workspace, folder, "iris-declare.yaml")
	raw, err := os.ReadFile(path) //nolint:gosec // G304: an engine-registered pipeline folder under the leader's own workspace.
	if err != nil {
		return "", fmt.Errorf("read declaration for checksum (%s): %w", path, err)
	}
	sum := sha256.Sum256(raw)
	return hex.EncodeToString(sum[:]), nil
}
