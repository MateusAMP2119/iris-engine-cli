package daemon

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"path/filepath"
	"strings"
	"sync"

	"github.com/MateusAMP2119/iris-engine-cli/internal/api"
	"github.com/MateusAMP2119/iris-engine-cli/internal/declare"
	"github.com/MateusAMP2119/iris-engine-cli/internal/dispatch"
	"github.com/MateusAMP2119/iris-engine-cli/internal/pat"
	"github.com/MateusAMP2119/iris-engine-cli/internal/pg"
	"github.com/MateusAMP2119/iris-engine-cli/internal/store"
)

// This file is the daemon's leader-side PAT-mint plane: the composition root that
// turns POST /pat/create into a minted token, its expanded read grants, the
// provisioned data-database read role, and the atomic meta persistence -- then
// returns the show-once token exactly once. It is leader-only: the api mux gates
// the mutation to the leader, and the single meta writer only exists once a
// candidate wins the lock, so the orchestrator is installed on winning leadership
// and cleared on demotion; a swappable patPlane holds it and satisfies
// api.PATMintHandler for the daemon's whole life.
//
// The mint order is deliberate: mint the token, expand the read grants against the
// leader's declared fields and applied endpoints, provision the NOLOGIN data role
// on the data database with those grants (data first, so a failed provision never
// leaves a PAT that cannot read), then persist the pats, pat_scopes, role, and
// grant rows as one atomic meta transaction, and finally return the show-once
// token. A failed provision or persist mints nothing durable.

// patPlane is the daemon's api.PATMintHandler: a stable handle the mux binds to for
// the daemon's whole life, delegating to the live orchestrator when the daemon leads
// and faulting internally otherwise.
type patPlane struct {
	mu   sync.RWMutex
	live *patMintOrchestrator
}

// compile-time proof the plane is the mux's PAT mint handler.
var _ api.PATMintHandler = (*patPlane)(nil)

// newPATPlane returns an unwired PAT mint plane: mints fault until a leader installs
// an orchestrator.
func newPATPlane() *patPlane { return &patPlane{} }

// install wires the live orchestrator (on winning leadership).
func (p *patPlane) install(o *patMintOrchestrator) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.live = o
}

// clear removes the orchestrator (on demotion).
func (p *patPlane) clear() {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.live = nil
}

func (p *patPlane) orchestrator() *patMintOrchestrator {
	p.mu.RLock()
	defer p.mu.RUnlock()
	return p.live
}

// CreatePAT routes to the live orchestrator, or faults when none is installed.
func (p *patPlane) CreatePAT(ctx context.Context, req api.PATCreateRequest) (api.PATCreateResult, error) {
	o := p.orchestrator()
	if o == nil {
		return api.PATCreateResult{}, api.ErrPATMintUnavailable
	}
	return o.mint(ctx, req)
}

// patMintOrchestrator runs the leader-side PAT mint against the workspace and the
// databases. It composes the pat leaf (mint/hash), the grant expander (over the
// leader's declared fields and applied endpoints), the data-database role
// provisioner, and the single meta writer.
type patMintOrchestrator struct {
	workspace string
	submit    dispatch.Submitter
	data      pg.DB
	endpoints *dispatch.EndpointRegistry
	logger    *slog.Logger
}

// newPATMintOrchestrator builds the leader's mint orchestrator over its workspace
// root, the single-writer submitter (this term's dispatcher), the data-database DDL
// client (data-PAT role provisioning), and the live endpoint registry (--endpoint
// grant expansion). A nil logger discards output.
func newPATMintOrchestrator(workspace string, submit dispatch.Submitter, data pg.DB, endpoints *dispatch.EndpointRegistry, logger *slog.Logger) *patMintOrchestrator {
	if logger == nil {
		logger = slog.New(slog.NewTextHandler(io.Discard, nil))
	}
	return &patMintOrchestrator{workspace: workspace, submit: submit, data: data, endpoints: endpoints, logger: logger}
}

// mint mints, provisions, and persists one PAT. It validates the scope set, mints a
// fresh token and its argon2id hash, and -- for a data-scope PAT -- expands its
// --read/--endpoint grants against the leader's declared fields and applied
// endpoints, provisions the NOLOGIN read role on the data database (with those
// grants and membership in the engine read-pool login), and persists the
// pats/pat_scopes/role/grants rows as one atomic meta transaction. It returns the
// show-once token exactly once. Any failure mints nothing durable.
func (o *patMintOrchestrator) mint(ctx context.Context, req api.PATCreateRequest) (api.PATCreateResult, error) {
	scopes, err := pat.ParseScopes(req.Scopes)
	if err != nil {
		return api.PATCreateResult{}, err
	}
	if err := pat.ValidateScopes(scopes); err != nil {
		return api.PATCreateResult{}, err
	}
	hasData := false
	for _, s := range scopes {
		if s == pat.ScopeData {
			hasData = true
		}
	}
	if (len(req.Reads) > 0 || len(req.Endpoints) > 0) && !hasData {
		return api.PATCreateResult{}, fmt.Errorf("--read and --endpoint require the data scope")
	}

	token, err := pat.Mint()
	if err != nil {
		return api.PATCreateResult{}, fmt.Errorf("mint token: %w", err)
	}
	hash, err := pat.Hash(token)
	if err != nil {
		return api.PATCreateResult{}, fmt.Errorf("hash token: %w", err)
	}

	rec := store.PATRecord{
		ID:     token.ID(),
		Hash:   hash,
		Label:  req.Label,
		Scopes: scopeStrings(pat.EffectiveAuthority(scopes)),
	}

	var dataRole string
	if hasData {
		grants, err := o.expandGrants(req)
		if err != nil {
			return api.PATCreateResult{}, err
		}
		dataRole = pat.DataRoleName(token.ID())
		if err := pg.ProvisionDataPATRole(ctx, o.data, pg.DataPATRoleProvision{
			Role:          dataRole,
			PoolLoginRole: pg.EngineReadPoolRole,
			MetaDatabase:  store.MetaDatabase,
			DataDatabase:  pg.DataDatabase,
			Grants:        grants,
		}); err != nil {
			return api.PATCreateResult{}, fmt.Errorf("provision data-PAT read role: %w", err)
		}
		rec.DataRole = dataRole
		rec.DataGrants = fieldGrantsToStore(grants)
	}

	if err := o.submit.Submit(ctx, func(w *store.Writer) error {
		return w.CreatePAT(ctx, rec)
	}); err != nil {
		return api.PATCreateResult{}, fmt.Errorf("persist PAT: %w", err)
	}

	return api.PATCreateResult{
		ID:       token.ID(),
		Token:    token.Reveal(),
		Scopes:   rec.Scopes,
		DataRole: dataRole,
	}, nil
}

// expandGrants resolves the request's --read/--endpoint specs into the fixed
// per-field read grant set recorded at mint, against the leader's declared fields
// (schemas/ tree) and applied endpoints (the live registry). An unknown table or
// endpoint, or a malformed spec, is an error naming it.
func (o *patMintOrchestrator) expandGrants(req api.PATCreateRequest) ([]declare.FieldGrant, error) {
	reads := make([]declare.DataPATRead, 0, len(req.Reads)+len(req.Endpoints))
	for _, spec := range req.Reads {
		r, err := parseReadSpec(spec)
		if err != nil {
			return nil, err
		}
		reads = append(reads, r)
	}
	for _, name := range req.Endpoints {
		reads = append(reads, declare.DataPATRead{Endpoint: name})
	}

	declaredFields, err := o.declaredFields()
	if err != nil {
		return nil, err
	}
	endpoints, err := o.endpointSources(req.Endpoints)
	if err != nil {
		return nil, err
	}
	return declare.ExpandDataPATGrants(reads, declaredFields, endpoints)
}

// declaredFields maps each declared "schema.table" to its declared column names, as
// of mint, from the leader's schemas/ tree -- the fixed field set a bare-table
// --read grant expands to.
func (o *patMintOrchestrator) declaredFields() (map[string][]string, error) {
	schemasDir := filepath.Join(o.workspace, "schemas")
	tables, err := declare.ValidateSchemaTree(schemasDir)
	if err != nil {
		return nil, fmt.Errorf("read schemas tree: %w", err)
	}
	out := make(map[string][]string, len(tables))
	for _, dt := range tables {
		fields := make([]string, 0, len(dt.Spec.Columns))
		for _, c := range dt.Spec.Columns {
			fields = append(fields, c.Name)
		}
		out[dt.Schema+"."+dt.Table] = fields
	}
	return out, nil
}

// endpointSources maps each requested endpoint name to its source and fields, from
// the live serving registry (an applied endpoint). An endpoint not applied is an
// error, so an --endpoint grant never resolves against a shape that is not live.
func (o *patMintOrchestrator) endpointSources(names []string) (map[string]declare.EndpointSource, error) {
	out := make(map[string]declare.EndpointSource, len(names))
	for _, name := range names {
		ce, ok := o.endpoints.Endpoint(name)
		if !ok {
			return nil, fmt.Errorf("unknown endpoint %q (apply it with `iris endpoint apply` before granting it)", name)
		}
		out[name] = declare.EndpointSource{Source: ce.Schema + "." + ce.Table, Fields: ce.Fields}
	}
	return out, nil
}

// parseReadSpec parses one --read spec into a DataPATRead: "schema.table.field"
// grants that one field; "schema.table" grants every field the table declares at
// mint. Anything else is a malformed spec.
func parseReadSpec(spec string) (declare.DataPATRead, error) {
	parts := strings.Split(spec, ".")
	switch len(parts) {
	case 2:
		return declare.DataPATRead{Table: spec}, nil
	case 3:
		return declare.DataPATRead{Table: parts[0] + "." + parts[1], Field: parts[2]}, nil
	default:
		return declare.DataPATRead{}, fmt.Errorf("malformed --read %q: want schema.table or schema.table.field", spec)
	}
}

// scopeStrings renders scopes as their string values for the meta record.
func scopeStrings(scopes []pat.Scope) []string {
	out := make([]string, len(scopes))
	for i, s := range scopes {
		out[i] = string(s)
	}
	return out
}

// fieldGrantsToStore maps declare field grants to the store grant rows CreatePAT
// records; every data-PAT grant is a read grant.
func fieldGrantsToStore(grants []declare.FieldGrant) []store.Grant {
	out := make([]store.Grant, len(grants))
	for i, g := range grants {
		out[i] = store.Grant{Schema: g.Schema, Table: g.Table, Field: g.Field, Access: store.AccessRead}
	}
	return out
}
