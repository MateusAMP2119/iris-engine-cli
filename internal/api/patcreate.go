package api

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
)

// This file is the control-plane PAT-mint surface of the daemon: POST
// /pat/create, the route `iris pat create` drives. Minting and persisting a PAT
// are meta writes (the pats/pat_scopes rows and, for a data PAT, its read role
// and grants) plus a data-database role provisioning, so it is a leader-only
// control-plane mutation: the mux's leader gate rejects it on a standby with
// not_leader guidance, and its scope is control. On the leader it runs the
// injected PATMintHandler, which the daemon wires to the mint orchestrator
// (mint token, expand grants, provision the read role, persist, return the
// show-once token). The raw token is in the response exactly once; the
// leader/CLI print it and it is never recoverable.

// PATCreateRequest is the body of a POST /pat/create: the requested scope set, an
// optional label, and the data-PAT read grant specs (--read and --endpoint). The
// CLI validates scopes locally and sends the raw grant specs; the leader expands
// them against its own workspace's declared fields and applied endpoints.
type PATCreateRequest struct {
	// Scopes is the requested non-empty scope subset of {control, read, data}.
	Scopes []string `json:"scopes"`
	// Label is the human label recorded for the PAT.
	Label string `json:"label,omitempty"`
	// Reads are the raw --read grant specs: "schema.table.field" (one field) or
	// "schema.table" (every field the table declares at mint). Data scope only.
	Reads []string `json:"reads,omitempty"`
	// Endpoints are the raw --endpoint grant specs: an endpoint name whose source
	// fields the data PAT is granted. Data scope only.
	Endpoints []string `json:"endpoints,omitempty"`
}

// PATCreateResult is the success payload of a mint: the token id (prefix, safe to
// store and log), the raw show-once token, the recorded scope set, and the read
// role a data PAT owns (empty otherwise). The token is exposed exactly once.
type PATCreateResult struct {
	// ID is the token prefix (pats.id).
	ID string `json:"id"`
	// Token is the raw show-once token: printed once, never recoverable.
	Token string `json:"token"`
	// Scopes are the PAT's recorded scopes, in canonical order.
	Scopes []string `json:"scopes"`
	// DataRole is the engine-managed read role a data PAT owns, empty otherwise.
	DataRole string `json:"data_role,omitempty"`
}

// PATMintHandler runs the leader-side PAT mint. The daemon implements it over the
// pat leaf (mint/hash), the grant expander, the data-database role provisioner, and
// the single meta writer; the mux depends only on this interface. An operation
// failure (a bad grant spec, an unknown endpoint, a provisioning failure) is
// rendered as the operation-failed envelope.
type PATMintHandler interface {
	// CreatePAT mints, provisions, and persists the PAT described by req, returning
	// the show-once token.
	CreatePAT(ctx context.Context, req PATCreateRequest) (PATCreateResult, error)
}

// ErrPATMintUnavailable is returned by the default (unwired) mint handler: a
// mutation reached the mux but no handler is installed. The leader installs it
// before reporting the leader role and the mux gates mutations to the leader, so
// this is an internal fault, not an operation failure.
var ErrPATMintUnavailable = errors.New("api: PAT mint plane not available")

// WithPATMint wires the leader-side PAT mint handler the mux routes POST
// /pat/create to. A nil handler is ignored, keeping the safe default (the mutation
// faults internally until a real handler is installed).
func WithPATMint(h PATMintHandler) MuxOption {
	return func(m *mux) {
		if h != nil {
			m.patMint = h
		}
	}
}

// noPATMint is the default handler before one is wired: every mint is an internal
// fault, never a silent success.
type noPATMint struct{}

func (noPATMint) CreatePAT(context.Context, PATCreateRequest) (PATCreateResult, error) {
	return PATCreateResult{}, ErrPATMintUnavailable
}

// servePATCreate handles POST /pat/create: decode the request, run the leader's
// mint, and render the data envelope carrying the show-once token. The leader
// gate ran already in ServeHTTP; a malformed body is 400, an operation failure
// 422, an internal fault 500.
func (m *mux) servePATCreate(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		WriteError(w, http.StatusMethodNotAllowed, string(CodeMethodNotAllowed), "POST "+r.URL.Path+" only")
		return
	}
	var req PATCreateRequest
	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()
	if err := dec.Decode(&req); err != nil {
		WriteError(w, http.StatusBadRequest, CodeBadRequest, "malformed pat create request body: "+err.Error())
		return
	}
	res, err := m.patMint.CreatePAT(r.Context(), req)
	if err != nil {
		if errors.Is(err, ErrPATMintUnavailable) {
			WriteError(w, http.StatusInternalServerError, "internal", err.Error())
			return
		}
		WriteError(w, http.StatusUnprocessableEntity, CodeOpFailed, err.Error())
		return
	}
	WriteData(w, http.StatusOK, res)
}
