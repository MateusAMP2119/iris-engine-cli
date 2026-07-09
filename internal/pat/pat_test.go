package pat_test

import (
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/MateusAMP2119/iris-engine-cli/internal/pat"
)

// This file proves the PAT leaf's pure logic (specification sections 2 and 7): the
// scope algebra over {control, read, data}, the effective-authority union, and the
// show-once token substrate -- mint, argon2id hashing, and constant-time verify.
// Every test is pure (no I/O), matching the unit tier of its contracts.

// TestValidateScopes proves iris pat create accepts any non-empty subset of the
// closed scope set {control, read, data} and rejects an empty set or one naming an
// unknown scope. The scope algebra is the leaf the CLI and the read path both gate
// on, so an unknown or empty scope is refused here before any PAT is minted.
//
// spec: S07/pat-scope-subset-validation
func TestValidateScopes(t *testing.T) {
	t.Run("S07/pat-scope-subset-validation", func(t *testing.T) {
		// Every non-empty subset of {control, read, data} is accepted.
		accepted := [][]pat.Scope{
			{pat.ScopeControl},
			{pat.ScopeRead},
			{pat.ScopeData},
			{pat.ScopeControl, pat.ScopeRead},
			{pat.ScopeRead, pat.ScopeData},
			{pat.ScopeControl, pat.ScopeData},
			{pat.ScopeControl, pat.ScopeRead, pat.ScopeData},
		}
		for _, scopes := range accepted {
			if err := pat.ValidateScopes(scopes); err != nil {
				t.Errorf("ValidateScopes(%v) = %v, want accepted", scopes, err)
			}
		}

		// The empty set is rejected: a PAT with no scope gates nothing.
		if err := pat.ValidateScopes(nil); !errors.Is(err, pat.ErrEmptyScopeSet) {
			t.Errorf("ValidateScopes(nil) = %v, want ErrEmptyScopeSet", err)
		}
		if err := pat.ValidateScopes([]pat.Scope{}); !errors.Is(err, pat.ErrEmptyScopeSet) {
			t.Errorf("ValidateScopes(empty) = %v, want ErrEmptyScopeSet", err)
		}

		// A set naming a scope outside the closed set is rejected, naming the bad scope.
		unknown := []pat.Scope{pat.ScopeControl, "admin"}
		err := pat.ValidateScopes(unknown)
		if err == nil {
			t.Fatalf("ValidateScopes(%v) accepted an unknown scope", unknown)
		}
		if !strings.Contains(err.Error(), "admin") {
			t.Errorf("ValidateScopes error %q does not name the unknown scope", err)
		}
	})
}

// TestParseScopes proves the string parse maps the raw --scope tokens to the closed
// scope set, rejecting an unknown token so a mistyped scope never reaches the store.
//
// spec: S07/pat-scope-subset-validation
func TestParseScopes(t *testing.T) {
	t.Run("S07/pat-scope-subset-validation", func(t *testing.T) {
		got, err := pat.ParseScopes([]string{"control", "read", "data"})
		if err != nil {
			t.Fatalf("ParseScopes: %v", err)
		}
		want := []pat.Scope{pat.ScopeControl, pat.ScopeRead, pat.ScopeData}
		if len(got) != len(want) {
			t.Fatalf("ParseScopes returned %v, want %v", got, want)
		}
		for i := range want {
			if got[i] != want[i] {
				t.Errorf("ParseScopes[%d] = %q, want %q", i, got[i], want[i])
			}
		}

		if _, err := pat.ParseScopes([]string{"control", "superuser"}); err == nil {
			t.Errorf("ParseScopes accepted an unknown token")
		}
	})
}

// TestEffectiveAuthority proves a PAT's effective authority is the union of its
// pat_scopes rows: every distinct scope present, duplicates collapsed, in the
// canonical scope order regardless of the row order the store returns.
//
// spec: S04/pat-authority-scope-union
func TestEffectiveAuthority(t *testing.T) {
	t.Run("S04/pat-authority-scope-union", func(t *testing.T) {
		// Union of all three rows, given out of canonical order and with a duplicate.
		rows := []pat.Scope{pat.ScopeData, pat.ScopeControl, pat.ScopeRead, pat.ScopeControl}
		got := pat.EffectiveAuthority(rows)
		want := []pat.Scope{pat.ScopeControl, pat.ScopeRead, pat.ScopeData}
		if fmt.Sprint(got) != fmt.Sprint(want) {
			t.Errorf("EffectiveAuthority(%v) = %v, want the canonical union %v", rows, got, want)
		}

		// A single row is its own authority.
		if got := pat.EffectiveAuthority([]pat.Scope{pat.ScopeRead}); fmt.Sprint(got) != fmt.Sprint([]pat.Scope{pat.ScopeRead}) {
			t.Errorf("EffectiveAuthority([read]) = %v, want [read]", got)
		}

		// No rows: no authority (the empty union).
		if got := pat.EffectiveAuthority(nil); len(got) != 0 {
			t.Errorf("EffectiveAuthority(nil) = %v, want empty", got)
		}

		// The union carries every distinct scope: dropping one would shrink authority.
		full := pat.EffectiveAuthority([]pat.Scope{pat.ScopeControl, pat.ScopeRead, pat.ScopeData})
		if len(full) != 3 {
			t.Errorf("EffectiveAuthority of all three scopes = %v, want three-scope union", full)
		}
	})
}

// TestTokenMintHashVerify proves the show-once token substrate (specification
// sections 2 and 7): Mint returns a full token exactly once, argon2id Hash persists
// no recoverable secret, and Verify round-trips while rejecting a tampered token.
// It is the pat-side half of pat-show-once-hash (the store-side persistence half is
// proved in internal/store); together they show a lost token is unrecoverable, only
// revoked and re-minted.
//
// spec: S07/pat-show-once-hash
func TestTokenMintHashVerify(t *testing.T) {
	t.Run("S07/pat-show-once-hash", func(t *testing.T) {
		tok, err := pat.Mint()
		if err != nil {
			t.Fatalf("Mint: %v", err)
		}

		full := tok.Reveal()
		if full == "" {
			t.Fatal("Mint produced an empty token")
		}
		id := tok.ID()
		if id == "" {
			t.Fatal("Mint produced an empty token id (pats.id prefix)")
		}
		if !strings.Contains(full, id) {
			t.Errorf("full token %q does not carry its id prefix %q", full, id)
		}

		// Two mints never collide: the id and secret are random.
		tok2, err := pat.Mint()
		if err != nil {
			t.Fatalf("Mint (second): %v", err)
		}
		if tok2.ID() == id || tok2.Reveal() == full {
			t.Errorf("two mints collided: id %q, token %q", id, full)
		}

		// The raw token redacts on every formatting path: only Reveal exposes it, so a
		// stray log line can never print it (it is shown exactly once, at creation).
		for _, rendered := range []string{
			fmt.Sprintf("%v", tok),
			fmt.Sprintf("%s", tok),
			fmt.Sprintf("%#v", tok),
			fmt.Sprintf("%+v", tok),
			tok.String(),
			tok.GoString(),
		} {
			if rendered != "Token(REDACTED)" || strings.Contains(rendered, full) {
				t.Errorf("formatting path leaked the raw token: %q", rendered)
			}
		}

		// argon2id Hash of the token: a PHC-format argon2id string that carries no
		// recoverable secret (never the raw token, never its secret half).
		hash, err := pat.Hash(tok)
		if err != nil {
			t.Fatalf("Hash: %v", err)
		}
		if !strings.HasPrefix(hash, "$argon2id$") {
			t.Errorf("Hash %q is not an argon2id PHC string", hash)
		}
		if strings.Contains(hash, full) {
			t.Errorf("argon2id hash leaked the raw token: %q", hash)
		}

		// Verify round-trips the real token and rejects a tampered one (constant-time).
		ok, err := pat.Verify(full, hash)
		if err != nil {
			t.Fatalf("Verify(real): %v", err)
		}
		if !ok {
			t.Errorf("Verify rejected the real token against its own hash")
		}
		ok, err = pat.Verify(full+"x", hash)
		if err != nil {
			t.Fatalf("Verify(tampered): %v", err)
		}
		if ok {
			t.Errorf("Verify accepted a tampered token")
		}

		// The full token parses back into its id and secret (the header dispatch shape).
		parsed, err := pat.ParseToken(full)
		if err != nil {
			t.Fatalf("ParseToken(%q): %v", full, err)
		}
		if parsed.ID() != id || parsed.Reveal() != full {
			t.Errorf("ParseToken round-trip = (id %q, token %q), want (id %q, token %q)", parsed.ID(), parsed.Reveal(), id, full)
		}
		if _, err := pat.ParseToken("not-a-token"); err == nil {
			t.Errorf("ParseToken accepted a malformed token")
		}
	})
}

// TestDataRoleName proves the engine derives a stable, cluster-unique NOLOGIN role
// name for a data PAT from its token id, mirroring the pipeline role convention
// (iris_pipeline_<name> -> iris_pat_<id>).
//
// spec: S04/data-pat-owns-read-role
func TestDataRoleName(t *testing.T) {
	t.Run("S04/data-pat-owns-read-role", func(t *testing.T) {
		got := pat.DataRoleName("abc123")
		if got != "iris_pat_abc123" {
			t.Errorf("DataRoleName(abc123) = %q, want iris_pat_abc123", got)
		}
		if pat.DataRoleName("x") == pat.DataRoleName("y") {
			t.Errorf("DataRoleName collided across distinct ids")
		}
	})
}
