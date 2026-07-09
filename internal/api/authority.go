package api

import (
	"context"
	"net/http"
	"strings"

	"github.com/MateusAMP2119/iris-engine-cli/internal/pat"
)

// This file is the request-authority model of the read API (specification
// section 7): who a request acts as, and which scope each route demands. The
// two transports resolve authority differently -- a unix-socket request is
// ambient (local, filesystem-guarded: full authority, no token), while a TCP
// request's bearer token resolves to the PAT's minted scopes -- but the mux
// checks every route against one Authority regardless of transport, so the
// scope split (a data-only PAT sees no engine internals, a read-only PAT no
// table data, control mutations demand control) is enforced in exactly one
// place. api imports pat (a leaf) for the closed scope set only.

// Authority is what a request is allowed to do: the resolved PAT identity and
// its minted scopes, or the ambient authority a unix-socket request carries.
// The zero value is deliberately powerless (no scopes, not ambient): only
// RequirePAT (a verified token) or the socket default grants authority.
type Authority struct {
	// PATID identifies the PAT the bearer token resolved to ("" for ambient).
	PATID string
	// Scopes are the PAT's minted scopes, any non-empty subset of
	// {control, read, data}.
	Scopes []pat.Scope
	// Ambient marks a unix-socket request: local and filesystem-guarded, it
	// passes every scope check without a PAT (specification section 7:
	// "Socket: ambient authorization").
	Ambient bool
}

// Allows reports whether the authority covers the given scope: ambient
// authority covers everything; a PAT covers exactly its minted scopes.
func (a Authority) Allows(s pat.Scope) bool {
	if a.Ambient {
		return true
	}
	for _, sc := range a.Scopes {
		if sc == s {
			return true
		}
	}
	return false
}

// authorityKey is the context key WithAuthority stores an Authority under.
type authorityKey struct{}

// WithAuthority returns a context carrying a: the request's resolved authority.
// RequirePAT attaches the verified PAT's authority for TCP requests; the unix
// socket serves the mux bare, so its requests carry none and default to ambient.
func WithAuthority(ctx context.Context, a Authority) context.Context {
	return context.WithValue(ctx, authorityKey{}, a)
}

// AuthorityFrom returns the request authority carried by ctx. A context with no
// authority is a unix-socket request (the socket serves the mux with no PAT
// gate), so the default is ambient -- the TCP path always attaches the verified
// authority before the mux runs.
func AuthorityFrom(ctx context.Context) Authority {
	if a, ok := ctx.Value(authorityKey{}).(Authority); ok {
		return a
	}
	return Authority{Ambient: true}
}

// requiredScope returns the scope the route at path demands (specification
// section 7: every route is scope-checked). The data surface (/data, /q)
// demands data; the control-plane mutations demand control; everything else --
// the engine-state roster, /healthz, /leader, and any unknown path about to
// 404 -- demands read, so a data-only PAT sees no engine internals and cannot
// even probe which engine-state routes exist.
func requiredScope(path string) pat.Scope {
	seg := strings.TrimPrefix(path, "/")
	if i := strings.IndexByte(seg, '/'); i >= 0 {
		seg = seg[:i]
	}
	switch seg {
	case "data", "q":
		return pat.ScopeData
	case "apply", "destroy":
		return pat.ScopeControl
	case "pipeline":
		// The control-plane pipeline verbs mutate (control); the listing is an
		// engine-state read.
		if path == "/pipeline/list" {
			return pat.ScopeRead
		}
		return pat.ScopeControl
	}
	// Known destructive mutation paths (exact) require control scope even though
	// their first segment may otherwise be a read surface (e.g. workload reads).
	// This implements the remote tiering for declare destroy, workload wipe,
	// deadletter drain over TCP (specification section 12).
	if path == "/deadletter/drain" || path == "/workload/wipe" {
		return pat.ScopeControl
	}
	return pat.ScopeRead
}

// authorize checks the request's authority against its route's required scope,
// writing the 403 forbidden envelope (specification section 7 status matrix:
// missing scope = 403) and reporting false when the scope is missing. It runs
// before routing, so every route -- read surfaces and control plane alike -- is
// scope-checked in one place.
func (m *mux) authorize(w http.ResponseWriter, r *http.Request) bool {
	need := requiredScope(r.URL.Path)
	if AuthorityFrom(r.Context()).Allows(need) {
		return true
	}
	WriteError(w, http.StatusForbidden, string(CodeForbidden),
		"missing scope: this route requires the "+string(need)+" scope")
	return false
}
