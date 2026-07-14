package daemon

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"path/filepath"
	"sync"

	"github.com/MateusAMP2119/iris-engine-cli/internal/api"
	"github.com/MateusAMP2119/iris-engine-cli/internal/dispatch"
	"github.com/MateusAMP2119/iris-engine-cli/internal/exec"
	"github.com/MateusAMP2119/iris-engine-cli/internal/store"
)

// This file is the daemon's leader-side build plane: the composition root that
// turns POST /pipeline/build into the dispatch build op. It sits at the top of the
// import graph (daemon composes api, dispatch, exec, and store) and is the one
// place they are wired together for the explicit build path: the pipeline's run
// target is read from meta (folder + run vector, the recipe inference input), the
// pinned recipe's toolchain runs through the exec seam in the pipeline's workspace
// folder, the produced binary's bytes land in the content-addressed object store at
// objects_path, and the content hash rides the single meta writer into artifacts.
//
// Building is a mutation -- it writes meta and the object store and executes a
// subprocess -- so it is leader-only, exactly like the manual-run plane: the
// orchestrator is installed on winning leadership (before the leader role is
// reported) and cleared on demotion, and the api mux gates the mutation to the
// leader too. A swappable buildPlane holds the live orchestrator and satisfies
// api.BuildHandler for the whole daemon lifetime, so the mux binds to a stable
// handler.

// buildPlane is the daemon's api.BuildHandler: it delegates the build to the live
// orchestrator when the daemon leads, faulting otherwise. It is a stable handle
// the mux binds to for the daemon's whole life.
type buildPlane struct {
	logger *slog.Logger

	mu   sync.RWMutex
	live *buildOrchestrator
}

// compile-time proof the build plane is the mux's build handler.
var _ api.BuildHandler = (*buildPlane)(nil)

// newBuildPlane returns a build plane with no orchestrator: the mutation faults
// until a leader installs one. A nil logger discards output.
func newBuildPlane(logger *slog.Logger) *buildPlane {
	if logger == nil {
		logger = slog.New(slog.NewTextHandler(io.Discard, nil))
	}
	return &buildPlane{logger: logger}
}

// install wires the live build orchestrator (on winning leadership).
func (p *buildPlane) install(o *buildOrchestrator) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.live = o
}

// clear removes the orchestrator (on demotion), so a build racing a lost lock
// faults rather than writing off the single-writer path.
func (p *buildPlane) clear() {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.live = nil
}

// orchestrator returns the installed orchestrator, or nil when not leading.
func (p *buildPlane) orchestrator() *buildOrchestrator {
	p.mu.RLock()
	defer p.mu.RUnlock()
	return p.live
}

// BuildPipeline routes to the live orchestrator, or faults when none is installed
// (the mux gates the mutation to the leader too).
func (p *buildPlane) BuildPipeline(ctx context.Context, req api.PipelineBuildRequest) (api.PipelineBuildResult, error) {
	o := p.orchestrator()
	if o == nil {
		return api.PipelineBuildResult{}, api.ErrControlUnavailable
	}
	return o.build(ctx, req)
}

// buildTargetReader resolves a pipeline's build input from meta: its folder and
// declared run vector (the recipe inference input). store.ManualReader satisfies
// it; a canned fake stands in for tests.
type buildTargetReader interface {
	// PipelineRunTarget returns a pipeline's folder and run argv, and whether it
	// is registered.
	PipelineRunTarget(ctx context.Context, name string) (store.PipelineRunTarget, bool, error)
}

// buildOrchestrator runs the leader-side explicit build against meta, the object
// store, and the exec seam: it resolves the pipeline's registered run target and
// drives the dispatch build op, translating the recorded artifact to the wire
// result the CLI prints.
type buildOrchestrator struct {
	workspace string
	targets   buildTargetReader
	builder   *dispatch.Builder
	logger    *slog.Logger
}

// newBuildOrchestrator wires the build op over the single dispatcher (the sole
// meta writer), the run-target read seam, the content-addressed object store at
// objects_path, and the process runner, resolving pipeline folders under
// workspace. A nil logger discards output.
func newBuildOrchestrator(workspace string, submit dispatch.Submitter, targets buildTargetReader, objects dispatch.ObjectPutter, runner exec.Runner, logger *slog.Logger) *buildOrchestrator {
	if logger == nil {
		logger = slog.New(slog.NewTextHandler(io.Discard, nil))
	}
	return &buildOrchestrator{
		workspace: workspace,
		targets:   targets,
		builder:   dispatch.NewBuilder(submit, objects, runner),
		logger:    logger,
	}
}

// build performs one explicit pipeline build and maps the recorded artifact to
// the wire result. An unregistered pipeline is an operation failure -- the route
// renders it 422 -- never a silent success.
func (o *buildOrchestrator) build(ctx context.Context, req api.PipelineBuildRequest) (api.PipelineBuildResult, error) {
	if req.Pipeline == "" {
		return api.PipelineBuildResult{}, fmt.Errorf("pipeline build: missing pipeline name")
	}
	target, found, err := o.targets.PipelineRunTarget(ctx, req.Pipeline)
	if err != nil {
		return api.PipelineBuildResult{}, fmt.Errorf("pipeline build %q: read run target: %w", req.Pipeline, err)
	}
	if !found {
		return api.PipelineBuildResult{}, fmt.Errorf("pipeline %q is not registered", req.Pipeline)
	}

	row, err := o.builder.Build(ctx, dispatch.BuildTarget{
		Pipeline: req.Pipeline,
		Dir:      filepath.Join(o.workspace, target.Folder),
		Run:      target.Argv,
	})
	if err != nil {
		return api.PipelineBuildResult{}, err
	}
	o.logger.Info("pipeline built", "pipeline", req.Pipeline, "hash", row.Hash, "size_bytes", row.SizeBytes)
	return api.PipelineBuildResult{Pipeline: req.Pipeline, Hash: row.Hash, SizeBytes: row.SizeBytes}, nil
}
