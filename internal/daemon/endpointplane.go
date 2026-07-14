package daemon

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"path/filepath"
	"sync"

	"github.com/MateusAMP2119/iris-engine-cli/internal/api"
	"github.com/MateusAMP2119/iris-engine-cli/internal/declare"
	"github.com/MateusAMP2119/iris-engine-cli/internal/dispatch"
)

// This file is the daemon's leader-side endpoint-apply plane: the composition root
// that turns POST /endpoint/apply into workspace endpoint discovery, compilation
// against the schemas/ set, prepare-verification against the data database, atomic
// meta persistence, and the live serving-registry swap. Like the control plane it
// is leader-only: the api mux gates the mutation to the leader, and the single meta
// writer (the dispatcher) only exists once a candidate wins the lock, so the live
// orchestrator is installed on winning leadership (before the leader role is
// reported) and cleared on demotion; a swappable endpointPlane holds it and
// satisfies api.EndpointControlHandler for the daemon's whole life.
//
// The live EndpointRegistry, by contrast, is process-long: it is built once at
// startup and shared with the serving mux, so an apply that commits makes the
// endpoint serve the very next /q request with no restart. The applier that swaps it
// is rebuilt each leadership term over that term's dispatcher.

// endpointPlane is the daemon's api.EndpointControlHandler: a stable handle the mux
// binds to for the daemon's whole life, delegating to the live orchestrator when the
// daemon leads and faulting internally otherwise.
type endpointPlane struct {
	mu   sync.RWMutex
	live *endpointOrchestrator
}

// compile-time proof the plane is the mux's endpoint control handler.
var _ api.EndpointControlHandler = (*endpointPlane)(nil)

// newEndpointPlane returns an unwired endpoint plane: applies fault until a leader
// installs an orchestrator.
func newEndpointPlane() *endpointPlane { return &endpointPlane{} }

// install wires the live orchestrator (on winning leadership).
func (p *endpointPlane) install(o *endpointOrchestrator) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.live = o
}

// clear removes the orchestrator (on demotion), so a request racing a lost lock
// faults rather than publishing off the single-writer path.
func (p *endpointPlane) clear() {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.live = nil
}

func (p *endpointPlane) orchestrator() *endpointOrchestrator {
	p.mu.RLock()
	defer p.mu.RUnlock()
	return p.live
}

// ApplyEndpoints routes to the live orchestrator, or faults when none is installed.
func (p *endpointPlane) ApplyEndpoints(ctx context.Context, req api.EndpointApplyRequest) (api.EndpointApplyResult, error) {
	o := p.orchestrator()
	if o == nil {
		return api.EndpointApplyResult{}, api.ErrEndpointControlUnavailable
	}
	return o.apply(ctx, req)
}

// endpointOrchestrator runs the leader-side endpoint apply against the workspace and
// the databases. It discovers and compiles the declared endpoints from the leader's
// workspace, then hands them to dispatch's applier (verify, persist, publish).
type endpointOrchestrator struct {
	workspace string
	applier   *dispatch.EndpointApplier
	logger    *slog.Logger
}

// newEndpointOrchestrator builds the leader's endpoint orchestrator over its
// workspace root and the endpoint applier (built over this term's dispatcher, the
// shared serving registry, and the data-database prepare-verifier). A nil logger
// discards output.
func newEndpointOrchestrator(workspace string, applier *dispatch.EndpointApplier, logger *slog.Logger) *endpointOrchestrator {
	if logger == nil {
		logger = slog.New(slog.NewTextHandler(io.Discard, nil))
	}
	return &endpointOrchestrator{workspace: workspace, applier: applier, logger: logger}
}

// apply publishes the workspace's declared endpoints (all, or the one named by req):
// it discovers the endpoints/ files and the schemas/ set from the leader's own
// workspace, compiles each endpoint against its source table, and runs the applier
// (prepare-verify against the data database, atomic meta persistence, live-registry
// swap on commit). An unknown endpoint name, a bad endpoint file, a compile error,
// or a prepare-verify refusal fails the whole apply, changing neither meta nor the
// serving surface.
func (o *endpointOrchestrator) apply(ctx context.Context, req api.EndpointApplyRequest) (api.EndpointApplyResult, error) {
	discovered, err := declare.DiscoverEndpoints(o.workspace)
	if err != nil {
		return api.EndpointApplyResult{}, fmt.Errorf("endpoint apply: discover endpoints: %w", err)
	}
	if len(discovered) == 0 {
		return api.EndpointApplyResult{}, fmt.Errorf("endpoint apply: no endpoints declared under %s", filepath.Join(o.workspace, "endpoints"))
	}

	tables, err := declare.ValidateSchemaTree(filepath.Join(o.workspace, "schemas"))
	if err != nil {
		return api.EndpointApplyResult{}, fmt.Errorf("endpoint apply: read schemas tree: %w", err)
	}
	index := declare.TableIndex(tables)

	selected := discovered
	if req.Name != "" {
		selected = nil
		for _, de := range discovered {
			if de.Name == req.Name {
				selected = append(selected, de)
				break
			}
		}
		if len(selected) == 0 {
			return api.EndpointApplyResult{}, fmt.Errorf("endpoint apply: no such endpoint %q under %s", req.Name, filepath.Join(o.workspace, "endpoints"))
		}
	}

	compiled := make([]*declare.CompiledEndpoint, 0, len(selected))
	applied := make([]string, 0, len(selected))
	for _, de := range selected {
		ce, err := declare.CompileEndpoint(de.Spec, index)
		if err != nil {
			return api.EndpointApplyResult{}, fmt.Errorf("endpoint apply: compile endpoint %q: %w", de.Name, err)
		}
		compiled = append(compiled, ce)
		applied = append(applied, de.Name)
	}

	if err := o.applier.Apply(ctx, compiled); err != nil {
		return api.EndpointApplyResult{}, fmt.Errorf("endpoint apply: %w", err)
	}
	return api.EndpointApplyResult{Applied: applied}, nil
}
