package daemon_test

import (
	"crypto/ed25519"
	"encoding/base64"
	"fmt"
	"strings"
	"testing"

	"github.com/MateusAMP2119/iris-engine-cli/internal/daemon"
)

// quotedPayload extracts the single-quoted SQL string literal from a one-line
// ALTER DATABASE ... SET ... = '<payload>'; statement. It is how the tests read
// back the base64 the engine-key DDL carries without the daemon package exposing
// the private half.
func quotedPayload(t *testing.T, stmt string) string {
	t.Helper()
	i := strings.Index(stmt, "'")
	j := strings.LastIndex(stmt, "'")
	if i < 0 || j <= i {
		t.Fatalf("no single-quoted payload in %q", stmt)
	}
	return stmt[i+1 : j]
}

// TestEngineKeyPublicHalfDerivedFromStoredPrivate proves the engine key's public
// half is derived from the private half persisted in meta: reading back the
// base64 the ALTER DATABASE ... SET iris.engine_key statement stores yields the
// same public half the minting side exposes, while the private material never
// renders through any formatting path. This is the mechanism `iris engine info`
// stands on -- it shows the public half of the key whose private half lives in
// meta (specification section 4, bootstrap Q/A).
//
// spec: S04/engine-key-public-via-info
func TestEngineKeyPublicHalfDerivedFromStoredPrivate(t *testing.T) {
	k, err := daemon.MintEngineKey()
	if err != nil {
		t.Fatalf("MintEngineKey: %v", err)
	}

	// The DDL that stores the key in meta carries the base64 private half; reading
	// it back (as current_setting would) and deriving the public half reproduces
	// exactly the public half the minting side exposes.
	privB64 := quotedPayload(t, daemon.SetEngineKeyDDL(k))
	back, err := daemon.DecodeEngineKey(privB64)
	if err != nil {
		t.Fatalf("DecodeEngineKey(stored private): %v", err)
	}
	if back.PublicBase64() != k.PublicBase64() {
		t.Errorf("public half after storing/reading private differs: minted %q, read-back %q",
			k.PublicBase64(), back.PublicBase64())
	}

	// The public half is a real 32-byte ed25519 public key, and is not the private
	// half (they are distinct material).
	pub, err := base64.StdEncoding.DecodeString(k.PublicBase64())
	if err != nil {
		t.Fatalf("public half is not base64: %v", err)
	}
	if len(pub) != ed25519.PublicKeySize {
		t.Errorf("public half is %d bytes, want %d", len(pub), ed25519.PublicKeySize)
	}
	if k.PublicBase64() == privB64 {
		t.Error("public half equals the stored private half; the private half must not be exposed as the public one")
	}

	// The private half never renders: no formatting verb, String, or GoString leaks
	// the base64 private material.
	for _, rendered := range []string{
		fmt.Sprintf("%v", k), fmt.Sprintf("%s", k), fmt.Sprintf("%#v", k), fmt.Sprintf("%d", k),
		k.String(), k.GoString(),
	} {
		if strings.Contains(rendered, privB64) {
			t.Errorf("engine key leaked its private half under formatting: %q", rendered)
		}
	}
}

// TestEngineKeyMintedFreshAndValidated proves the engine key minted at install is
// a fresh ed25519 keypair (distinct per mint, crypto/rand), that its storage DDL
// is a single ALTER DATABASE meta SET iris.engine_key statement, and that decoding
// rejects malformed material rather than silently accepting it.
//
// spec: S04/install-bootstraps-engine
func TestEngineKeyMintedFreshAndValidated(t *testing.T) {
	k1, err := daemon.MintEngineKey()
	if err != nil {
		t.Fatalf("MintEngineKey: %v", err)
	}
	k2, err := daemon.MintEngineKey()
	if err != nil {
		t.Fatalf("MintEngineKey: %v", err)
	}
	if k1.PublicBase64() == k2.PublicBase64() {
		t.Error("two mints produced the same key; the engine key must be a fresh crypto/rand keypair per install")
	}

	// The storage DDL is a single ALTER DATABASE on the meta database that sets the
	// documented setting name; it never creates a table or touches the roster.
	ddl := daemon.SetEngineKeyDDL(k1)
	for _, want := range []string{"ALTER DATABASE meta", "SET iris.engine_key", "= '"} {
		if !strings.Contains(ddl, want) {
			t.Errorf("engine-key DDL missing %q: %s", want, ddl)
		}
	}
	if strings.Count(ddl, ";") != 1 {
		t.Errorf("engine-key DDL is not a single statement: %s", ddl)
	}

	// Round-trip: the base64 the DDL carries decodes to the same key.
	back, err := daemon.DecodeEngineKey(quotedPayload(t, ddl))
	if err != nil {
		t.Fatalf("DecodeEngineKey(round-trip): %v", err)
	}
	if back.PublicBase64() != k1.PublicBase64() {
		t.Errorf("round-tripped key public half differs: %q vs %q", back.PublicBase64(), k1.PublicBase64())
	}

	// Malformed material fails fast rather than passing as a key.
	for _, bad := range []string{"", "not-base64!!", base64.StdEncoding.EncodeToString([]byte("too short"))} {
		if _, err := daemon.DecodeEngineKey(bad); err == nil {
			t.Errorf("DecodeEngineKey(%q) = nil error, want a failure on malformed key material", bad)
		}
	}
}
