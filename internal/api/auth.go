package api

import (
	"context"
	"errors"
	"net/http"
	"strings"
)

// TokenVerifier resolves a PAT presented over a TCP request (TCP carries
// Authorization: Bearer <token> per request) to the authority it carries. It is
// the seam the real PAT store (argon2id prefix+hash lookup over the E09.1 store)
// plugs into; until that wiring lands the daemon uses RejectAllVerifier, the
// honest state of a fresh deployment with no PAT minted.
type TokenVerifier interface {
	// VerifyToken resolves the bearer token: on success it returns the PAT's
	// authority (identity plus minted scopes) the mux scope-checks each route
	// against; a non-nil error rejects the request (the daemon answers 401).
	// The context is the request's, so a verifier that reaches a store honors
	// cancellation.
	VerifyToken(ctx context.Context, token string) (Authority, error)
}

// ErrNoPATsMinted is the rejection the default verifier returns: TCP control is
// authenticated-only, and no PAT exists yet, so every TCP request is 401 until
// one is minted (`iris pat create`, E09.1). It is a sentinel callers may match
// with errors.Is.
var ErrNoPATsMinted = errors.New("no PATs minted yet: TCP access requires a PAT (mint one with `iris pat create`)")

// RejectAllVerifier is the default TCP token verifier: it rejects every token
// with ErrNoPATsMinted. It is deliberately honest -- the TCP listener is
// authenticated-only, and before E09.1 mints the first PAT there is nothing to
// authenticate against -- so every TCP request answers 401 until the real
// verifier lands.
func RejectAllVerifier() TokenVerifier { return rejectAllVerifier{} }

// rejectAllVerifier rejects every token.
type rejectAllVerifier struct{}

// VerifyToken always rejects, naming the no-PAT state.
func (rejectAllVerifier) VerifyToken(context.Context, string) (Authority, error) {
	return Authority{}, ErrNoPATsMinted
}

// RequirePAT wraps h so every request must present a valid PAT as
// Authorization: Bearer <token>, verified by v. A missing/malformed header or a
// rejected token is 401 unauthorized and the
// wrapped handler is never reached; an accepted token passes the request through
// carrying the PAT's resolved authority in its context, which the mux
// scope-checks per route. The daemon wraps this around the shared mux for the
// TCP listener only -- unix-socket requests are ambient and never see it.
func RequirePAT(v TokenVerifier, h http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		token, ok := bearerToken(r.Header.Get("Authorization"))
		if !ok {
			WriteError(w, http.StatusUnauthorized, "unauthorized",
				"TCP access requires Authorization: Bearer <pat>")
			return
		}
		a, err := v.VerifyToken(r.Context(), token)
		if err != nil {
			WriteError(w, http.StatusUnauthorized, "unauthorized", err.Error())
			return
		}
		h.ServeHTTP(w, r.WithContext(WithAuthority(r.Context(), a)))
	})
}

// bearerToken extracts the token from an Authorization header value, requiring
// the "Bearer " scheme (case-insensitive) and a non-empty token. It reports
// false for a missing header, a different scheme, or an empty token.
func bearerToken(header string) (string, bool) {
	const prefix = "bearer "
	if len(header) <= len(prefix) || !strings.EqualFold(header[:len(prefix)], prefix) {
		return "", false
	}
	token := strings.TrimSpace(header[len(prefix):])
	if token == "" {
		return "", false
	}
	return token, true
}
