package store

import (
	"context"
	"errors"
	"fmt"
)

// This file is the PAT authentication read surface of specification sections 4 and
// 7: the plain-MVCC meta read that resolves a token prefix (pats.id) to the durable
// record a bearer-token verifier checks -- the argon2id hash, the revoked flag, the
// scope-row union, and, for a data-scope PAT, the engine-managed read role it owns
// (roles.pat -> pg_role). It is a read, never a write, so it rides the reader pool
// on any node (a standby authenticates TCP reads exactly as the leader does). The
// raw token never appears here: the verifier hashes the presented token and
// compares against the stored hash; this read carries only the stored hash out.

// ErrPATNotFound is returned by LookupPAT when no pats row matches the token
// prefix: the token names a PAT that was never minted (or its prefix is garbage).
// The verifier maps it to a 401, indistinguishable to the caller from a bad
// secret. Callers test it with errors.Is.
var ErrPATNotFound = errors.New("store: no PAT matches the presented token prefix")

// PATAuth is the durable authentication record a token prefix resolves to: the
// stored argon2id hash the verifier compares the presented token against, the
// revoked flag (a revoked PAT fails authentication), the union of the PAT's scope
// rows, and the engine-managed read role a data-scope PAT owns (empty otherwise).
// It never carries the raw token.
type PATAuth struct {
	// ID is the token prefix (pats.id) this record was looked up by.
	ID string
	// Hash is the stored argon2id hash (pats.hash); the verifier compares the
	// presented token against it and never against a raw token.
	Hash string
	// Revoked reports whether the PAT is revoked (pats.revoked); a revoked PAT
	// authenticates to nothing.
	Revoked bool
	// Scopes are the PAT's scope-row values (pat_scopes.scope), the raw union the
	// verifier maps to the effective authority.
	Scopes []string
	// DataRole is the engine-managed NOLOGIN read role the PAT owns (roles.pat ->
	// pg_role), empty for a PAT without a data role. It is the role every data-surface
	// read this PAT makes executes as, via SET ROLE on the shared read pool.
	DataRole string
}

// PATReader resolves a token prefix to its authentication record. The pgx-pool
// reader is the production implementation; a fake stands in for tests. It is a
// plain MVCC read, so it never blocks behind the single writer or the leader lock.
type PATReader interface {
	// LookupPAT returns the PAT record for the token prefix id, or ErrPATNotFound
	// when no such PAT exists.
	LookupPAT(ctx context.Context, id string) (PATAuth, error)
}

// lookupPATSQL resolves one token prefix to its hash, revoked flag, the union of
// its scope rows, and the read role it owns. The scope union and the role are
// aggregated so exactly one row comes back regardless of how many scope rows the
// PAT carries. The FILTER drops the NULL a scope-less LEFT JOIN would produce; the
// coalesce yields an empty array rather than NULL. A data PAT owns exactly one
// role (roles.pat is unique per PAT here), so max() collapses the join deterministically.
const lookupPATSQL = `SELECT p.hash, p.revoked,
       coalesce(array_agg(DISTINCT s.scope) FILTER (WHERE s.scope IS NOT NULL), ARRAY[]::text[]),
       coalesce(max(r.pg_role), '')
FROM pats p
LEFT JOIN pat_scopes s ON s.pat_id = p.id
LEFT JOIN roles r ON r.pat = p.id
WHERE p.id = $1
GROUP BY p.hash, p.revoked`

// pgxPATReader is the pgx-pool-backed PATReader: a plain MVCC read on the meta pool.
type pgxPATReader struct {
	pool readPool
}

// compile-time proof the pgx reader satisfies the seam.
var _ PATReader = (*pgxPATReader)(nil)

// LookupPAT reads the authentication record for a token prefix. A prefix with no
// pats row is ErrPATNotFound (the token names no minted PAT); any other read error
// is wrapped. It reads only the durable record -- the raw token never enters this
// path.
func (r *pgxPATReader) LookupPAT(ctx context.Context, id string) (PATAuth, error) {
	if id == "" {
		return PATAuth{}, ErrPATNotFound
	}
	rows, err := r.pool.query(ctx, lookupPATSQL, id)
	if err != nil {
		return PATAuth{}, fmt.Errorf("store: look up PAT %q: %w", id, err)
	}
	defer rows.Close()

	if !rows.Next() {
		if err := rows.Err(); err != nil {
			return PATAuth{}, fmt.Errorf("store: look up PAT %q: %w", id, err)
		}
		return PATAuth{}, ErrPATNotFound
	}
	auth := PATAuth{ID: id}
	if err := rows.Scan(&auth.Hash, &auth.Revoked, &auth.Scopes, &auth.DataRole); err != nil {
		return PATAuth{}, fmt.Errorf("store: scan PAT %q: %w", id, err)
	}
	if err := rows.Err(); err != nil {
		return PATAuth{}, fmt.Errorf("store: look up PAT %q: %w", id, err)
	}
	return auth, nil
}
