package store

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"fmt"
	"strings"
)

// This file is the engine-owned access-ledger write surface: the meta writes that
// register a Postgres role's owner, replace its field-level grants, and store its
// engine-managed login credential. Truth lives in meta and is reconciled onto the
// data database by internal/pg, which both renders the role/grant DDL and issues
// it (ProvisionPipelineRole, ProvisionDataPATRole). Every write rides the single
// meta writer.
//
// Three shapes, one ledger:
//   - roles maps a pg_role to exactly one owner, a pipeline XOR a data PAT. The
//     roles CHECK ((pipeline IS NULL) <> (pat IS NULL)) enforces the XOR at the
//     database; RegisterRole binds exactly one owner column so the write respects it.
//   - grants holds (pg_role, schema, table, field, access) rows, indexed on pg_role.
//     ReplaceGrants rewrites a role's grants as one atomic full-role rewrite.
//   - credentials holds an engine-managed secret per LOGIN role, pipeline roles only.
//     A data-PAT role is NOLOGIN (assumed via SET ROLE on the read path) and holds no
//     credential, so SetCredential rejects one loudly (ErrDataPATRoleNoCredential).

// GrantAccess is a grant's access kind (grants.access): read or write.
type GrantAccess string

// The grant access kinds. reads declare read access, writes declare write access,
// both recorded per field in the grants ledger.
const (
	// AccessRead is field-level read access (a declared reads entry).
	AccessRead GrantAccess = "read"
	// AccessWrite is field-level write access (a declared writes entry).
	AccessWrite GrantAccess = "write"
)

// Grant is one field-level access-ledger row for a role: the schema, table, field,
// and access kind the role holds. The pg_role it belongs to is supplied to
// ReplaceGrants, not carried per grant, so a whole role's grant set is one call.
type Grant struct {
	// Schema is the grant's schema.
	Schema string
	// Table is the grant's table.
	Table string
	// Field is the single column the grant covers (field-level: no all-columns).
	Field string
	// Access is the grant's access kind (read or write).
	Access GrantAccess
}

// RoleOwnerKind distinguishes a role's single owner: a pipeline or a data PAT.
type RoleOwnerKind int

// The role owner kinds.
const (
	// OwnerNone is the zero owner: neither a pipeline nor a data PAT. A role can
	// never be registered with it (the roles XOR CHECK forbids owning nothing).
	OwnerNone RoleOwnerKind = iota
	// OwnerPipeline is a pipeline-owned role: a login role with an injected
	// connection and an engine-managed credential.
	OwnerPipeline
	// OwnerDataPAT is a data-PAT-owned role: NOLOGIN, assumed via SET ROLE on the
	// read path, no credential.
	OwnerDataPAT
)

// RoleOwner identifies the single owner of an engine-managed Postgres role: a
// pipeline or a data PAT, exactly one. Build one with PipelineOwner or
// DataPATOwner; the zero value is invalid and RegisterRole rejects it.
type RoleOwner struct {
	// Kind is which owner this is.
	Kind RoleOwnerKind
	// Name is the owner's identity: a pipeline name (roles.pipeline) or a PAT id
	// (roles.pat).
	Name string
}

// PipelineOwner returns the owner for a pipeline's login role (roles.pipeline set).
func PipelineOwner(pipeline string) RoleOwner {
	return RoleOwner{Kind: OwnerPipeline, Name: pipeline}
}

// DataPATOwner returns the owner for a data PAT's NOLOGIN read role (roles.pat set).
func DataPATOwner(patID string) RoleOwner {
	return RoleOwner{Kind: OwnerDataPAT, Name: patID}
}

// IsLogin reports whether the owner's role is a login role: pipeline roles log in
// (and hold a credential); data-PAT roles are NOLOGIN (and hold none).
func (o RoleOwner) IsLogin() bool { return o.Kind == OwnerPipeline }

// ErrInvalidRoleOwner is returned by RegisterRole when the owner is neither a
// named pipeline nor a named data PAT: registering such a role would trip the
// roles pipeline-XOR-pat CHECK, so the write is refused before it reaches meta.
var ErrInvalidRoleOwner = errors.New("store: role owner must be exactly one of a named pipeline or a named data PAT")

// ErrDataPATRoleNoCredential is returned by SetCredential for a data-PAT role: it
// is NOLOGIN and holds no credential. Callers test it with errors.Is.
var ErrDataPATRoleNoCredential = errors.New("store: a data-PAT role is NOLOGIN and holds no credential; credentials hold pipeline login roles only")

// ErrEmptySecret is returned by SetCredential when the secret is the zero value: a
// login role's credential is a non-empty engine-managed secret, never blank.
var ErrEmptySecret = errors.New("store: credential secret is empty")

// The access-ledger write statements. Each is a single parameterized statement;
// ReplaceGrants groups its clearing delete and inserts into one atomic transaction.
const (
	// registerRoleSQL upserts a role's owner. A re-register overwrites the owner
	// columns, so a role is never left with a stale second owner; the roles CHECK
	// enforces that exactly one is non-null on every write.
	registerRoleSQL = `INSERT INTO roles (pg_role, pipeline, pat)
VALUES ($1, $2, $3)
ON CONFLICT (pg_role) DO UPDATE SET pipeline = EXCLUDED.pipeline, pat = EXCLUDED.pat`

	// deleteGrantsSQL clears a role's grant rows, the first half of the atomic
	// full-role grant rewrite.
	deleteGrantsSQL = `DELETE FROM grants WHERE pg_role = $1`

	// insertGrantSQL writes one field-level grant row. The reserved column names
	// schema and table are double-quoted.
	insertGrantSQL = `INSERT INTO grants (pg_role, "schema", "table", field, access) VALUES ($1, $2, $3, $4, $5)`

	// setCredentialSQL upserts a login role's engine-managed secret. A re-set
	// (credential rotation) overwrites the prior secret.
	setCredentialSQL = `INSERT INTO credentials (pg_role, secret)
VALUES ($1, $2)
ON CONFLICT (pg_role) DO UPDATE SET secret = EXCLUDED.secret`
)

// RegisterRole registers (or re-registers) a Postgres role's owner in the access
// ledger: it upserts the roles row with exactly one owner column set -- pipeline
// XOR pat. The owner is validated first, so an ownerless or dual-owner write
// never reaches meta to trip the roles CHECK. It is a single atomic statement and
// a leader-only meta write, riding the single Writer.
func (w *Writer) RegisterRole(ctx context.Context, pgRole string, owner RoleOwner) error {
	if pgRole == "" {
		return fmt.Errorf("store: writer register role: %w", ErrInvalidRoleOwner)
	}
	pipeline, pat, err := owner.columns()
	if err != nil {
		return fmt.Errorf("store: writer register role %q: %w", pgRole, err)
	}
	if err := w.conn.Exec(ctx, registerRoleSQL, pgRole, pipeline, pat); err != nil {
		return fmt.Errorf("store: writer register role %q: %w", pgRole, err)
	}
	return nil
}

// columns returns the (pipeline, pat) column bindings for the owner: exactly one is
// a non-nil string, the other SQL NULL, so the roles pipeline-XOR-pat CHECK holds.
// An ownerless or unnamed owner yields ErrInvalidRoleOwner.
func (o RoleOwner) columns() (pipeline, pat any, err error) {
	if o.Name == "" {
		return nil, nil, ErrInvalidRoleOwner
	}
	switch o.Kind {
	case OwnerPipeline:
		return o.Name, nil, nil
	case OwnerDataPAT:
		return nil, o.Name, nil
	default:
		return nil, nil, ErrInvalidRoleOwner
	}
}

// ReplaceGrants rewrites a role's field-level grants as one atomic full-role
// rewrite: it clears the role's existing grant rows and re-inserts the given set,
// one row per grant carrying (pg_role, schema, table, field, access), in a single
// meta transaction. An empty set clears the role's grants and writes no row. The
// whole batch commits together or not at all, so the ledger never reflects a
// partial grant set. It is a leader-only meta write, riding the single Writer.
func (w *Writer) ReplaceGrants(ctx context.Context, pgRole string, grants []Grant) error {
	if pgRole == "" {
		return fmt.Errorf("store: writer replace grants: empty pg_role")
	}
	stmts := []Statement{{SQL: deleteGrantsSQL, Args: []any{pgRole}}}
	for _, g := range grants {
		stmts = append(stmts, Statement{SQL: insertGrantSQL, Args: []any{pgRole, g.Schema, g.Table, g.Field, string(g.Access)}})
	}
	if err := w.execTx(ctx, stmts); err != nil {
		return fmt.Errorf("store: writer replace grants for %q: %w", pgRole, err)
	}
	return nil
}

// SetCredential stores the engine-managed secret for a pipeline LOGIN role,
// upserting the credentials row. It is guarded to login roles: a data-PAT owner
// is NOLOGIN and holds no credential, so it returns ErrDataPATRoleNoCredential
// and writes nothing. The secret must be non-empty. It is a single atomic
// statement and a leader-only meta write, riding the single Writer.
func (w *Writer) SetCredential(ctx context.Context, pgRole string, owner RoleOwner, secret Secret) error {
	if pgRole == "" {
		return fmt.Errorf("store: writer set credential: empty pg_role")
	}
	if !owner.IsLogin() {
		return fmt.Errorf("store: writer set credential for %q: %w", pgRole, ErrDataPATRoleNoCredential)
	}
	if secret.IsZero() {
		return fmt.Errorf("store: writer set credential for %q: %w", pgRole, ErrEmptySecret)
	}
	if err := w.conn.Exec(ctx, setCredentialSQL, pgRole, secret.reveal()); err != nil {
		return fmt.Errorf("store: writer set credential for %q: %w", pgRole, err)
	}
	return nil
}

// RenderSetRolePassword renders the credential-bearing DDL that sets a pipeline
// login role's password to the engine-minted secret: ALTER ROLE "role" WITH
// PASSWORD '<secret>'. It lives here -- beside Secret, the one place a
// credential's raw value is revealed -- so the secret never leaves store except
// through its deliberate exits; the live role provisioner (internal/pg) executes
// the returned statement in order but never constructs or logs it. The role name
// is a quoted identifier and the secret a quoted string literal, so neither can
// break the statement. The secret is engine-minted (base64url, no
// metacharacters), but it is quoted defensively regardless.
func RenderSetRolePassword(role string, secret Secret) string {
	// The role is engine-derived and quoted as an identifier; the secret is an
	// engine-minted base64url credential quoted as a SQL string literal (single quotes
	// doubled). Postgres utility statements (ALTER ROLE) take no bind parameters, so the
	// credential must be a quoted literal, not a placeholder.
	return fmt.Sprintf("ALTER ROLE %s WITH PASSWORD %s;", quoteRoleIdentifier(role), quoteStringLiteral(secret.reveal())) //nolint:gosec // credential is engine-minted and quoted as a literal; no injection vector.
}

// quoteRoleIdentifier double-quotes a Postgres role identifier and escapes any embedded
// double quote by doubling it (the SQL standard escape), so a role name is always a
// safe, unambiguous identifier in the DDL store renders.
func quoteRoleIdentifier(name string) string {
	return `"` + strings.ReplaceAll(name, `"`, `""`) + `"`
}

// quoteStringLiteral renders s as a Postgres string literal, doubling every embedded
// single quote (the standard escape; standard_conforming_strings is on by default, so
// backslashes are literal). It is used to bind the engine-minted credential into an
// ALTER ROLE statement, which takes no parameters.
func quoteStringLiteral(s string) string {
	return `'` + strings.ReplaceAll(s, `'`, `''`) + `'`
}

// redactedSecret is what every formatting path renders in place of a credential
// secret, so a stray %v, %s, %#v, or String() in a log line can never leak it (the
// AdminDSN redaction pattern).
const redactedSecret = "Secret(REDACTED)"

// secretBytes is the entropy of a generated credential secret: 32 random bytes,
// base64-url encoded to a URL-safe printable string.
const secretBytes = 32

// Secret is an engine-managed credential secret held only for a pipeline login
// role. Its raw value never leaves the process through a formatting or encoding
// path: it has one unexported field, implements fmt.Formatter, fmt.Stringer, and
// fmt.GoStringer to redact, and exposes the raw string only to the credentials
// write bind (reveal, within this package). It is engine-managed: authors and
// consumers never handle it.
type Secret struct {
	// value is the raw secret. Unexported so no reflection-based encoder
	// (encoding/json, etc.) can serialize it, keeping the secret memory-only until
	// the deliberate credentials write bind.
	value string
}

// GenerateSecret mints a fresh engine-managed credential secret from crypto/rand:
// 32 bytes of entropy, base64-url encoded. It is the sole construction path for a
// new pipeline-role credential, so a secret is never author-supplied.
func GenerateSecret() (Secret, error) {
	buf := make([]byte, secretBytes)
	if _, err := rand.Read(buf); err != nil {
		return Secret{}, fmt.Errorf("store: generate credential secret: %w", err)
	}
	return Secret{value: base64.RawURLEncoding.EncodeToString(buf)}, nil
}

// IsZero reports whether the secret is the zero value (no secret held).
func (s Secret) IsZero() bool { return s.value == "" }

// reveal returns the raw secret. It is unexported: only this package reads the raw
// value, and only to bind it into the credentials write (SetCredential).
func (s Secret) reveal() string { return s.value }

// Format implements fmt.Formatter, which fmt consults before every other
// formatting path -- String, GoString, and the struct reflection a numeric verb
// (%d, %o, %b) would otherwise fall through to and print the unexported field. It
// writes the redacted marker for every verb, so no verb can render the raw secret.
func (Secret) Format(f fmt.State, _ rune) { _, _ = f.Write([]byte(redactedSecret)) }

// String implements fmt.Stringer, redacting the secret for direct String() callers.
func (Secret) String() string { return redactedSecret }

// GoString implements fmt.GoStringer, redacting the secret for direct GoString()
// callers.
func (Secret) GoString() string { return redactedSecret }
