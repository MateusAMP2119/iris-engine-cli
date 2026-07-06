package api

import (
	"context"
	"errors"
	"net/http"
	"strings"
)

// TokenVerifier verifies a PAT presented over a TCP request (specification
// section 7: "TCP: Authorization: Bearer <token> per request"). It is the seam
// the real PAT store (argon2id prefix+hash, scope checks) plugs into at E09.1;
// until then the daemon uses RejectAllVerifier, the honest state of a fresh
// deployment with no PAT minted.
type TokenVerifier interface {
	// VerifyToken reports whether the bearer token authenticates: nil accepts the
	// request, a non-nil error rejects it (the daemon answers 401). The context is
	// the request's, so a verifier that reaches a store honors cancellation.
	VerifyToken(ctx context.Context, token string) error
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
func (rejectAllVerifier) VerifyToken(context.Context, string) error { return ErrNoPATsMinted }

// RequirePAT wraps h so every request must present a valid PAT as
// Authorization: Bearer <token>, verified by v (specification sections 2 and 7).
// A missing/malformed header or a rejected token is 401 unauthorized and the
// wrapped handler is never reached; an accepted token passes the request
// through. The daemon wraps this around the shared mux for the TCP listener only
// -- unix-socket requests are ambient and never see it.
func RequirePAT(v TokenVerifier, h http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		token, ok := bearerToken(r.Header.Get("Authorization"))
		if !ok {
			WriteError(w, http.StatusUnauthorized, "unauthorized",
				"TCP access requires Authorization: Bearer <pat>")
			return
		}
		if err := v.VerifyToken(r.Context(), token); err != nil {
			WriteError(w, http.StatusUnauthorized, "unauthorized", err.Error())
			return
		}
		h.ServeHTTP(w, r)
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
