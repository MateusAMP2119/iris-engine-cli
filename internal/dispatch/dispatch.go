// Package dispatch owns the leader's single-writer meta path: one goroutine, the sole
// meta writer. Every meta write is submitted to the one Dispatcher and executed
// serially by its one goroutine, so meta has exactly one writer and writes never
// overlap. The dispatcher is a leader-owned component: only the elected leader starts
// one (the daemon does so on winning the leader lock), and it holds the one
// store.Writer -- which it alone constructs, so no other package can open a second
// write path to meta.
//
// The dispatcher carries the write-serialization mechanism itself plus the two
// leader-only writes below (the meta schema re-check and the leader-address
// advertisement); the run-record, dead-letter, replay, and journal-lifecycle writes
// it owns all ride the same Submit path, so the single-writer invariant holds for
// every meta mutation.
package dispatch

import (
	"context"
	"errors"
	"sync"

	"github.com/MateusAMP2119/iris-engine-cli/internal/store"
)

// ErrDispatcherStopped is returned by Submit when the dispatcher has stopped: a
// write is never silently dropped, so a caller learns its meta mutation did not run
// (a deposed leader, for instance, whose dispatcher shut down).
var ErrDispatcherStopped = errors.New("dispatch: dispatcher stopped")

// request is one unit of work handed to the dispatcher goroutine: the write closure
// and the channel its result returns on.
type request struct {
	fn   func(*store.Writer) error
	resp chan error
}

// Dispatcher serializes every meta write onto one goroutine that owns the single
// store.Writer. Build it with New over the leader's meta write connection, bring it
// up with Start, submit writes with Submit, and shut it down with Stop.
type Dispatcher struct {
	writer *store.Writer
	reqs   chan request
	done   chan struct{}

	startOnce sync.Once
	stopOnce  sync.Once
	wg        sync.WaitGroup
}

// New builds a dispatcher over the leader's meta write connection. It is the one
// place store.NewWriter is called: the dispatcher owns the sole meta writer, so no
// other component can construct one and open a second write path (a static
// architecture check enforces this).
func New(conn store.MetaWriteConn) *Dispatcher {
	return &Dispatcher{
		writer: store.NewWriter(conn),
		reqs:   make(chan request),
		done:   make(chan struct{}),
	}
}

// Start launches the single dispatcher goroutine. It is idempotent; the goroutine
// runs until Stop is called or ctx is cancelled.
func (d *Dispatcher) Start(ctx context.Context) {
	d.startOnce.Do(func() {
		d.wg.Add(1)
		go d.loop(ctx)
	})
}

// loop is the sole meta-writer goroutine: it runs each submitted write closure
// against the one Writer, one at a time, in submission order, until stopped.
func (d *Dispatcher) loop(ctx context.Context) {
	defer d.wg.Done()
	for {
		select {
		case <-ctx.Done():
			return
		case <-d.done:
			return
		case req := <-d.reqs:
			req.resp <- req.fn(d.writer)
		}
	}
}

// Submit runs fn on the dispatcher goroutine against the single Writer and returns
// its result, blocking until the write completes (or ctx is cancelled, or the
// dispatcher stops). Because every meta write goes through here, they are
// serialized onto the one goroutine: there is no second path to the Writer.
func (d *Dispatcher) Submit(ctx context.Context, fn func(*store.Writer) error) error {
	resp := make(chan error, 1)
	select {
	case d.reqs <- request{fn: fn, resp: resp}:
	case <-d.done:
		return ErrDispatcherStopped
	case <-ctx.Done():
		return ctx.Err()
	}
	select {
	case err := <-resp:
		return err
	case <-ctx.Done():
		return ctx.Err()
	}
}

// EnsureSchema submits the leader's meta schema re-check through the single-writer
// path: the create-if-missing control-table DDL the leader issues at election. It is
// a leader-only meta write, so it runs on the dispatcher goroutine like every other.
func (d *Dispatcher) EnsureSchema(ctx context.Context) error {
	return d.Submit(ctx, func(w *store.Writer) error { return w.EnsureSchema(ctx) })
}

// AdvertiseLeader submits the leader-address advertisement through the single-writer
// path: the upsert of this leader's advertised address into the single-row leadership
// table the leader issues on winning the advisory lock. It is a leader-only meta
// write, so it runs on the dispatcher goroutine like every other.
func (d *Dispatcher) AdvertiseLeader(ctx context.Context, addr string) error {
	return d.Submit(ctx, func(w *store.Writer) error { return w.AdvertiseLeader(ctx, addr) })
}

// Stop shuts the dispatcher goroutine down and waits for it to exit. It is
// idempotent and safe to call from a defer.
func (d *Dispatcher) Stop() {
	d.stopOnce.Do(func() { close(d.done) })
	d.wg.Wait()
}
