package daemon

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"fmt"

	"github.com/MateusAMP2119/iris-engine-cli/internal/config"
)

// This file holds the engine key: the ed25519 keypair minted once at
// `iris engine install`, whose signature seals the tamper-evidence checkpoint
// chain (specification section 4). The spec states the private half lives "in
// meta" and the public half is surfaced by `iris engine info`.
//
// # Where the private half lives
//
// The private key is persisted as a row in the engine-owned single-row engine_key
// meta table (id pinned to 1), added by the devdebt 2026-07-10 spec delta. This
// supersedes two earlier realizations, both flawed: a per-database GUC
// (`ALTER DATABASE meta SET iris.engine_key`) which needs SUPERUSER the external
// admin role lacks (specification section 2 grants it only CREATEDB), so it never
// worked in external mode; and a workspace key file, which forces a shared
// filesystem for HA. The meta table is superuser-free and gives HA via the shared
// meta database standbys already read, so a restarted or failover leader signs the
// same chain. store owns the bytes (internal/store/enginekey.go); this file owns
// the crypto: minting, the create-once insert DDL, byte decoding, public-half
// derivation, and redaction.
//
// The engine key never renders through any formatting path (like the admin DSN):
// only PublicBase64 exposes material, and only ever the public half; the one
// statement that carries the private half is InsertEngineKeyDDL, which callers must
// never log.

// engineKeyRedacted is what every formatting path renders in place of the engine
// key, so a stray %v/%s/%#v can never leak the private half.
const engineKeyRedacted = "EngineKey(REDACTED)"

// ErrEngineNotInstalled is returned by an EngineKeyReader when the engine key
// cannot be read: the engine is not installed, or its meta database is
// unreachable. `iris engine info` maps it to an operation-failed exit with a clear
// message. Callers test it with errors.Is.
var ErrEngineNotInstalled = errors.New("daemon: engine not installed or its meta database is unreachable; the engine key could not be read")

// EngineKey is the engine's ed25519 keypair. It holds the private key and exposes
// only the public half; the private material never renders through fmt, String, or
// GoString, exactly like the admin DSN.
type EngineKey struct {
	// private is the full 64-byte ed25519 private key (seed followed by public
	// half). Unexported so no reflection-based encoder can serialize it.
	private ed25519.PrivateKey
}

// MintEngineKey mints a fresh ed25519 engine keypair from crypto/rand: the key
// minted once at install (specification section 4). Each call is an independent
// keypair.
func MintEngineKey() (EngineKey, error) {
	_, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		return EngineKey{}, fmt.Errorf("daemon: mint engine key: %w", err)
	}
	return EngineKey{private: priv}, nil
}

// DecodeEngineKey reconstructs an EngineKey from a base64-encoded ed25519 private
// half. It fails fast on material that is not base64 or not a valid-length ed25519
// private key rather than accepting a malformed key. It is the string form used by
// tests and callers that hold a base64 rendering; DecodeEngineKeyBytes is the raw
// form the meta store returns.
func DecodeEngineKey(privateBase64 string) (EngineKey, error) {
	raw, err := base64.StdEncoding.DecodeString(privateBase64)
	if err != nil {
		return EngineKey{}, fmt.Errorf("daemon: decode engine key: %w", err)
	}
	return DecodeEngineKeyBytes(raw)
}

// DecodeEngineKeyBytes reconstructs an EngineKey from the raw ed25519 private-key
// bytes the engine_key meta table stores (what store.JournalSealReader.ReadEngineKey
// returns). It copies the bytes so the key does not alias the caller's buffer, and
// fails fast on a wrong length rather than accepting a malformed key.
func DecodeEngineKeyBytes(priv []byte) (EngineKey, error) {
	if len(priv) != ed25519.PrivateKeySize {
		return EngineKey{}, fmt.Errorf("daemon: decode engine key: got %d bytes, want an ed25519 private key of %d", len(priv), ed25519.PrivateKeySize)
	}
	return EngineKey{private: ed25519.PrivateKey(append([]byte(nil), priv...))}, nil
}

// PublicBase64 returns the base64-encoded public half of the engine key: the value
// `iris engine info` exposes and an offline auditor validates checkpoints with. It
// is the only material EngineKey exposes.
func (k EngineKey) PublicBase64() string {
	pub, _ := k.private.Public().(ed25519.PublicKey)
	return base64.StdEncoding.EncodeToString(pub)
}

// privateBytes returns a copy of the raw ed25519 private half. It is unexported:
// only InsertEngineKeyDDL and the leader-side seal's mint path read it, to persist
// the key. The private half never leaves the package as raw material any other way.
func (k EngineKey) privateBytes() []byte {
	return append([]byte(nil), k.private...)
}

// InsertEngineKeyDDL is the statement that persists the engine key in meta: a
// create-once INSERT into the single-row engine_key table (id pinned to 1) that
// records the raw private half as a bytea hex literal and does nothing on conflict,
// so two candidates that both mint converge on one key. It is issued on the meta
// connection at install (the connection is already on the meta database, so the
// table is unqualified). It is the one place the private half appears in a
// statement; callers must never log the statement. The leader-side seal mints
// through store.Writer.InsertEngineKey instead (a bind parameter, no literal).
func InsertEngineKeyDDL(k EngineKey) string {
	return fmt.Sprintf("INSERT INTO engine_key (id, private_key, created_at) VALUES (1, '\\x%x', now()::text) ON CONFLICT (id) DO NOTHING;", k.privateBytes())
}

// Format implements fmt.Formatter, redacting the engine key under every verb (fmt
// consults it before String, GoString, or struct reflection), so no formatting
// path can render the private half.
func (EngineKey) Format(f fmt.State, _ rune) { _, _ = f.Write([]byte(engineKeyRedacted)) }

// String implements fmt.Stringer, redacting the engine key for direct callers.
func (EngineKey) String() string { return engineKeyRedacted }

// GoString implements fmt.GoStringer, redacting the engine key for direct callers.
func (EngineKey) GoString() string { return engineKeyRedacted }

// valid reports whether the key carries private material (a zero EngineKey does
// not). BootstrapEngine rejects a zero key so install never stores empty material.
func (k EngineKey) valid() bool { return len(k.private) == ed25519.PrivateKeySize }

// SignDigest returns the ed25519 signature over the given digest (the checkpoint's
// own digest). This is the signature stored in journal_checkpoints.signature.
// (S04/checkpoint-ed25519-signature, S14/checkpoint-digest-chain)
func (k EngineKey) SignDigest(digest []byte) ([]byte, error) {
	if !k.valid() {
		return nil, fmt.Errorf("daemon: sign digest: invalid engine key")
	}
	return ed25519.Sign(k.private, digest), nil
}

// VerifyDigest reports whether sig is a valid ed25519 signature over digest for
// this key's public half. Used to verify checkpoint signatures.
// (S04/checkpoint-ed25519-signature)
func (k EngineKey) VerifyDigest(digest, sig []byte) bool {
	if !k.valid() {
		return false
	}
	pub, _ := k.private.Public().(ed25519.PublicKey)
	return ed25519.Verify(pub, digest, sig)
}

// Public returns a copy of the public key (for offline chain validation etc).
func (k EngineKey) Public() ed25519.PublicKey {
	pub, _ := k.private.Public().(ed25519.PublicKey)
	return append(ed25519.PublicKey(nil), pub...)
}

// EngineKeyReader reads the engine key back from where install stored it, so
// `iris engine info` can derive and show its public half. The live meta-connection
// reader lands with the daemon's connection wiring; a test fake and the
// unwired production reader both satisfy it until then.
type EngineKeyReader interface {
	// ReadEngineKey returns the stored engine key, or ErrEngineNotInstalled when it
	// cannot be read.
	ReadEngineKey(ctx context.Context) (EngineKey, error)
}

// NewEngineKeyReader returns the production engine-key reader for the given
// settings. The reader that opens a live meta connection and reads the engine_key
// table's private half lands with the daemon's pgx-backed connection wiring (a
// later task); until then this reader reports ErrEngineNotInstalled, so `iris engine
// info` fails clearly rather than pretending to read a key it cannot yet reach. The
// settings are accepted now so the signature is stable when the live reader replaces
// this body.
func NewEngineKeyReader(_ config.Settings) EngineKeyReader {
	return unwiredEngineKeyReader{}
}

// unwiredEngineKeyReader is the placeholder production reader: with no live
// meta-connection wiring yet, every read reports ErrEngineNotInstalled.
type unwiredEngineKeyReader struct{}

// ReadEngineKey reports ErrEngineNotInstalled: the live meta-connection read is not
// wired yet.
func (unwiredEngineKeyReader) ReadEngineKey(context.Context) (EngineKey, error) {
	return EngineKey{}, ErrEngineNotInstalled
}
