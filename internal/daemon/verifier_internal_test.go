package daemon

import (
	"context"
	"errors"
	"testing"

	"github.com/MateusAMP2119/iris-lakehouse/internal/pat"
	"github.com/MateusAMP2119/iris-lakehouse/internal/store"
)

// fakePATReader is a store.PATReader over a fixed record set keyed by token prefix.
type fakePATReader struct {
	recs map[string]store.PATAuth
	err  error
}

func (f fakePATReader) LookupPAT(_ context.Context, id string) (store.PATAuth, error) {
	if f.err != nil {
		return store.PATAuth{}, f.err
	}
	rec, ok := f.recs[id]
	if !ok {
		return store.PATAuth{}, store.ErrPATNotFound
	}
	return rec, nil
}

// mintStored mints a real token and returns it with its stored PATAuth record
// (argon2id hash, scopes, and data role), as the meta store would hold it.
func mintStored(t *testing.T, scopes []string, dataRole string, revoked bool) (pat.Token, store.PATAuth) {
	t.Helper()
	tok, err := pat.Mint()
	if err != nil {
		t.Fatalf("Mint: %v", err)
	}
	hash, err := pat.Hash(tok)
	if err != nil {
		t.Fatalf("Hash: %v", err)
	}
	return tok, store.PATAuth{ID: tok.ID(), Hash: hash, Revoked: revoked, Scopes: scopes, DataRole: dataRole}
}

// TestStoreVerifierResolvesAuthority proves the TCP bearer-token verifier resolves a
// valid token to its minted scopes and data read role: the argon2id verify succeeds
// against the stored hash and the scope rows become the effective authority.
func TestStoreVerifierResolvesAuthority(t *testing.T) {
	tok, rec := mintStored(t, []string{"data", "read"}, "iris_pat_"+"deadbeef", false)
	v := newStoreVerifier(fakePATReader{recs: map[string]store.PATAuth{rec.ID: rec}})

	auth, err := v.VerifyToken(context.Background(), tok.Reveal())
	if err != nil {
		t.Fatalf("VerifyToken: %v", err)
	}
	if auth.PATID != tok.ID() {
		t.Errorf("PATID = %q, want %q", auth.PATID, tok.ID())
	}
	if auth.DataRole != rec.DataRole {
		t.Errorf("DataRole = %q, want %q", auth.DataRole, rec.DataRole)
	}
	if !auth.Allows(pat.ScopeData) || !auth.Allows(pat.ScopeRead) {
		t.Errorf("authority %+v does not carry the minted scopes", auth)
	}
	if auth.Allows(pat.ScopeControl) {
		t.Errorf("authority carries a scope it was never minted with")
	}
	if auth.Ambient {
		t.Errorf("a PAT authority is never ambient")
	}
}

// TestStoreVerifierRejectsUniformly proves every failed verification -- a malformed
// token, an unknown prefix, a revoked PAT, and a wrong secret -- rejects the same
// way, leaking nothing about which it was.
func TestStoreVerifierRejectsUniformly(t *testing.T) {
	good, goodRec := mintStored(t, []string{"data"}, "iris_pat_r", false)
	_, revokedRec := mintStored(t, []string{"data"}, "iris_pat_x", true)
	// A second real token whose prefix is not in the store (unknown), and one whose
	// secret does not match a stored record (wrong secret against good's prefix).
	other, _ := mintStored(t, []string{"data"}, "", false)

	v := newStoreVerifier(fakePATReader{recs: map[string]store.PATAuth{
		goodRec.ID:    goodRec,
		revokedRec.ID: revokedRec,
	}})

	// Sanity: the good token verifies.
	if _, err := v.VerifyToken(context.Background(), good.Reveal()); err != nil {
		t.Fatalf("good token should verify: %v", err)
	}

	cases := map[string]string{
		"malformed token": "not-a-real-token",
		"unknown prefix":  other.Reveal(),
		"revoked PAT":     mustReveal(t, revokedRec.ID),
		"wrong secret":    "irispat_" + goodRec.ID + "." + "wrongsecretwrongsecretwrongsecret",
	}
	for name, token := range cases {
		t.Run(name, func(t *testing.T) {
			if _, err := v.VerifyToken(context.Background(), token); !errors.Is(err, errRejected) {
				t.Fatalf("%s: err = %v, want the uniform rejection", name, err)
			}
		})
	}
}

// mustReveal reconstructs a plausible raw token for a stored prefix with an
// arbitrary secret: the revoked case rejects before the secret is ever checked, so
// the exact secret does not matter -- only the well-formed prefix does.
func mustReveal(t *testing.T, id string) string {
	t.Helper()
	return "irispat_" + id + "." + "anysecretanysecretanysecretanyse"
}
