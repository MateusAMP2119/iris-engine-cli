package dispatch

// This file is the endpoint apply lifecycle op: the leader-side path that takes
// declare's compiled endpoint shapes (declare.CompileEndpoint) live. An apply
// prepare-verifies each endpoint's one derived SQL statement against the DATA
// database (a shape whose statement Postgres refuses never publishes), then
// persists every shape to endpoints + endpoint_filters as one atomic meta
// transaction through the single writer, and only on commit swaps the shapes into
// the live serving registry -- so an applied endpoint takes effect immediately, with
// no daemon restart, and a failed apply changes nothing, neither meta nor the
// serving surface. Remove retires the shape the same way: meta rows first, live
// registry on commit.
//
// The lifecycle is deliberately independent of the workload graph: apply reads
// no registry and writes no workload table, remove retires shape only, and
// declare destroy (destroy.go) never touches an endpoint row -- contracts
// publish and retire independently of declare apply, and declare destroy
// leaves tables and endpoints standing.
//
// PrepareVerifier is the data-database PREPARE, kept as a seam so this op stays
// free of a database client. The pgx-backed implementation is
// pg.Client.PrepareVerify, which rides the shared data pool and is what the daemon
// wires in at leader election; a fake drives it in this package's tests.

import (
	"context"
	"errors"
	"fmt"
	"sync"

	"github.com/MateusAMP2119/iris-lakehouse/internal/declare"
	"github.com/MateusAMP2119/iris-lakehouse/internal/store"
)

// PrepareVerifier prepare-verifies one derived endpoint statement against the data
// database (apply prepare-verifies the derived SQL). The production implementation
// issues a PREPARE on a data-database connection so Postgres itself vets the
// statement -- source present, columns real, types castable -- before anything
// persists; a recording fake stands in for integration tests.
type PrepareVerifier interface {
	// PrepareVerify vets the derived SQL of the named endpoint against the data
	// database, returning Postgres's refusal verbatim on failure.
	PrepareVerify(ctx context.Context, name, sql string) error
}

// EndpointRegistry is the daemon's live endpoint shapes: the one in-memory
// name-to-compiled-shape map the /q serving path checks a request's shape out
// of. Shapes go live only when their apply commits and are treated as
// immutable thereafter; a re-apply swaps the map entry, never the shape a
// prior checkout returned, so an in-flight request finishes with the shape it
// started with while the very next request sees the new one (the
// request-boundary swap). It satisfies the api layer's EndpointSource seam.
type EndpointRegistry struct {
	mu     sync.RWMutex
	shapes map[string]*declare.CompiledEndpoint
}

// NewEndpointRegistry returns an empty live endpoint registry. The daemon
// builds exactly one at startup and hands it to both the serving mux and the
// endpoint applier; it is never rebuilt, which is what makes an apply
// effective without a restart.
func NewEndpointRegistry() *EndpointRegistry {
	return &EndpointRegistry{shapes: map[string]*declare.CompiledEndpoint{}}
}

// Endpoint returns the live compiled shape for name. This is the
// request-boundary checkout: a request resolves its shape exactly once, here,
// and holds the returned (immutable) shape for its whole execution, so a
// concurrent re-apply swaps the registry without disturbing it.
func (r *EndpointRegistry) Endpoint(name string) (*declare.CompiledEndpoint, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	ce, ok := r.shapes[name]
	return ce, ok
}

// publish swaps eps live in one lock hold, so a multi-endpoint apply lands as
// one visible change. Called only after the persisting transaction commits.
func (r *EndpointRegistry) publish(eps []*declare.CompiledEndpoint) {
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, ep := range eps {
		r.shapes[ep.Name] = ep
	}
}

// Reload replaces the whole live shape set with a reloaded set at daemon startup, so
// a restart or failover serves every persisted endpoint without a re-apply. It swaps
// the map wholesale in one lock hold: the registry starts empty each process, so the
// reloaded set IS the truth. Callers pass the endpoints recompiled from the persisted
// meta rows; a nil or empty set leaves an empty registry (no persisted endpoints to
// serve).
func (r *EndpointRegistry) Reload(eps []*declare.CompiledEndpoint) {
	shapes := make(map[string]*declare.CompiledEndpoint, len(eps))
	for _, ep := range eps {
		if ep != nil {
			shapes[ep.Name] = ep
		}
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	r.shapes = shapes
}

// retire removes name from the live shapes. Called only after the removing
// transaction commits; an in-flight request that already checked its shape out
// finishes normally.
func (r *EndpointRegistry) retire(name string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	delete(r.shapes, name)
}

// EndpointApplier is the endpoint lifecycle op: verify, persist, publish.
// Build it with NewEndpointApplier over the data-database verifier seam, the
// single-writer submission seam (the Dispatcher), and the daemon's one live
// registry.
type EndpointApplier struct {
	verify PrepareVerifier
	submit Submitter
	live   *EndpointRegistry
}

// NewEndpointApplier builds the endpoint lifecycle op over the prepare-verify
// seam, the single-writer submitter, and the live serving registry.
func NewEndpointApplier(verify PrepareVerifier, submit Submitter, live *EndpointRegistry) *EndpointApplier {
	return &EndpointApplier{verify: verify, submit: submit, live: live}
}

// Apply publishes compiled endpoints, in three strictly ordered steps: (1)
// prepare-verify every endpoint's derived SQL against the data database -- any
// refusal returns before any write, so a multi-endpoint apply is all-or-nothing from
// the first step; (2) persist every shape to endpoints + endpoint_filters as one
// atomic meta transaction through the single writer; (3) only on commit, swap the
// shapes into the live registry, making the apply effective immediately -- requests
// serve the new shapes with no daemon restart, and a failed apply leaves both meta
// and the serving surface exactly as they were. It reads no workload registry and
// writes no workload table: the endpoint lifecycle is independent of declare apply.
func (a *EndpointApplier) Apply(ctx context.Context, eps []*declare.CompiledEndpoint) error {
	if len(eps) == 0 {
		return errors.New("dispatch: apply endpoints: nothing to apply")
	}
	for _, ep := range eps {
		if ep == nil {
			return errors.New("dispatch: apply endpoints: nil compiled endpoint")
		}
		if err := a.verify.PrepareVerify(ctx, ep.Name, ep.SQL); err != nil {
			return fmt.Errorf("dispatch: apply endpoint %q: prepare-verify derived SQL against the data database: %w", ep.Name, err)
		}
	}

	rows := make([]store.EndpointRow, 0, len(eps))
	for _, ep := range eps {
		rows = append(rows, endpointRow(ep))
	}
	if err := a.submit.Submit(ctx, func(w *store.Writer) error {
		return w.ApplyEndpoints(ctx, rows)
	}); err != nil {
		return fmt.Errorf("dispatch: apply %d endpoint(s): %w", len(eps), err)
	}

	// Effective on commit: the swap happens only after the transaction above
	// succeeded, so the serving surface never runs ahead of meta.
	a.live.publish(eps)
	return nil
}

// Remove retires one endpoint: it deletes the persisted shape (filter rows, then the
// endpoints row) as one atomic meta transaction through the single writer, and only
// on commit retires the shape from the live registry -- the next request sees 404, an
// in-flight one finishes its checked-out shape. It retires shape only: no declared
// table, no data, no workload row -- remove is independent of declare destroy. An
// unknown endpoint is refused by name before any write.
func (a *EndpointApplier) Remove(ctx context.Context, name string) error {
	if _, ok := a.live.Endpoint(name); !ok {
		return fmt.Errorf("dispatch: remove endpoint %q: no such endpoint", name)
	}
	if err := a.submit.Submit(ctx, func(w *store.Writer) error {
		return w.RemoveEndpoint(ctx, name)
	}); err != nil {
		return fmt.Errorf("dispatch: remove endpoint %q: %w", name, err)
	}
	a.live.retire(name)
	return nil
}

// endpointRow flattens one compiled endpoint into its persisted store row: the
// dotted source, the projection, the sort, and the filter grammar in
// declaration order. The derived SQL is not persisted (it recompiles
// deterministically from these rows).
func endpointRow(ep *declare.CompiledEndpoint) store.EndpointRow {
	filters := make([]store.EndpointFilterRow, 0, len(ep.Filters))
	for _, f := range ep.Filters {
		filters = append(filters, store.EndpointFilterRow{Param: f.Param, Op: string(f.Op)})
	}
	return store.EndpointRow{
		Name:    ep.Name,
		Source:  ep.Schema + "." + ep.Table,
		Fields:  append([]string(nil), ep.Fields...),
		Sort:    ep.Sort,
		Filters: filters,
	}
}
