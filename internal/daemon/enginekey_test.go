package daemon_test

import (
	"crypto/ed25519"
	"encoding/base64"
	"encoding/hex"
	"fmt"
	"strings"
	"testing"

	"github.com/MateusAMP2119/iris-engine-cli/internal/daemon"
	"github.com/MateusAMP2119/iris-engine-cli/internal/store"
)

// quotedPayload extracts the single-quoted SQL string literal from a one-line
// statement (the engine-key INSERT's bytea hex literal). It is how the tests read
// back the private half the engine-key DDL carries without the daemon package
// exposing a raw accessor.
func quotedPayload(t *testing.T, stmt string) string {
	t.Helper()
	i := strings.Index(stmt, "'")
	j := strings.LastIndex(stmt, "'")
	if i < 0 || j <= i {
		t.Fatalf("no single-quoted payload in %q", stmt)
	}
	return stmt[i+1 : j]
}

// privBytesFromInsertDDL decodes the raw ed25519 private key bytes from the bytea
// hex literal (\x<hex>) the engine-key INSERT statement carries.
func privBytesFromInsertDDL(t *testing.T, ddl string) []byte {
	t.Helper()
	lit := quotedPayload(t, ddl)
	if !strings.HasPrefix(lit, `\x`) {
		t.Fatalf("engine-key insert literal %q is not a bytea hex literal", lit)
	}
	raw, err := hex.DecodeString(lit[2:])
	if err != nil {
		t.Fatalf("decode engine-key bytea hex literal: %v", err)
	}
	return raw
}

// TestEngineKeyPublicHalfDerivedFromStoredPrivate proves the engine key's public
// half is derived from the private half persisted in meta: reading back the bytes
// the engine_key INSERT statement stores yields the same public half the minting
// side exposes, while the private material never renders through any formatting
// path. This is the mechanism `iris engine info` stands on -- it shows the public
// half of the key whose private half lives in the engine_key meta table.
func TestEngineKeyPublicHalfDerivedFromStoredPrivate(t *testing.T) {
	k, err := daemon.MintEngineKey()
	if err != nil {
		t.Fatalf("MintEngineKey: %v", err)
	}

	// The DDL that stores the key in meta carries the raw private half; reading it
	// back (as the meta store returns it) and deriving the public half reproduces
	// exactly the public half the minting side exposes.
	privBytes := privBytesFromInsertDDL(t, daemon.InsertEngineKeyDDL(k))
	back, err := daemon.DecodeEngineKeyBytes(privBytes)
	if err != nil {
		t.Fatalf("DecodeEngineKeyBytes(stored private): %v", err)
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

	// The private half never renders: no formatting verb, String, or GoString leaks
	// the private material (in hex or base64 form).
	privHex := fmt.Sprintf("%x", privBytes)
	privB64 := base64.StdEncoding.EncodeToString(privBytes)
	for _, rendered := range []string{
		fmt.Sprintf("%v", k), fmt.Sprintf("%s", k), fmt.Sprintf("%#v", k), fmt.Sprintf("%d", k),
		k.String(), k.GoString(),
	} {
		if strings.Contains(rendered, privHex) || strings.Contains(rendered, privB64) {
			t.Errorf("engine key leaked its private half under formatting: %q", rendered)
		}
	}
}

// TestEngineKeyMintedFreshAndValidated proves the engine key minted at install is
// a fresh ed25519 keypair (distinct per mint, crypto/rand), that its storage DDL
// is a single create-once INSERT into the engine_key meta table (ON CONFLICT DO
// NOTHING), and that decoding rejects malformed material rather than silently
// accepting it.
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

	// The storage DDL is a single create-once INSERT into the engine_key meta table;
	// it is superuser-free (no ALTER DATABASE / GUC) and pins the single row (id, 1).
	ddl := daemon.InsertEngineKeyDDL(k1)
	for _, want := range []string{"INSERT INTO engine_key", "private_key", "ON CONFLICT (id) DO NOTHING", `\x`} {
		if !strings.Contains(ddl, want) {
			t.Errorf("engine-key DDL missing %q: %s", want, ddl)
		}
	}
	if strings.Contains(ddl, "ALTER DATABASE") {
		t.Errorf("engine-key DDL still uses the superuser-only GUC form: %s", ddl)
	}
	if strings.Count(ddl, ";") != 1 {
		t.Errorf("engine-key DDL is not a single statement: %s", ddl)
	}

	// Round-trip: the bytes the DDL carries decode to the same key.
	back, err := daemon.DecodeEngineKeyBytes(privBytesFromInsertDDL(t, ddl))
	if err != nil {
		t.Fatalf("DecodeEngineKeyBytes(round-trip): %v", err)
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

// TestCheckpointSignatureAndChain proves ed25519 signatures over checkpoint
// digests, parent chaining, per-sealed production of a checkpoint row, digest
// chaining on seal, and tamper detection.
func TestCheckpointSignatureAndChain(t *testing.T) {
	t.Run("checkpoint-ed25519-signature", func(t *testing.T) {
		k, err := daemon.MintEngineKey()
		if err != nil {
			t.Fatalf("mint: %v", err)
		}
		digest := []byte("compacted-rows-digest-bytes")
		sig, err := k.SignDigest(digest)
		if err != nil {
			t.Fatalf("SignDigest: %v", err)
		}
		if len(sig) != ed25519.SignatureSize {
			t.Errorf("sig len=%d want %d", len(sig), ed25519.SignatureSize)
		}
		if !k.VerifyDigest(digest, sig) {
			t.Error("VerifyDigest failed for own signature over digest")
		}
		if k.VerifyDigest(append([]byte{}, digest...), append([]byte{0}, sig...)) {
			t.Error("VerifyDigest accepted tampered sig")
		}
		pub := k.Public()
		if !ed25519.Verify(pub, digest, sig) {
			t.Error("direct ed25519.Verify failed")
		}
	})

	t.Run("checkpoint-parent-chain", func(t *testing.T) {
		cases := []struct {
			name    string
			prev    *store.CheckpointRow
			next    store.CheckpointRow
			wantErr bool
		}{
			{
				name: "first has no parent",
				prev: nil,
				next: store.CheckpointRow{Digest: store.ComputeDigest([][]byte{[]byte("first")})},
			},
			{
				name: "chains to prior digest",
				prev: &store.CheckpointRow{Digest: store.ComputeDigest([][]byte{[]byte("row1"), []byte("row2")})},
				next: store.CheckpointRow{
					Digest:       store.ComputeDigest([][]byte{[]byte("row3")}),
					ParentDigest: store.ParentFor(&store.CheckpointRow{Digest: store.ComputeDigest([][]byte{[]byte("row1"), []byte("row2")})}),
				},
			},
		}
		for _, c := range cases {
			c := c
			t.Run(c.name, func(t *testing.T) {
				gotParent := store.ParentFor(c.prev)
				if c.prev != nil {
					if !bytesEqual(gotParent, c.prev.Digest) {
						t.Error("parent_digest does not chain to prior digest")
					}
				} else if len(gotParent) != 0 {
					t.Error("first checkpoint must have nil/empty parent_digest")
				}
				chain := []store.CheckpointRow{}
				if c.prev != nil {
					chain = append(chain, *c.prev)
				}
				chain = append(chain, c.next)
				if err := store.ValidateChain(chain, nil); (err != nil) != c.wantErr {
					t.Errorf("ValidateChain err=%v wantErr=%v", err, c.wantErr)
				}
			})
		}
	})

	t.Run("checkpoint-per-sealed-partition", func(t *testing.T) {
		// Table: sealing one partition always yields exactly one CheckpointRow
		// with id_from/id_to and digest over the compacted rows (in id order).
		cases := []struct {
			name      string
			idFrom    int64
			idTo      int64
			compacted [][]byte
		}{
			{"one partition", 10, 11, [][]byte{[]byte("id=10|op=insert"), []byte("id=11|op=update")}},
			{"single row partition", 42, 42, [][]byte{[]byte("only")}},
		}
		for _, c := range cases {
			c := c
			t.Run(c.name, func(t *testing.T) {
				row := store.CheckpointForSealed(c.idFrom, c.idTo, c.compacted, nil)
				if row.IDFrom != c.idFrom || row.IDTo != c.idTo {
					t.Error("id_from/id_to not set for sealed partition")
				}
				wantDig := store.ComputeDigest(c.compacted)
				if !bytesEqual(row.Digest, wantDig) {
					t.Error("digest not computed over compacted rows in id order")
				}
				// exactly one row produced per sealed
				if row.IDFrom > row.IDTo && len(c.compacted) > 0 {
					t.Error("invalid range but one row per seal")
				}
			})
		}
	})

	t.Run("checkpoint-digest-chain", func(t *testing.T) {
		k, _ := daemon.MintEngineKey()
		cases := []struct {
			name      string
			compacted [][]byte
			idFrom    int64
			idTo      int64
		}{
			{"first sealed", [][]byte{[]byte("r1")}, 1, 5},
			{"second chained", [][]byte{[]byte("r2")}, 6, 10},
		}
		var prev *store.CheckpointRow
		for i, c := range cases {
			c := c
			t.Run(c.name, func(t *testing.T) {
				d := store.ComputeDigest(c.compacted)
				cp := store.CheckpointForSealed(c.idFrom, c.idTo, c.compacted, prev)
				cp.Seq = int64(i + 1)
				cp.Digest = d // ensure (same)
				sig, _ := k.SignDigest(d)
				cp.Signature = sig

				chain := []store.CheckpointRow{cp}
				if prev != nil {
					chain = append([]store.CheckpointRow{*prev}, chain...)
				}
				if err := store.ValidateChain(chain, k.Public()); err != nil {
					t.Fatalf("valid chain should verify: %v", err)
				}
				prev = &cp
			})
		}
	})

	t.Run("chain-detects-tamper", func(t *testing.T) {
		k, _ := daemon.MintEngineKey()
		d1 := store.ComputeDigest([][]byte{[]byte("a")})
		cp1 := store.CheckpointRow{Seq: 1, Digest: d1, Signature: mustSign(t, k, d1)}
		d2 := store.ComputeDigest([][]byte{[]byte("b")})
		cp2 := store.CheckpointRow{Seq: 2, Digest: d2, ParentDigest: d1, Signature: mustSign(t, k, d2)}

		cases := []struct {
			name    string
			chain   []store.CheckpointRow
			wantErr bool
		}{
			{"tampered digest", func() []store.CheckpointRow {
				b := []store.CheckpointRow{cp1, cp2}
				b[1].Digest = []byte("tampered")
				return b
			}(), true},
			{"broken parent_digest", []store.CheckpointRow{cp1, {Seq: 2, Digest: d2, ParentDigest: []byte("wrong"), Signature: mustSign(t, k, d2)}}, true},
			{"bad signature", []store.CheckpointRow{cp1, {Seq: 2, Digest: d2, ParentDigest: d1, Signature: []byte("bad")}}, true},
			{"lost middle (parent mismatch on non-adj)", []store.CheckpointRow{cp1, {Seq: 3, Digest: store.ComputeDigest([][]byte{[]byte("c")}), ParentDigest: []byte("lost"), Signature: mustSign(t, k, store.ComputeDigest([][]byte{[]byte("c")}))}}, true},
		}
		for _, c := range cases {
			c := c
			t.Run(c.name, func(t *testing.T) {
				if err := store.ValidateChain(c.chain, k.Public()); (err != nil) != c.wantErr {
					t.Errorf("ValidateChain error = %v, wantErr=%v (tamper or loss must fail visibly)", err, c.wantErr)
				}
			})
		}
	})
}

func mustSign(t *testing.T, k daemon.EngineKey, d []byte) []byte {
	t.Helper()
	s, err := k.SignDigest(d)
	if err != nil {
		t.Fatalf("sign: %v", err)
	}
	return s
}

func bytesEqual(a, b []byte) bool {
	if len(a) == 0 && len(b) == 0 {
		return true
	}
	return string(a) == string(b)
}
