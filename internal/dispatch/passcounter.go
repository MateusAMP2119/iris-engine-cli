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
	counts map[string]int64
}

// NewPassCounter returns an empty pass counter: a freshly started daemon has
// completed no passes.
func NewPassCounter() *PassCounter {
	return &PassCounter{counts: make(map[string]int64)}
}

// Hook returns the per-pass observability hook the lane loop invokes after each
// completed lane pass (WithOnPass): it increments the pass's lane by one. It
// never influences dispatch.
func (c *PassCounter) Hook() func(PassReport) {
	return func(report PassReport) {
		c.mu.Lock()
		defer c.mu.Unlock()
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

// Reset zeroes every lane's count: the leader-change semantics. The daemon
// invokes it when a candidate wins a leadership term, so counts never carry
// across terms; a daemon restart needs no call (a new process constructs a
// fresh counter).
func (c *PassCounter) Reset() {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.counts = make(map[string]int64)
}
