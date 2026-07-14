package storetest

import (
	"context"
	"sync"

	"github.com/MateusAMP2119/iris-lakehouse/internal/store"
)

// This file extends the meta-store fake with the pipeline-show read seam
// (store.ShowReader): a pipeline's declaration detail, its role grants, the
// dependency edges, each upstream's latest run, and the run_inputs already-consumed
// check, beside the run records the embedded Fake already serves (which back the
// show readout's recent-runs list). It keeps the fake's value semantics -- every
// read returns copies -- so the pipeline-show readout is proven with no live
// Postgres.

// ShowFake is an in-memory store.ShowReader: the run-record Fake (recent runs) plus
// the declaration detail, grants, dependency edges, upstream latest runs, and the
// consumed check the show readout and its gate ledger draw from. The zero value is
// not usable; construct one with NewShow.
type ShowFake struct {
	*Fake

	mu       sync.Mutex
	details  map[string]store.PipelineDetail
	found    map[string]bool
	grants   map[string][]store.Grant
	edges    []store.DependencyEdge
	latest   map[string]store.LatestRunInfo
	hasRun   map[string]bool
	consumed map[string]map[int64]bool
	lanes    []store.LaneEntry
	regs     []string
}

// compile-time proof the fake satisfies the pipeline-show read seam.
var _ store.ShowReader = (*ShowFake)(nil)

// NewShow returns an empty in-memory pipeline-show source over a fresh run-record
// Fake.
func NewShow() *ShowFake {
	return &ShowFake{
		Fake:     New(),
		details:  map[string]store.PipelineDetail{},
		found:    map[string]bool{},
		grants:   map[string][]store.Grant{},
		latest:   map[string]store.LatestRunInfo{},
		hasRun:   map[string]bool{},
		consumed: map[string]map[int64]bool{},
		lanes:    nil,
		regs:     nil,
	}
}

// SetDetail records a pipeline's resolved declaration detail (marking it
// registered) and returns the fake so calls chain.
func (f *ShowFake) SetDetail(name string, d store.PipelineDetail) *ShowFake {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.details[name] = d
	f.found[name] = true
	return f
}

// AddGrant records one field-level grant for a role and returns the fake so calls
// chain.
func (f *ShowFake) AddGrant(pgRole string, g store.Grant) *ShowFake {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.grants[pgRole] = append(f.grants[pgRole], g)
	return f
}

// AddEdge records one depends_on edge (from = dependent) and returns the fake so
// calls chain.
func (f *ShowFake) AddEdge(from, to string) *ShowFake {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.edges = append(f.edges, store.DependencyEdge{From: from, To: to})
	return f
}

// SetLatestRun records a pipeline's most recent run (the single run the gate reads
// for an upstream) and returns the fake so calls chain.
func (f *ShowFake) SetLatestRun(pipeline string, info store.LatestRunInfo) *ShowFake {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.latest[pipeline] = info
	f.hasRun[pipeline] = true
	return f
}

// SetConsumed marks that dependent has consumed upstreamRunID (a run_inputs row)
// and returns the fake so calls chain.
func (f *ShowFake) SetConsumed(dependent string, upstreamRunID int64) *ShowFake {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.consumed[dependent] == nil {
		f.consumed[dependent] = map[int64]bool{}
	}
	f.consumed[dependent][upstreamRunID] = true
	return f
}

// SeedLaneRows seeds the lanes rows for BuildWalk (returns self for chaining).
func (f *ShowFake) SeedLaneRows(rows ...store.LaneEntry) *ShowFake {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.lanes = append([]store.LaneEntry(nil), rows...)
	return f
}

// SeedRegistered seeds the list of registered pipelines (returns self).
func (f *ShowFake) SeedRegistered(names ...string) *ShowFake {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.regs = append([]string(nil), names...)
	return f
}

// PipelineDetail returns a copy of the pipeline's declaration detail, and whether
// it is registered.
func (f *ShowFake) PipelineDetail(_ context.Context, name string) (store.PipelineDetail, bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	d, ok := f.details[name]
	if !ok {
		return store.PipelineDetail{}, false, nil
	}
	d.Run = append([]string(nil), d.Run...)
	return d, true, nil
}

// GrantsForRole returns a copy of the role's grants.
func (f *ShowFake) GrantsForRole(_ context.Context, pgRole string) ([]store.Grant, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]store.Grant(nil), f.grants[pgRole]...), nil
}

// DependencyEdges returns a copy of the seeded depends_on edges.
func (f *ShowFake) DependencyEdges(_ context.Context) ([]store.DependencyEdge, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]store.DependencyEdge(nil), f.edges...), nil
}

// LatestRun returns a pipeline's most recent run, and whether it has any run.
func (f *ShowFake) LatestRun(_ context.Context, pipeline string) (store.LatestRunInfo, bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.latest[pipeline], f.hasRun[pipeline], nil
}

// Consumed reports whether dependent has consumed upstreamRunID.
func (f *ShowFake) Consumed(_ context.Context, dependent string, upstreamRunID int64) (bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.consumed[dependent][upstreamRunID], nil
}

// LaneRows returns seeded lane rows.
func (f *ShowFake) LaneRows(_ context.Context) ([]store.LaneEntry, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]store.LaneEntry(nil), f.lanes...), nil
}

// RegisteredPipelines returns seeded registered names.
func (f *ShowFake) RegisteredPipelines(_ context.Context) ([]string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.regs...), nil
}
