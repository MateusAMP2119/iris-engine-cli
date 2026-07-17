package daemon

import (
	"sort"
	"sync"

	"github.com/MateusAMP2119/iris-lakehouse/internal/api"
)

// This file is the resident turn-counter registry (#206): the in-memory,
// no-rows account of the turns each pipeline's worker has been given. Quiet
// turns deliberately write nothing, so these counters are the operator's only
// visibility into a quiet loop -- `iris ps` renders them as "turns since last
// recorded run". The registry is daemon-scoped like the in-flight registry; the
// lane loop bumps it and a new leadership term resets it with the loop's own
// state.

// turnCounters counts driven turns per pipeline: the total and the tally since
// the last turn that recorded a run row.
type turnCounters struct {
	mu sync.Mutex
	m  map[string]*turnCount
}

// turnCount is one pipeline's tally.
type turnCount struct {
	turns     uint64
	sinceRun  uint64
}

// newTurnCounters builds an empty registry.
func newTurnCounters() *turnCounters {
	return &turnCounters{m: map[string]*turnCount{}}
}

// bump records one driven turn; recorded reports whether the turn minted (or
// completed) a run row, resetting the since-run tally.
func (c *turnCounters) bump(pipeline string, recorded bool) {
	if c == nil {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	t := c.m[pipeline]
	if t == nil {
		t = &turnCount{}
		c.m[pipeline] = t
	}
	t.turns++
	if recorded {
		t.sinceRun = 0
	} else {
		t.sinceRun++
	}
}

// reset clears every tally (a new leadership term starts a fresh account).
func (c *turnCounters) reset() {
	if c == nil {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	c.m = map[string]*turnCount{}
}

// snapshot renders the tallies as the ps payload's resident rows, sorted by
// pipeline for a stable readout. Empty when no turns have been driven.
func (c *turnCounters) snapshot() []api.PsResident {
	if c == nil {
		return nil
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	out := make([]api.PsResident, 0, len(c.m))
	for pipeline, t := range c.m {
		out = append(out, api.PsResident{Pipeline: pipeline, Turns: t.turns, TurnsSinceRun: t.sinceRun})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Pipeline < out[j].Pipeline })
	return out
}
