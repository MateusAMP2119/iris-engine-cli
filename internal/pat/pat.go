// Package pat is the PAT (personal access token) leaf of specification sections 2
// and 7: minting, argon2id hashing and constant-time verification, and the scope
// algebra over {control, read, data}. It is a leaf package -- it depends on no
// other engine package -- so the store persists what Mint produces, the leader
// prints the show-once token, and the read path checks scopes, each without pulling
// in the others.
//
// The raw token is a show-once secret: Mint returns it exactly once, it redacts on
// every formatting path (so a stray log line can never print it), and only its
// argon2id hash and its id prefix are ever persisted. A lost token is therefore
// unrecoverable -- revoked and re-minted, never recovered.
package pat

import (
	"crypto/rand"
	"crypto/subtle"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"

	"golang.org/x/crypto/argon2"
)

// Scope is one PAT authority scope (pat_scopes.scope): control, read, or data. Its
// string value matches the pat_scopes CHECK set.
type Scope string

// The closed PAT scope set (specification section 7), in canonical order.
const (
	// ScopeControl authorizes the remote control plane (mutations via the leader).
	ScopeControl Scope = "control"
	// ScopeRead authorizes the read API (engine-state routes).
	ScopeRead Scope = "read"
	// ScopeData authorizes the data read path via an engine-managed read-only role.
	ScopeData Scope = "data"
)

// canonicalScopes is the closed scope set in canonical order: the order the CHECK
// pins and EffectiveAuthority emits, so a union is stable regardless of row order.
var canonicalScopes = []Scope{ScopeControl, ScopeRead, ScopeData}

// isKnownScope reports whether s is one of the closed scope set.
func isKnownScope(s Scope) bool {
	for _, k := range canonicalScopes {
		if s == k {
			return true
		}
	}
	return false
}

// ErrEmptyScopeSet is returned by ValidateScopes for an empty scope set: a PAT needs
// a non-empty subset of {control, read, data} (specification section 7). Callers
// test it with errors.Is.
var ErrEmptyScopeSet = errors.New("pat: empty scope set; a PAT needs a non-empty subset of {control, read, data}")

// ValidateScopes reports whether scopes is a valid PAT scope set: a non-empty subset
// of the closed set {control, read, data} (specification section 7). An empty set
// yields ErrEmptyScopeSet; a set naming a scope outside the closed set yields an
// error naming that scope. Duplicates are tolerated (a scope set is a union;
// EffectiveAuthority and the pat_scopes primary key collapse them).
func ValidateScopes(scopes []Scope) error {
	if len(scopes) == 0 {
		return ErrEmptyScopeSet
	}
	for _, s := range scopes {
		if !isKnownScope(s) {
			return fmt.Errorf("pat: unknown scope %q; valid scopes are control, read, data", s)
		}
	}
	return nil
}

// ParseScope maps a raw --scope token to a Scope, rejecting an unknown token so a
// mistyped scope is refused before it reaches the store.
func ParseScope(s string) (Scope, error) {
	sc := Scope(s)
	if !isKnownScope(sc) {
		return "", fmt.Errorf("pat: unknown scope %q; valid scopes are control, read, data", s)
	}
	return sc, nil
}

// ParseScopes maps raw --scope tokens to Scopes in order, rejecting the first
// unknown token. It does not validate non-emptiness (ValidateScopes owns that), so
// the caller can distinguish "no --scope given" from "an unknown scope".
func ParseScopes(raw []string) ([]Scope, error) {
	out := make([]Scope, 0, len(raw))
	for _, r := range raw {
		s, err := ParseScope(r)
		if err != nil {
			return nil, err
		}
		out = append(out, s)
	}
	return out, nil
}

// EffectiveAuthority returns a PAT's effective authority: the union of its
// pat_scopes rows (specification section 7). Every distinct scope present is
// included exactly once, in canonical scope order, regardless of the order or
// multiplicity the rows arrive in. An unknown scope value in the rows is ignored (it
// is not part of the closed authority set); the empty union is the empty slice.
func EffectiveAuthority(scopes []Scope) []Scope {
	present := make(map[Scope]bool, len(scopes))
	for _, s := range scopes {
		present[s] = true
	}
	out := make([]Scope, 0, len(canonicalScopes))
	for _, s := range canonicalScopes {
		if present[s] {
			out = append(out, s)
		}
	}
	return out
}

// tokenMarker is the fixed, greppable prefix every raw token carries, so a leaked
// token is recognizable to a secret scanner. The full token is
// "irispat_<id>.<secret>": the marker, the id (a lookup key, pats.id), a dot
// separator (absent from the base64url secret alphabet), and the secret.
const tokenMarker = "irispat_"

// tokenSep separates the id prefix from the secret in a raw token. It is not part
// of the hex id or the base64url secret alphabet, so the split is unambiguous.
const tokenSep = "."

const (
	// idBytes is the entropy of a token id (pats.id): 8 random bytes, hex-encoded to
	// a 16-char lookup key.
	idBytes = 8
	// secretBytes is the entropy of a token secret: 32 random bytes, base64url-encoded.
	secretBytes = 32
)

// Token is a minted PAT: its id prefix (the pats.id lookup key) and its secret half.
// The raw token (marker + id + secret) is a show-once value: Reveal exposes it -- the
// single deliberate exit -- while String, GoString, and Format all redact, so no
// formatting or logging path can print it. The zero Token is invalid (IsZero true).
type Token struct {
	// id is the token's lookup prefix (pats.id), hex of idBytes random bytes.
	id string
	// secret is the token's secret half, base64url of secretBytes random bytes. It is
	// unexported so no reflection-based encoder can serialize it.
	secret string
}

// ID returns the token's id: the pats.id lookup prefix, safe to store and log.
func (t Token) ID() string { return t.id }

// Reveal returns the full raw token (marker + id + secret). It is the single
// deliberate exit for the secret: the leader prints it exactly once at creation and
// never again. Callers must never store or log it -- a lost token is revoked and
// re-minted, never recovered.
func (t Token) Reveal() string {
	if t.IsZero() {
		return ""
	}
	return tokenMarker + t.id + tokenSep + t.secret
}

// Secret returns the token's secret half. Like Reveal it is a deliberate exit for
// the raw material (used to hash the token); it never appears in a formatting path.
func (t Token) Secret() string { return t.secret }

// IsZero reports whether the token is the zero value (no id or secret).
func (t Token) IsZero() bool { return t.id == "" && t.secret == "" }

// redacted is what every formatting path renders in place of the raw token.
const redacted = "Token(REDACTED)"

// Format implements fmt.Formatter, consulted before every other formatting path
// (String, GoString, and the struct reflection a numeric verb would fall through
// to). It writes the redacted marker for every verb, so no verb can render the
// secret.
func (Token) Format(f fmt.State, _ rune) { _, _ = f.Write([]byte(redacted)) }

// String implements fmt.Stringer, redacting the token for direct String() callers.
func (Token) String() string { return redacted }

// GoString implements fmt.GoStringer, redacting the token for %#v and direct
// GoString() callers.
func (Token) GoString() string { return redacted }

// Mint mints a fresh PAT from crypto/rand: a random hex id (pats.id) and a random
// base64url secret. It is the sole construction path for a new token, so a token is
// never caller-supplied. The returned Token's Reveal is the show-once value.
func Mint() (Token, error) {
	idBuf := make([]byte, idBytes)
	if _, err := rand.Read(idBuf); err != nil {
		return Token{}, fmt.Errorf("pat: mint token id: %w", err)
	}
	secretBuf := make([]byte, secretBytes)
	if _, err := rand.Read(secretBuf); err != nil {
		return Token{}, fmt.Errorf("pat: mint token secret: %w", err)
	}
	return Token{
		id:     hex.EncodeToString(idBuf),
		secret: base64.RawURLEncoding.EncodeToString(secretBuf),
	}, nil
}

// ParseToken parses a raw token string (as presented in an Authorization header)
// back into its id and secret, so the read path can look the PAT up by id and verify
// the secret against the stored hash. A string without the token marker or the id/
// secret separator is rejected.
func ParseToken(raw string) (Token, error) {
	rest, ok := strings.CutPrefix(raw, tokenMarker)
	if !ok {
		return Token{}, fmt.Errorf("pat: malformed token: missing %q marker", tokenMarker)
	}
	id, secret, ok := strings.Cut(rest, tokenSep)
	if !ok || id == "" || secret == "" {
		return Token{}, errors.New("pat: malformed token: not <id>.<secret>")
	}
	return Token{id: id, secret: secret}, nil
}

// argon2id parameters (specification section 9: argon2id for PAT hashing). They are
// the encoded cost recorded in every hash, so Verify recomputes with the same cost
// even if these defaults change later.
const (
	argonTime    = 1         // passes
	argonMemory  = 64 * 1024 // 64 MiB
	argonThreads = 4
	argonKeyLen  = 32
	argonSaltLen = 16
)

// Hash returns the argon2id hash of the token's raw value, in PHC string format
// ($argon2id$v=19$m=...,t=...,p=...$salt$hash), with a fresh random salt. Only this
// hash and the token id are persisted; the raw token is never stored, so the hash is
// the one-way record a lost token cannot be recovered from.
func Hash(t Token) (string, error) {
	if t.IsZero() {
		return "", errors.New("pat: hash of a zero token")
	}
	salt := make([]byte, argonSaltLen)
	if _, err := rand.Read(salt); err != nil {
		return "", fmt.Errorf("pat: hash: generate salt: %w", err)
	}
	return encodeHash(argonKey(t.Reveal(), salt), salt), nil
}

// argonKey derives the argon2id key for a raw token and salt at the fixed cost.
func argonKey(rawToken string, salt []byte) []byte {
	return argon2.IDKey([]byte(rawToken), salt, argonTime, argonMemory, argonThreads, argonKeyLen)
}

// encodeHash renders the PHC argon2id string for a derived key and its salt.
func encodeHash(key, salt []byte) string {
	return fmt.Sprintf("$argon2id$v=%d$m=%d,t=%d,p=%d$%s$%s",
		argon2.Version, argonMemory, argonTime, argonThreads,
		base64.RawStdEncoding.EncodeToString(salt),
		base64.RawStdEncoding.EncodeToString(key),
	)
}

// Verify reports whether rawToken hashes to encodedHash, in constant time. It parses
// the encoded cost and salt from the hash, recomputes the argon2id key over the
// presented token at that cost, and compares with crypto/subtle so the comparison
// leaks no timing signal. A malformed hash yields an error (never a silent false).
func Verify(rawToken, encodedHash string) (bool, error) {
	salt, key, err := decodeHash(encodedHash)
	if err != nil {
		return false, err
	}
	// Recompute at the engine's fixed key length: every hash this engine writes uses
	// argonKeyLen, and a stored key of any other length is safely rejected by the
	// constant-time compare (unequal lengths never match).
	got := argon2.IDKey([]byte(rawToken), salt, argonTime, argonMemory, argonThreads, argonKeyLen)
	return subtle.ConstantTimeCompare(got, key) == 1, nil
}

// decodeHash parses a PHC argon2id string into its salt and stored key, validating
// the algorithm and version. The cost fields are parsed for shape but the recompute
// uses the package constants (the defaults have not changed); a future cost change
// would read them from here.
func decodeHash(encoded string) (salt, key []byte, err error) {
	parts := strings.Split(encoded, "$")
	// ["", "argon2id", "v=19", "m=...,t=...,p=...", "<salt>", "<key>"]
	if len(parts) != 6 || parts[0] != "" || parts[1] != "argon2id" {
		return nil, nil, fmt.Errorf("pat: verify: malformed argon2id hash")
	}
	var version int
	if _, err := fmt.Sscanf(parts[2], "v=%d", &version); err != nil {
		return nil, nil, fmt.Errorf("pat: verify: bad argon2id version field: %w", err)
	}
	if version != argon2.Version {
		return nil, nil, fmt.Errorf("pat: verify: unsupported argon2id version %d", version)
	}
	var m, tCost uint32
	var p uint8
	if _, err := fmt.Sscanf(parts[3], "m=%d,t=%d,p=%d", &m, &tCost, &p); err != nil {
		return nil, nil, fmt.Errorf("pat: verify: bad argon2id parameter field: %w", err)
	}
	salt, err = base64.RawStdEncoding.DecodeString(parts[4])
	if err != nil {
		return nil, nil, fmt.Errorf("pat: verify: decode salt: %w", err)
	}
	key, err = base64.RawStdEncoding.DecodeString(parts[5])
	if err != nil {
		return nil, nil, fmt.Errorf("pat: verify: decode hash: %w", err)
	}
	if len(salt) == 0 || len(key) == 0 {
		return nil, nil, fmt.Errorf("pat: verify: empty salt or hash")
	}
	return salt, key, nil
}

// dataRolePrefix names the engine-managed read-only role of a data PAT, mirroring
// the pipeline login-role convention (iris_pipeline_<name>).
const dataRolePrefix = "iris_pat_"

// DataRoleName derives the cluster-unique Postgres role name for a data PAT from its
// token id: the engine-managed read-only NOLOGIN role the PAT owns in the access
// ledger (specification section 7). The id is a hex lookup key, so the derived name
// is a safe bare identifier.
func DataRoleName(tokenID string) string { return dataRolePrefix + tokenID }
