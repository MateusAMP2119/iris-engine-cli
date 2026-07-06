package dispatch

// This file is the declaration apply op: the leader-side path that turns a validated
// declaration into persisted registry state (specification sections 3 and 6.3). A
// pipeline apply reads the current registry, validates the declaration's depends_on
// edges against it (upstream-first plus acyclicity), and -- only on success --
// submits the pipelines row and its dependency edges to the single meta writer as
// one atomic transaction; a validation failure returns before any write, so meta is
// unchanged. A composer apply rewrites its lane's whole order atomically. This is
// the dispatch-level surface; the CLI and daemon control-connection wiring that
// drives it is a later task.

import (
	"context"
	"fmt"

	"github.com/MateusAMP2119/iris-engine-cli/internal/declare"
	"github.com/MateusAMP2119/iris-engine-cli/internal/store"
)

// Applier persists declared pipelines and lane composers into the registry through
// the single meta writer. Build it with NewApplier over the registry read seam
// (used to rebuild the dependency graph a pipeline apply validates against) and the
// single-writer submission seam (the Dispatcher).
type Applier struct {
	reg    store.RegistryReader
	submit Submitter
}

// NewApplier builds the apply op over the registry reader and the single-writer
// submitter.
func NewApplier(reg store.RegistryReader, submit Submitter) *Applier {
	return &Applier{reg: reg, submit: submit}
}

// ApplyPipeline validates decl against the current registry and, on success,
// persists it as one atomic meta transaction: the pipelines row (folder is the
// pipeline's folder value) plus its depends_on edges. A fresh registration is
// recorded as source/disposable; build and promote own those columns thereafter, so
// a re-apply preserves them. The apply never writes the lanes table. A validation
// failure -- a depends_on on an unregistered pipeline, or one that closes a cycle --
// returns before any write, so meta is unchanged.
func (a *Applier) ApplyPipeline(ctx context.Context, folder string, decl *declare.Pipeline) error {
	graph, err := a.buildGraph(ctx)
	if err != nil {
		return fmt.Errorf("dispatch: apply pipeline %q: %w", decl.Name, err)
	}
	if err := declare.ValidateDependencies(graph, decl); err != nil {
		return fmt.Errorf("dispatch: apply pipeline %q: %w", decl.Name, err)
	}
	row := store.PipelineRow{
		Name:     decl.Name,
		Folder:   folder,
		Run:      decl.Run,
		Artifact: store.ArtifactSource,
		DataMode: store.DataDisposable,
	}
	if err := a.submit.Submit(ctx, func(w *store.Writer) error {
		return w.RegisterPipeline(ctx, row, decl.DependsOn)
	}); err != nil {
		return fmt.Errorf("dispatch: apply pipeline %q: %w", decl.Name, err)
	}
	return nil
}

// ApplyComposer persists a lane composer: it rewrites the lane's entire member order
// in lanes as one atomic full-lane rewrite through the single meta writer,
// all-or-nothing, members registered or not (specification sections 3 and 6.3). The
// composer's 2+ interlock and containment rules are validated upstream (lane
// composer validation); this op owns only the atomic persistence.
func (a *Applier) ApplyComposer(ctx context.Context, composer *declare.Composer) error {
	if err := a.submit.Submit(ctx, func(w *store.Writer) error {
		return w.RewriteLane(ctx, composer.Lane, composer.Order)
	}); err != nil {
		return fmt.Errorf("dispatch: apply composer %q: %w", composer.Lane, err)
	}
	return nil
}

// buildGraph rebuilds the registered dependency graph from the current registry: it
// reads the registered pipeline names and their depends_on edges and folds them into
// the in-memory Graph ValidateDependencies reads. Because apply runs on the sole
// dispatcher path (no concurrent meta writer), the view it reads is stable across
// the read-validate-write.
func (a *Applier) buildGraph(ctx context.Context) (*declare.Registry, error) {
	names, err := a.reg.RegisteredPipelines(ctx)
	if err != nil {
		return nil, fmt.Errorf("read registry: %w", err)
	}
	edges, err := a.reg.DependencyEdges(ctx)
	if err != nil {
		return nil, fmt.Errorf("read registry: %w", err)
	}
	byFrom := make(map[string][]string, len(names))
	for _, e := range edges {
		byFrom[e.From] = append(byFrom[e.From], e.To)
	}
	reg := declare.NewRegistry()
	for _, name := range names {
		reg.Add(name, byFrom[name]...)
	}
	return reg, nil
}
