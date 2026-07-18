package dispatch

import "sync"

// This file is the per-lane loop pass counter: "loop passes completed since daemon
// start (a leader-held runtime counter, reset on restart and leader change)". It is
// deliberately process memory, never a meta row -- the count is a property of the
// current leader's runtime, so a daemon restart resets it by construction (a fresh
// process constructs a fresh counter), and the daemon resets it explicitly when it
// wins a leadership term (internal/daemon), so a re-elected leader never resumes a
// previous term's counts. Clock-free: a count of completed passes, never a duration
// or a rate.

// PassCounter counts completed lane passes per lane. It is safe for concurrent
// use: the lane loop's per-lane goroutines increment it through Hook while the
// stats read path snapshots it through Counts. The zero value is not usable;
// construct one with NewPassCounter.
type PassCounter struct {
	mu     sync.Mutex
	epoch  uint64 // term boundary: Reset bumps it; hooks minted before no-op (#173)
	counts map[string]int64
}

// NewPassCounter returns an empty pass counter: a freshly started daemon has
// completed no passes.
func NewPassCounter() *PassCounter {
	return &PassCounter{counts: make(map[string]int64)}
}

// Hook returns the per-pass observability hook the lane loop invokes after each
// completed lane pass (WithOnPass): it increments the pass's lane by one. It
// never influences dispatch. Epoch-bound at mint: after a Reset it no-ops, so a
// deposed term's straggler pass never counts into the new term (#173).
func (c *PassCounter) Hook() func(PassReport) {
	c.mu.Lock()
	epoch := c.epoch
	c.mu.Unlock()
	return func(report PassReport) {
		c.mu.Lock()
		defer c.mu.Unlock()
		if c.epoch != epoch {
			return // stale term's hook: discard
		}
		c.counts[report.Lane]++
	}
}

// Counts returns a snapshot copy of the per-lane pass counts; mutating the
// returned map never reaches the counter.
func (c *PassCounter) Counts() map[string]int64 {
	c.mu.Lock()
	defer c.mu.Unlock()
	out := make(map[string]int64, len(c.counts))
	for lane, n := range c.counts {
		out[lane] = n
	}
	return out
}

// Reset zeroes every lane's count and bumps the epoch (the leader-change
// semantics): counts never carry across terms, and prior hooks go stale with
// them. A daemon restart needs no call (a new process constructs fresh).
func (c *PassCounter) Reset() {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.epoch++
	c.counts = make(map[string]int64)
}
