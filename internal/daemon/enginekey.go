package daemon

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"fmt"

	"github.com/MateusAMP2119/iris-engine-cli/internal/config"
	"github.com/MateusAMP2119/iris-engine-cli/internal/store"
)

// This file holds the engine key: the ed25519 keypair minted once at
// `iris engine install`, whose signature seals the tamper-evidence checkpoint
// chain (specification section 4). The spec states the private half lives "in
// meta" and the public half is surfaced by `iris engine info`.
//
// # Where the private half lives (a design choice flagged for spec review)
//
// The eighteen-table meta roster is closed (S04/eighteen-table-roster), so the
// private key is not a table row. It is persisted instead as a per-database GUC on
// the meta database: `ALTER DATABASE meta SET iris.engine_key = '<base64>'`, which
// Postgres records in pg_db_role_setting -- persistent, inside Postgres, tied to
// the meta database, and read back with current_setting('iris.engine_key') on a
// meta connection. This keeps "private half in meta" literally true without adding
// a table, at the cost of storing the key as a database setting rather than a row.
// It is deliberately documented here and in the E02.4 report as a point for spec
// review. The signing that consumes the key is E-later work; this file owns only
// minting, persistence DDL, public-half derivation, and redaction.
//
// The engine key never renders through any formatting path (like the admin DSN):
// only PublicBase64 exposes material, and only ever the public half.

// EngineKeySetting is the Postgres per-database setting name the meta database
// carries the base64-encoded ed25519 private key under. It is read back with
// current_setting on a meta connection.
const EngineKeySetting = "iris.engine_key"

// ReadEngineKeyQuery is the query a live meta connection runs to read the stored
// engine key back (its base64 private half). The pgx-backed reader that runs it
// lands with the daemon's live-connection wiring; NewEngineKeyReader stands in
// until then.
const ReadEngineKeyQuery = "SELECT current_setting('" + EngineKeySetting + "')"

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

// DecodeEngineKey reconstructs an EngineKey from the base64-encoded private half
// stored in meta (what current_setting('iris.engine_key') returns). It fails fast
// on material that is not base64 or not a valid-length ed25519 private key rather
// than accepting a malformed key.
func DecodeEngineKey(privateBase64 string) (EngineKey, error) {
	raw, err := base64.StdEncoding.DecodeString(privateBase64)
	if err != nil {
		return EngineKey{}, fmt.Errorf("daemon: decode engine key: %w", err)
	}
	if len(raw) != ed25519.PrivateKeySize {
		return EngineKey{}, fmt.Errorf("daemon: decode engine key: got %d bytes, want an ed25519 private key of %d", len(raw), ed25519.PrivateKeySize)
	}
	return EngineKey{private: ed25519.PrivateKey(raw)}, nil
}

// PublicBase64 returns the base64-encoded public half of the engine key: the value
// `iris engine info` exposes and an offline auditor validates checkpoints with. It
// is the only material EngineKey exposes.
func (k EngineKey) PublicBase64() string {
	pub, _ := k.private.Public().(ed25519.PublicKey)
	return base64.StdEncoding.EncodeToString(pub)
}

// privateBase64 returns the base64-encoded private half. It is unexported: only
// SetEngineKeyDDL reads it, to build the storage statement. The private half never
// leaves the package any other way.
func (k EngineKey) privateBase64() string {
	return base64.StdEncoding.EncodeToString(k.private)
}

// SetEngineKeyDDL is the statement that persists the engine key in meta: an ALTER
// DATABASE meta SET iris.engine_key that records the base64 private half as a
// per-database setting (see the package/file doc for why a setting rather than a
// table row). It is issued on the meta connection at install. It is the one place
// the private half appears in a statement; callers must never log the statement.
func SetEngineKeyDDL(k EngineKey) string {
	return fmt.Sprintf("ALTER DATABASE %s SET %s = '%s';", store.MetaDatabase, EngineKeySetting, k.privateBase64())
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
// settings. The reader that opens a live meta connection and runs ReadEngineKeyQuery
// lands with the daemon's pgx-backed connection wiring (a later task); until then
// this reader reports ErrEngineNotInstalled, so `iris engine info` fails clearly
// rather than pretending to read a key it cannot yet reach. The settings are
// accepted now so the signature is stable when the live reader replaces this body.
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
