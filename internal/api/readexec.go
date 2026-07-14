package api

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"

	"github.com/MateusAMP2119/iris-engine-cli/internal/declare"
	"github.com/MateusAMP2119/iris-engine-cli/internal/store"
)

// This file is the execution composition of the data surface: the seam the
// routes execute engine-built statements through, and the production /q reader
// over it. The ReadExecutor is the shared read pool's map-shaped surface
// (store.ReadPool satisfies it; api holds no database path of its own -- the
// daemon supplies the wired pool); a request's execution identity comes from
// its resolved Authority: always the calling PAT's engine-managed role, or the
// engine's own self read for ambient (socket) callers. Endpoints never enter
// the picture as an identity -- they own no roles and mint no credentials (pure
// shape) -- so the same endpoint read by two PATs executes under two different
// roles.

// ReadExecutor executes one engine-built read statement through the shared
// read pool on the data database and returns the served rows as column-keyed
// maps in served order. ExecuteRead runs SET ROLE <role> around the statement
// (the caller PAT's role); ExecuteReadSelf runs as the pool's own login role
// (the engine itself, ambient callers only). A read Postgres refused for a
// missing grant is reported wrapped in store.ErrReadForbidden.
type ReadExecutor interface {
	// ExecuteRead runs the fixed statement as role with args bound positionally.
	ExecuteRead(ctx context.Context, role, name, text string, args []any, columns []string) ([]map[string]any, error)
	// ExecuteReadSelf runs the fixed statement as the pool's own login role.
	ExecuteReadSelf(ctx context.Context, name, text string, args []any, columns []string) ([]map[string]any, error)
}

// Compile-time proof the shared read pool is the executor the daemon wires in.
var _ ReadExecutor = (*store.ReadPool)(nil)

// WithReadExecutor wires the read-statement executor the /data route serves
// through. A nil executor is ignored, keeping the unwired default (/data
// answers the internal-fault envelope).
func WithReadExecutor(ex ReadExecutor) MuxOption {
	return func(m *mux) {
		if ex != nil {
			m.readexec = ex
		}
	}
}

// executionRole resolves the Postgres identity a data-surface request executes
// as: the calling PAT's engine-managed data role, or the engine's own self read
// for ambient authority (self true). A non-ambient authority without a data
// role is an internal fault -- the scope check already admitted the request, so
// a missing role is broken wiring, never a reason to fall back to any other
// identity.
func executionRole(a Authority) (role string, self bool, err error) {
	if a.DataRole != "" {
		return a.DataRole, false, nil
	}
	if a.Ambient {
		return "", true, nil
	}
	return "", false, errors.New("request authority carries no data role to execute as")
}

// executeRead runs one engine-built statement under the request's resolved
// execution identity through the wired executor.
func executeRead(ctx context.Context, ex ReadExecutor, name, text string, args []any, columns []string) ([]map[string]any, error) {
	role, self, err := executionRole(AuthorityFrom(ctx))
	if err != nil {
		return nil, err
	}
	if self {
		return ex.ExecuteReadSelf(ctx, name, text, args, columns)
	}
	return ex.ExecuteRead(ctx, role, name, text, args, columns)
}

// PoolReader is the production EndpointReader (the /q executor): it binds the
// request's validated plan into the endpoint's compiled statement and executes
// it through the shared read pool as the calling PAT's role. It holds no
// identity of its own and consults none of the endpoint's -- there is none to
// consult.
type PoolReader struct {
	exec ReadExecutor
}

// NewPoolReader returns the production /q reader over the given executor
// (store.ReadPool in the daemon's wiring, a fake in tests).
func NewPoolReader(exec ReadExecutor) *PoolReader { return &PoolReader{exec: exec} }

// ReadEndpoint executes the compiled shape's statement under the request's
// plan: BindArgs turns the plan into the positional vector for the compiled
// binding plan, and the executor runs the fixed text as the caller's role.
func (p *PoolReader) ReadEndpoint(ctx context.Context, shape *declare.CompiledEndpoint, plan *QueryPlan) ([]map[string]any, error) {
	if p.exec == nil {
		return nil, errors.New("api: pool reader: no executor wired")
	}
	if shape == nil {
		return nil, errors.New("api: pool reader: nil compiled endpoint")
	}
	args, err := BindArgs(shape.Params, plan)
	if err != nil {
		return nil, fmt.Errorf("api: pool reader: endpoint %s: %w", shape.Name, err)
	}
	return executeRead(ctx, p.exec, qStatementName(shape), shape.SQL, args, shape.Fields)
}

// qStatementName derives the session-scoped prepared-statement name for a
// compiled endpoint: q_<endpoint>_<hash8(SQL)>. The content hash keys the name
// to the exact compiled text, so a re-apply that changes the shape prepares a
// fresh statement instead of colliding with the old text on a pooled session.
func qStatementName(shape *declare.CompiledEndpoint) string {
	sum := sha256.Sum256([]byte(shape.SQL))
	return "q_" + shape.Name + "_" + hex.EncodeToString(sum[:4])
}
