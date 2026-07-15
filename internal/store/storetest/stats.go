package storetest

import (
	"context"
	"sync"

	"github.com/MateusAMP2119/iris-lakehouse/internal/store"
)

// This file extends the meta-store fake with the stats read seam
// (store.StatsSource): the dead-letter worklist, the persisted composer, the
// registry roster, the journal counters, and the checkpoint chain, beside the
// run records the embedded Fake already serves. It keeps the fake's value
// semantics -- every read returns copies -- so the stats rollup is proven with
// no live Postgres.

// StatsFake is an in-memory store.StatsSource: the run-record Fake plus the
// stats rollup's other sources. The zero value is not usable; construct one
// with NewStats.
type StatsFake struct {
	*Fake

	mu          sync.Mutex
	deadLetters []store.DeadLetterEntry
	members     []store.LaneMember
	pipelines   []string
	journal     store.JournalStats
	checkpoints []store.Checkpoint
}

// compile-time proof the fake satisfies the stats read seam.
var _ store.StatsSource = (*StatsFake)(nil)

// NewStats returns an empty in-memory stats source over a fresh run-record Fake.
func NewStats() *StatsFake {
	return &StatsFake{Fake: New()}
}

// AddDeadLetter parks one outstanding dead-letter worklist entry.
func (f *StatsFake) AddDeadLetter(e store.DeadLetterEntry) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.deadLetters = append(f.deadLetters, e)
}

// AddLaneMember records one persisted composer row (pipeline lane membership).
func (f *StatsFake) AddLaneMember(lane, pipeline string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.members = append(f.members, store.LaneMember{Lane: lane, Pipeline: pipeline})
}

// RegisterPipeline records one registered pipeline name.
func (f *StatsFake) RegisterPipeline(name string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.pipelines = append(f.pipelines, name)
}

// SetJournal sets the journal counters the Journal read returns.
func (f *StatsFake) SetJournal(j store.JournalStats) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.journal = j
}

// AddCheckpoint appends one checkpoint chain row.
func (f *StatsFake) AddCheckpoint(cp store.Checkpoint) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.checkpoints = append(f.checkpoints, cp)
}

// DeadLetters returns a copy of the outstanding dead-letter worklist.
func (f *StatsFake) DeadLetters(context.Context) ([]store.DeadLetterEntry, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]store.DeadLetterEntry(nil), f.deadLetters...), nil
}

// LaneMembers returns a copy of the persisted composer rows.
func (f *StatsFake) LaneMembers(context.Context) ([]store.LaneMember, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]store.LaneMember(nil), f.members...), nil
}

// PipelineNames returns a copy of the registered pipeline names.
func (f *StatsFake) PipelineNames(context.Context) ([]string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.pipelines...), nil
}

// Journal returns the journal counters.
func (f *StatsFake) Journal(context.Context) (store.JournalStats, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.journal, nil
}

// Checkpoints returns a copy of the checkpoint chain rows.
func (f *StatsFake) Checkpoints(context.Context) ([]store.Checkpoint, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]store.Checkpoint(nil), f.checkpoints...), nil
}
