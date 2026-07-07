package api

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/MateusAMP2119/iris-engine-cli/internal/pat"
)

// fakeVerifier accepts exactly one bearer token and rejects every other, so the
// PAT-gate middleware can be proven to distinguish an authenticated request from
// an unauthenticated one.
type fakeVerifier struct{ good string }

// VerifyToken accepts only the configured token, resolving it to a full-scope
// authority.
func (f fakeVerifier) VerifyToken(_ context.Context, tok string) (Authority, error) {
	if tok == f.good {
		return Authority{PATID: "fake", Scopes: []pat.Scope{pat.ScopeControl, pat.ScopeRead, pat.ScopeData}}, nil
	}
	return Authority{}, errors.New("api: unknown token")
}

// okHandler is the trivial downstream handler the middleware protects: it writes
// a 200 with a body, so a reached downstream is observable.
func okHandler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, "reached")
	})
}

// TestRequirePATGatesTCP proves the TCP-side PAT gate (specification sections 2
// and 7): every request must carry Authorization: Bearer <pat>; a missing or
// rejected token is 401 and never reaches the downstream handler, a valid token
// passes through, and the default reject-all verifier -- the honest deployment
// state before any PAT is minted -- 401s even a well-formed bearer with a clear
// "no PATs minted yet" detail.
func TestRequirePATGatesTCP(t *testing.T) {
	// spec: S02/tcp-opt-in-pat-gated
	t.Run("S02/tcp-opt-in-pat-gated", func(t *testing.T) {
		guarded := RequirePAT(fakeVerifier{good: "secret"}, okHandler())

		// No Authorization header: 401, downstream never reached.
		if code, body := do(t, guarded, ""); code != http.StatusUnauthorized {
			t.Errorf("no bearer: status = %d, want 401 (body %q)", code, body)
		} else if strings.Contains(body, "reached") {
			t.Errorf("no bearer: downstream was reached: %q", body)
		}

		// A wrong bearer: 401, downstream never reached.
		if code, body := do(t, guarded, "Bearer wrong"); code != http.StatusUnauthorized {
			t.Errorf("wrong bearer: status = %d, want 401 (body %q)", code, body)
		} else if strings.Contains(body, "reached") {
			t.Errorf("wrong bearer: downstream was reached: %q", body)
		}

		// The right bearer: 200, downstream reached.
		if code, body := do(t, guarded, "Bearer secret"); code != http.StatusOK {
			t.Errorf("valid bearer: status = %d, want 200 (body %q)", code, body)
		} else if !strings.Contains(body, "reached") {
			t.Errorf("valid bearer: downstream not reached: %q", body)
		}

		// A 401 body is the read-API error envelope with the closed unauthorized code.
		_, body := do(t, guarded, "")
		var env struct {
			Error struct {
				Code    string `json:"code"`
				Message string `json:"message"`
			} `json:"error"`
		}
		if err := json.Unmarshal([]byte(body), &env); err != nil {
			t.Fatalf("401 body is not a JSON error envelope: %v (%q)", err, body)
		}
		if env.Error.Code != "unauthorized" {
			t.Errorf("401 envelope code = %q, want unauthorized", env.Error.Code)
		}

		// The default deployment verifier rejects every token: no PAT exists yet, so
		// even a well-formed bearer is 401 with an honest "no PATs" detail (E09.1
		// supplies the real verifier).
		rejectAll := RequirePAT(RejectAllVerifier(), okHandler())
		code, body := do(t, rejectAll, "Bearer anything")
		if code != http.StatusUnauthorized {
			t.Errorf("reject-all verifier: status = %d, want 401", code)
		}
		if !strings.Contains(strings.ToLower(body), "pat") {
			t.Errorf("reject-all 401 detail does not mention PATs: %q", body)
		}
	})
}

// do sends a GET through h with the given Authorization header (empty for none)
// and returns the status code and body.
func do(t *testing.T, h http.Handler, authz string) (int, string) {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, "/healthz", nil)
	if authz != "" {
		req.Header.Set("Authorization", authz)
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec.Code, rec.Body.String()
}
