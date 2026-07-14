package daemon

import (
	"context"
	"errors"
	"fmt"

	"github.com/MateusAMP2119/iris-engine-cli/internal/api"
	"github.com/MateusAMP2119/iris-engine-cli/internal/pat"
	"github.com/MateusAMP2119/iris-engine-cli/internal/store"
)

// This file is the production TCP bearer-token verifier: the api.TokenVerifier the
// daemon wraps around the shared mux for the TCP listener, resolving
// "Authorization: Bearer <token>" to the PAT's authority. It is the real seam that
// replaces api.RejectAllVerifier once a PAT store exists. The daemon is the
// composition root, so the verifier lives here: it composes the pat leaf (parse +
// argon2id verify + scope union) with the store PAT reader (the prefix -> record
// lookup over the meta pool), reaching neither into api's internals nor giving
// store a dependency on pat.
//
// The verification is deliberately uniform: a malformed token, an unknown prefix, a
// revoked PAT, and a wrong secret all reject the same way, so a caller learns only
// "not authenticated", never which of those it was. The lookup is a plain MVCC read,
// so a standby verifies TCP reads exactly as the leader does.

// storeVerifier resolves a bearer token against the meta PAT store. It holds the
// plain-MVCC PAT reader; each request parses the token, looks the prefix up, and
// verifies the secret against the stored argon2id hash in constant time.
type storeVerifier struct {
	pats store.PATReader
}

// compile-time proof the store verifier satisfies the api seam.
var _ api.TokenVerifier = (*storeVerifier)(nil)

// newStoreVerifier builds the production verifier over the meta PAT reader.
func newStoreVerifier(pats store.PATReader) *storeVerifier {
	return &storeVerifier{pats: pats}
}

// errRejected is the uniform rejection every failed verification returns, so the
// caller cannot distinguish a malformed token, an unknown prefix, a revoked PAT, or
// a wrong secret. RequirePAT renders it as a 401.
var errRejected = errors.New("invalid or unknown PAT")

// VerifyToken resolves the bearer token to its authority: it parses the token into
// its prefix and secret, looks the prefix up in the meta PAT store, rejects a
// revoked PAT, verifies the presented token against the stored argon2id hash in
// constant time, and returns the PAT's minted scopes plus its data read role. Every
// failure -- a malformed token, an unknown prefix, a revoked PAT, a hash mismatch --
// returns the same uniform rejection, so the response leaks nothing about which it
// was. A read fault (not a miss) surfaces as an internal error rather than the
// uniform rejection, so a broken meta pool is not silently reported as bad auth.
func (v *storeVerifier) VerifyToken(ctx context.Context, token string) (api.Authority, error) {
	parsed, err := pat.ParseToken(token)
	if err != nil {
		return api.Authority{}, errRejected
	}
	rec, err := v.pats.LookupPAT(ctx, parsed.ID())
	if err != nil {
		if errors.Is(err, store.ErrPATNotFound) {
			return api.Authority{}, errRejected
		}
		return api.Authority{}, fmt.Errorf("daemon: verify PAT %q: %w", parsed.ID(), err)
	}
	if rec.Revoked {
		return api.Authority{}, errRejected
	}
	ok, err := pat.Verify(token, rec.Hash)
	if err != nil || !ok {
		return api.Authority{}, errRejected
	}

	scopes := make([]pat.Scope, 0, len(rec.Scopes))
	for _, s := range rec.Scopes {
		scopes = append(scopes, pat.Scope(s))
	}
	return api.Authority{
		PATID:    rec.ID,
		Scopes:   pat.EffectiveAuthority(scopes),
		DataRole: rec.DataRole,
	}, nil
}
