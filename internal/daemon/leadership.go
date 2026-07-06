package daemon

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"time"

	"github.com/MateusAMP2119/iris-engine-cli/internal/api"
	"github.com/MateusAMP2119/iris-engine-cli/internal/dispatch"
	"github.com/MateusAMP2119/iris-engine-cli/internal/store"
)

// lockReleaseGrace bounds the explicit advisory-lock release (pg_advisory_unlock +
// session close) run on a detached context during demotion, so the release can
// complete even though the daemon's own context is already cancelled. Closing the
// session releases the lock regardless, so this only bounds the courteous explicit
// unlock.
const lockReleaseGrace = 5 * time.Second

// This file is leader election: the step that turns a daemon candidate into the
// one leader, the sole dispatcher (specification sections 2 and 15). Leadership is
// a Postgres session advisory lock: a candidate blocks acquiring it (standby),
// and the acquire returns only when it wins (leader). On winning, the candidate
// starts the single dispatcher goroutine, re-checks the meta schema through it
// (the leader-only write at election, specification section 4), and reports the
// leader role so its listeners accept mutations. Standbys reject mutations and
// serve reads. Connection death releases the lock and promotes the next standby;
// E11 drives the failover consequences (self-demotion, dead-lettering) -- E02.6
// lands the election and the single-writer path it gates.

// Candidate is one daemon candidate for leadership. Serve blocks it as a standby
// until it acquires the leader lock, then runs it as the leader (sole dispatcher)
// until its context is cancelled or its lock session is lost.
type Candidate struct {
	lock      store.LeaderLock
	role      *api.RoleState
	writeConn store.MetaWriteConn
	logger    *slog.Logger
}

// NewCandidate builds a leadership candidate over the leader lock, the role state
// its listeners consult, and the leader's meta write connection (which the
// dispatcher wraps in the single Writer on winning). A nil logger discards output.
func NewCandidate(lock store.LeaderLock, role *api.RoleState, writeConn store.MetaWriteConn, logger *slog.Logger) *Candidate {
	if logger == nil {
		logger = slog.New(slog.NewTextHandler(io.Discard, nil))
	}
	return &Candidate{lock: lock, role: role, writeConn: writeConn, logger: logger}
}

// Serve runs the candidate: it reports the standby role, blocks acquiring the
// leader lock, and -- once it wins -- starts the single dispatcher, re-checks the
// meta schema through it, and reports the leader role. It then blocks until ctx is
// cancelled or the lock session is lost, at which point it stops dispatching and
// releases the lock so the next standby is promoted. A cancelled-before-acquire
// candidate returns ctx.Err() without ever leading.
func (c *Candidate) Serve(ctx context.Context) error {
	c.role.SetStandby("")
	c.logger.Info("iris daemon standby: contending for leadership")

	if err := c.lock.Acquire(ctx); err != nil {
		if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
			// Cancelled while still a standby (never won the lock): a clean shutdown,
			// not an error.
			return nil
		}
		return fmt.Errorf("daemon: acquire leader lock: %w", err)
	}
	// Won the lock: become the leader and the sole dispatcher.
	return c.lead(ctx)
}

// lead runs the leader loop: start the dispatcher, re-check the meta schema through
// it (a leader-only meta write), report the leader role, and block until ctx is
// cancelled or the lock session dies -- then tear down and release the lock.
func (c *Candidate) lead(ctx context.Context) error {
	d := dispatch.New(c.writeConn)
	d.Start(ctx)
	defer d.Stop()

	// The leader re-checks the meta schema at election (specification section 4),
	// through the single-writer dispatcher path: the first meta write only the
	// leader performs.
	if err := d.EnsureSchema(ctx); err != nil {
		// Failed to establish the schema: relinquish leadership so another candidate
		// can try, rather than lead with an unverified meta.
		c.role.SetStandby("")
		return errors.Join(err, c.release())
	}

	c.role.SetLeader()
	c.logger.Info("iris daemon leader: dispatching (sole meta writer)")

	select {
	case <-ctx.Done():
	case <-c.lock.SessionLost():
		// Connection death released the lock (specification section 15): stop leading.
		c.logger.Warn("iris daemon leader: lock session lost, demoting")
	}

	c.role.SetStandby("")
	// A clean shutdown (ctx cancelled) or a demotion (session lost) is not an error;
	// only a failed lock release is.
	return c.release()
}

// release relinquishes the leader lock on a detached, time-bounded context: the
// daemon's own context is cancelled at shutdown, but the explicit pg_advisory_unlock
// should still run (closing the session releases the lock regardless).
func (c *Candidate) release() error {
	ctx, cancel := context.WithTimeout(context.Background(), lockReleaseGrace)
	defer cancel()
	return c.lock.Release(ctx)
}
