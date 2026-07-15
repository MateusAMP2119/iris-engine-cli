package dispatch

import "sync/atomic"

// This file is the leader's meta-change watermark: the in-process signal the lane
// loop parks on so an idle lane costs nothing between causes. Every meta mutation
// rides the single Dispatcher (dispatch.go), so one bump there covers every
// engine-visible cause -- apply, manual run, replay, drain, wipe, and the loop's
// own run transitions -- and the watermark needs no stored state, no polling
// query, and no clock. It is leader-term scoped exactly like the dispatcher that
// bumps it: a new term starts a fresh watermark, and a fresh leader's first pass
// runs unconditionally (no lane has passed yet), so nothing is missed across a
// failover.
//
// The watermark is deliberately coarse: any meta write advances it, and an
// advanced watermark only makes a lane ELIGIBLE for a pass -- the depends_on gate
// and the root cause gate still decide whether anything runs. Over-waking costs
// one cheap pass; the alternative (a per-lane change feed) would be a consumer
// cursor, which the engine deliberately holds nowhere.

// Events is a monotonic in-process change counter with a coalescing wake channel.
// Bump advances the sequence and wakes any parked waiter; Seq reads the current
// sequence; Wake exposes the wait channel. It is safe for concurrent use. The
// zero value is NOT ready; build it with NewEvents.
type Events struct {
	seq  atomic.Uint64
	wake chan struct{}
}

// NewEvents builds a ready watermark at sequence zero.
func NewEvents() *Events {
	return &Events{wake: make(chan struct{}, 1)}
}

// Bump advances the sequence and wakes a parked waiter. The wake send is
// non-blocking and coalescing: any number of bumps while no one waits leave one
// pending token, so a waiter never misses that something changed, only how many
// times -- the sequence carries that.
func (e *Events) Bump() {
	e.seq.Add(1)
	select {
	case e.wake <- struct{}{}:
	default:
	}
}

// Seq returns the current sequence. A lane whose last pass started at this
// sequence has seen every cause recorded so far.
func (e *Events) Seq() uint64 {
	return e.seq.Load()
}

// Wake returns the coalescing wake channel. A receive consumes the pending
// token; the caller re-checks Seq after waking rather than counting receives.
func (e *Events) Wake() <-chan struct{} {
	return e.wake
}
