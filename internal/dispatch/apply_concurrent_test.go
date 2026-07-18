package dispatch_test

import (
	"context"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/MateusAMP2119/iris-lakehouse/internal/declare"
	"github.com/MateusAMP2119/iris-lakehouse/internal/dispatch"
	"github.com/MateusAMP2119/iris-lakehouse/internal/store"
)

// This file proves the apply op validates the dependency graph on the serialized
// single-writer path, closing the read-validate-write race two concurrent applies
// could otherwise exploit to commit a dependency cycle. Both applies run through one
// Dispatcher over a fake that couples the read and write seams -- a read observes
// prior committed writes -- so if validation ran on a pre-write snapshot outside the
// writer, both A->B and B->A would pass acyclicity and both would commit, leaving a
// cycle.

// rendezvous forces two callers to meet before either proceeds, with a timeout
// fallback so a lone caller (the serialized path, where the two reads can never be
// in flight at once) is never deadlocked. It fires exactly once: the broken code
// reads before it writes, so both applies rendezvous on the pre-write snapshot; the
// fixed code reads inside the writer closure, so each read runs alone, times out,
// and serializes behind the other's committed write.
type rendezvous struct {
	mu       sync.Mutex
	first    chan struct{}
	released bool
	timeout  time.Duration
}

func newRendezvous(timeout time.Duration) *rendezvous {
	return &rendezvous{timeout: timeout}
}

func (r *rendezvous) meet() {
	r.mu.Lock()
	if r.released {
		r.mu.Unlock()
		return
	}
	if r.first == nil {
		ch := make(chan struct{})
		r.first = ch
		r.mu.Unlock()
		select {
		case <-ch: // partner arrived and released us.
		case <-time.After(r.timeout): // no partner: the serialized single-writer path.
			r.mu.Lock()
			r.released = true
			r.mu.Unlock()
		}
		return
	}
	close(r.first) // second arrival: release the first.
	r.released = true
	r.mu.Unlock()
}

// coupledRegistry is both the meta write connection the Dispatcher's single Writer
// commits through (store.MetaTxConn) and the registry reader the apply op rebuilds
// its dependency graph from (store.RegistryReader). Unlike the record-only
// WriteRecorder it applies the registry statements it receives, so a read observes
// prior committed writes -- the coupling a concurrent-apply cycle test needs. It
// interprets the three registry statements an apply issues by their bound args.
type coupledRegistry struct {
	mu    sync.Mutex
	names map[string]bool
	edges map[string]map[string]bool // from -> set(to)
	gate  *rendezvous
}

func newCoupledRegistry(gate *rendezvous) *coupledRegistry {
	return &coupledRegistry{names: map[string]bool{}, edges: map[string]map[string]bool{}, gate: gate}
}

var (
	_ store.MetaTxConn     = (*coupledRegistry)(nil)
	_ store.RegistryReader = (*coupledRegistry)(nil)
)

// seed registers a pipeline with no edges, the pre-existing registry state a
// concurrent pair of applies validates against.
func (c *coupledRegistry) seed(names ...string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	for _, n := range names {
		c.names[n] = true
	}
}

// Exec applies one registry statement (the apply op uses ExecTx, but the write seam
// requires Exec too).
func (c *coupledRegistry) Exec(_ context.Context, sql string, args ...any) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.applyLocked(store.Statement{SQL: sql, Args: args})
	return nil
}

// ExecTx applies the statements of an apply's atomic registry transaction.
func (c *coupledRegistry) ExecTx(_ context.Context, stmts []store.Statement) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	for _, s := range stmts {
		c.applyLocked(s)
	}
	return nil
}

// applyLocked interprets one registry statement by its bound args. c.mu is held.
func (c *coupledRegistry) applyLocked(s store.Statement) {
	switch {
	case strings.Contains(s.SQL, "INSERT INTO pipelines"):
		c.names[s.Args[0].(string)] = true
	case strings.Contains(s.SQL, "DELETE FROM dependencies"):
		delete(c.edges, s.Args[0].(string))
	case strings.Contains(s.SQL, "INSERT INTO dependencies"):
		from, to := s.Args[0].(string), s.Args[1].(string)
		if c.edges[from] == nil {
			c.edges[from] = map[string]bool{}
		}
		c.edges[from][to] = true
	}
}

// RegisteredPipelines rendezvouses (the race window) then returns the registered
// names. Gating only this first read of buildGraph makes each apply arrive once.
func (c *coupledRegistry) RegisteredPipelines(_ context.Context) ([]string, error) {
	c.gate.meet()
	c.mu.Lock()
	defer c.mu.Unlock()
	out := make([]string, 0, len(c.names))
	for n := range c.names {
		out = append(out, n)
	}
	return out, nil
}

// LaneMembers is unused by the apply op (satisfies the read seam only).
func (c *coupledRegistry) LaneMembers(_ context.Context, _ string) ([]string, error) {
	return nil, nil
}

// DependencyEdges returns the committed depends_on edges.
func (c *coupledRegistry) DependencyEdges(_ context.Context) ([]store.DependencyEdge, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	var out []store.DependencyEdge
	for from, tos := range c.edges {
		for to := range tos {
			out = append(out, store.DependencyEdge{From: from, To: to})
		}
	}
	return out, nil
}

func (c *coupledRegistry) edgeCount() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	n := 0
	for _, tos := range c.edges {
		n += len(tos)
	}
	return n
}

// TestApplyConcurrentCycleRejected proves two concurrent applies that each close
// half of a cycle (A depends_on B, B depends_on A) can never both commit: validation
// runs on the serialized single-writer path, so whichever apply commits first is
// observed by the other's acyclicity check, which then rejects. Exactly one apply
// succeeds; the other fails with a cycle rejection, and the registry keeps a single
// edge. Run repeatedly to exercise the interleaving.
func TestApplyConcurrentCycleRejected(t *testing.T) {
	t.Run("depends-on-cycle-rejected", func(t *testing.T) {
		for i := 0; i < 5; i++ {
			reg := newCoupledRegistry(newRendezvous(300 * time.Millisecond))
			reg.seed("A", "B") // both upstreams registered; the only failure is the cycle.

			d := dispatch.New(reg)
			d.Start(context.Background())
			applier := dispatch.NewApplier(reg, d)

			declA := &declare.Pipeline{Name: "A", Run: []string{"python", "a.py"}, DependsOn: []string{"B"}}
			declB := &declare.Pipeline{Name: "B", Run: []string{"python", "b.py"}, DependsOn: []string{"A"}}

			var wg sync.WaitGroup
			wg.Add(2)
			errs := make([]error, 2)
			go func() { defer wg.Done(); errs[0] = applier.ApplyPipeline(context.Background(), "p/A", declA) }()
			go func() { defer wg.Done(); errs[1] = applier.ApplyPipeline(context.Background(), "p/B", declB) }()
			wg.Wait()
			d.Stop()

			failures := 0
			var failErr error
			for _, e := range errs {
				if e != nil {
					failures++
					failErr = e
				}
			}
			if failures != 1 {
				t.Fatalf("iter %d: concurrent A<->B applies got %d failures, want exactly 1 (one commits, the other is a cycle rejection): errA=%v errB=%v", i, failures, errs[0], errs[1])
			}
			if !strings.Contains(failErr.Error(), "cycle") {
				t.Errorf("iter %d: the rejected apply error = %v, want a cycle rejection", i, failErr)
			}
			if n := reg.edgeCount(); n != 1 {
				t.Errorf("iter %d: registry holds %d depends_on edges after the race, want 1 (no cycle committed)", i, n)
			}
		}
	})
}
