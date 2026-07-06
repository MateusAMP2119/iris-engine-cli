// This file adds an in-memory fake of the registry read seam
// (store.RegistryReader): the pipelines and dependencies view the apply op rebuilds
// its dependency graph from. A test seeds the registered pipelines and their
// depends_on edges, then drives an apply against that view with no live Postgres
// (S16/integration-fakes-interfaces).
package storetest

import (
	"context"
	"sync"

	"github.com/MateusAMP2119/iris-engine-cli/internal/store"
)

// RegistryFake is an in-memory store.RegistryReader: registered pipeline names and
// their depends_on edges, seeded with Register. The zero value is not usable;
// construct one with NewRegistryFake.
type RegistryFake struct {
	mu        sync.Mutex
	names     []string
	edges     []store.DependencyEdge
	seenNames map[string]bool
}

// NewRegistryFake returns an empty registry view: no pipelines registered.
func NewRegistryFake() *RegistryFake {
	return &RegistryFake{seenNames: map[string]bool{}}
}

// compile-time proof the fake satisfies the registry read seam.
var _ store.RegistryReader = (*RegistryFake)(nil)

// Register seeds a registered pipeline with the given depends_on upstreams (from =
// name, the dependent) and returns the fake so calls chain. A name is recorded once
// even if seeded again. Re-seeding replaces the name's edges wholesale, mirroring
// the production apply's delete-then-insert (store.Writer.RegisterPipeline): a
// re-seed with a different set persists that set, never the stale union, so the
// view a validation reads matches what the writer would persist.
func (f *RegistryFake) Register(name string, dependsOn ...string) *RegistryFake {
	f.mu.Lock()
	defer f.mu.Unlock()
	if !f.seenNames[name] {
		f.seenNames[name] = true
		f.names = append(f.names, name)
	}
	kept := make([]store.DependencyEdge, 0, len(f.edges)+len(dependsOn))
	for _, e := range f.edges {
		if e.From != name {
			kept = append(kept, e) // preserve other pipelines' edges.
		}
	}
	for _, dep := range dependsOn {
		kept = append(kept, store.DependencyEdge{From: name, To: dep})
	}
	f.edges = kept
	return f
}

// RegisteredPipelines returns a copy of the registered pipeline names.
func (f *RegistryFake) RegisteredPipelines(_ context.Context) ([]string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.names...), nil
}

// DependencyEdges returns a copy of the seeded depends_on edges.
func (f *RegistryFake) DependencyEdges(_ context.Context) ([]store.DependencyEdge, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]store.DependencyEdge(nil), f.edges...), nil
}
