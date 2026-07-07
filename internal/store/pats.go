package store

import (
	"context"
	"fmt"
)

// This file is the unified PAT store write surface (specification sections 4 and 7):
// the meta writes that persist a minted PAT -- its token prefix (pats.id), argon2id
// hash, label, and scope rows (pat_scopes) -- and, for a data-scope PAT, its
// engine-managed read-only NOLOGIN Postgres role in the access ledger (roles and
// grants), with no credentials row (data-PAT roles are NOLOGIN; credentials holds
// pipeline login roles only). Every write rides the single meta writer.
//
// A PAT create is one atomic transaction: the pats row, its pat_scopes rows, and --
// when the PAT carries the data scope -- the roles row and its field-level grants all
// commit together, or none do. So meta never reflects a PAT with a missing scope row,
// or a data PAT whose read role was recorded without its grants.

// PATRecord is the durable PAT the store persists at mint: the token prefix
// (pats.id), its argon2id hash, its label, and its scope set -- never the raw token.
// The raw token is a show-once secret returned by the pat leaf's Mint and printed
// once by the leader; only this record's prefix and hash survive, so a lost token is
// unrecoverable (revoke + re-mint).
//
// For a PAT carrying the data scope, DataRole names the engine-managed read-only
// NOLOGIN Postgres role it owns and DataGrants its fixed field-level read grants;
// both are recorded in the same atomic transaction as the pats and pat_scopes rows.
// credentials is never touched (a data-PAT role is NOLOGIN, assumed via SET ROLE on
// the read path). DataRole is empty for a PAT without the data scope.
type PATRecord struct {
	// ID is the token prefix, the pats primary key and lookup key.
	ID string
	// Hash is the token's argon2id hash (pat leaf's Hash); the raw token is never stored.
	Hash string
	// Label is the human label recorded for the PAT.
	Label string
	// Scopes are the PAT's scope rows (pat_scopes.scope); must be non-empty. Duplicates
	// are collapsed so the pat_scopes primary key never trips.
	Scopes []string
	// DataRole, when set, is the engine-managed read-only NOLOGIN role the data-scope
	// PAT owns (owner=data-PAT in roles). Empty for a PAT without the data scope.
	DataRole string
	// DataGrants are the data-PAT role's fixed field-level read grants, recorded in
	// the same transaction as the role. Ignored when DataRole is empty.
	DataGrants []Grant
}

// The PAT write statements. Each is a single parameterized statement; CreatePAT
// groups the ones a mint needs into one atomic transaction.
const (
	// insertPATSQL writes the pats row keyed by token prefix. revoked is bound false
	// at mint (explicit, not defaulted); a lost token is later revoked (RevokePAT),
	// never recovered.
	insertPATSQL = `INSERT INTO pats (id, hash, label, revoked) VALUES ($1, $2, $3, $4)`

	// insertPATScopeSQL writes one pat_scopes row: the PAT's authority is the union of
	// these rows.
	insertPATScopeSQL = `INSERT INTO pat_scopes (pat_id, scope) VALUES ($1, $2)`

	// revokePATSQL flips a PAT's revoked flag by prefix, guarded by the id primary key.
	revokePATSQL = `UPDATE pats SET revoked = true WHERE id = $1`
)

// CreatePAT persists a minted PAT as one atomic meta transaction: the pats row (its
// prefix, argon2id hash, label, revoked=false) and one pat_scopes row per distinct
// scope, plus -- when the PAT owns a data-scope read role -- the roles row (owner=data
// PAT, NOLOGIN) and its field-level read grants (specification sections 4 and 7). No
// credentials row is ever written (data-PAT roles hold none). The whole batch commits
// together or not at all, so meta never reflects a PAT missing a scope row or a data
// role recorded without its grants. It is a leader-only meta write, riding the single
// Writer.
func (w *Writer) CreatePAT(ctx context.Context, rec PATRecord) error {
	if rec.ID == "" {
		return fmt.Errorf("store: writer create PAT: empty token prefix")
	}
	if rec.Hash == "" {
		return fmt.Errorf("store: writer create PAT %q: empty hash", rec.ID)
	}
	scopes := dedupeKeepOrder(rec.Scopes)
	if len(scopes) == 0 {
		return fmt.Errorf("store: writer create PAT %q: empty scope set", rec.ID)
	}

	stmts := []Statement{{SQL: insertPATSQL, Args: []any{rec.ID, rec.Hash, rec.Label, false}}}
	for _, s := range scopes {
		stmts = append(stmts, Statement{SQL: insertPATScopeSQL, Args: []any{rec.ID, s}})
	}
	// A data-scope PAT owns an engine-managed read-only role: record it (owner=data
	// PAT, so roles.pat is set and pipeline NULL) and its field-level read grants in
	// the same transaction. credentials is intentionally never written.
	if rec.DataRole != "" {
		stmts = append(stmts, Statement{SQL: registerRoleSQL, Args: []any{rec.DataRole, nil, rec.ID}})
		for _, g := range rec.DataGrants {
			stmts = append(stmts, Statement{SQL: insertGrantSQL, Args: []any{rec.DataRole, g.Schema, g.Table, g.Field, string(g.Access)}})
		}
	}

	if err := w.execTx(ctx, stmts); err != nil {
		return fmt.Errorf("store: writer create PAT %q: %w", rec.ID, err)
	}
	return nil
}

// RevokePAT marks a PAT revoked by flipping its pats.revoked flag, keyed on the token
// prefix (specification section 7: a lost token is revoked and re-minted). It is a
// single guarded statement -- the id primary key bounds it to the one PAT -- and a
// leader-only meta write, riding the single Writer. The role and grants a data PAT
// owns are left in place; a revoked PAT simply fails authentication.
func (w *Writer) RevokePAT(ctx context.Context, id string) error {
	if id == "" {
		return fmt.Errorf("store: writer revoke PAT: empty token prefix")
	}
	if err := w.conn.Exec(ctx, revokePATSQL, id); err != nil {
		return fmt.Errorf("store: writer revoke PAT %q: %w", id, err)
	}
	return nil
}
