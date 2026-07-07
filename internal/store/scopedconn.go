package store

import (
	"fmt"
	"net"
	"net/url"
	"strconv"
)

// This file is the pipeline-scoped connection the engine injects into a run at spawn
// (specification section 7): the least-privilege Postgres connection string that
// authenticates as a pipeline's engine-managed role, with the engine-minted
// credential, and targets the data database. It is built here -- beside Secret, the
// one place a credential's raw value is revealed -- so the secret never leaves store
// except through the two deliberate exits it already has (the credentials write bind)
// plus this one (the run-environment injection). Authors and consumers never handle
// it; the engine mints, holds, and injects it (specification section 7).

// redactedScopedConn is what every formatting path renders in place of a scoped
// connection string, so a stray %v, %s, %#v, or String() in a log line can never leak
// the credential-bearing DSN (the Secret / AdminDSN redaction pattern).
const redactedScopedConn = "ScopedConn(REDACTED)"

// ScopedConnParams are the data-database connection coordinates a scoped connection is
// assembled from: everything but the identity. The engine derives them from the one
// admin DSN (host and port) plus the fixed data-database name; the per-run identity
// (the pipeline role and its engine-minted secret) is supplied to BuildScopedConn.
type ScopedConnParams struct {
	// Host is the data cluster host (from the admin DSN).
	Host string
	// Port is the data cluster port (from the admin DSN).
	Port int
	// Database is the data database the run connects to (pg.DataDatabase), never the
	// meta control database.
	Database string
	// Options is the raw connection query string (e.g. "sslmode=disable"), without a
	// leading '?'. Empty for no options.
	Options string
}

// ScopedConn is the least-privilege Postgres connection string the engine injects into
// a pipeline run at spawn: it authenticates as the pipeline's engine-managed role with
// the engine-minted credential and targets the data database (specification section
// 7). Its raw value never leaves the process through a formatting or encoding path: it
// has one unexported field, implements fmt.Formatter, fmt.Stringer, and fmt.GoStringer
// to redact, and exposes the raw DSN only through EnvValue -- the deliberate exit that
// injects it into the run's IRIS_DB_URL. Authors and consumers never handle it.
type ScopedConn struct {
	// dsn is the raw scoped connection string. Unexported so no reflection-based
	// encoder (encoding/json, etc.) can serialize it, keeping the credential-bearing
	// DSN memory-only until the deliberate run-environment injection.
	dsn string
}

// BuildScopedConn assembles the scoped connection for a pipeline's login role from the
// data-database coordinates, the role name, and the engine-minted credential
// (specification section 7). The role must be non-empty and the secret non-zero (a
// login role always carries a credential); an empty role yields ErrInvalidRoleOwner
// and a zero secret yields ErrEmptySecret. The userinfo is URL-encoded, so a
// credential containing a URL metacharacter still yields a valid DSN, and the raw
// secret is read exactly once here (through the package-private reveal) and sealed
// inside the returned ScopedConn.
func BuildScopedConn(params ScopedConnParams, role string, secret Secret) (ScopedConn, error) {
	if role == "" {
		return ScopedConn{}, fmt.Errorf("store: build scoped connection: %w", ErrInvalidRoleOwner)
	}
	if secret.IsZero() {
		return ScopedConn{}, fmt.Errorf("store: build scoped connection for %q: %w", role, ErrEmptySecret)
	}
	u := url.URL{
		Scheme:   "postgres",
		User:     url.UserPassword(role, secret.reveal()),
		Host:     net.JoinHostPort(params.Host, strconv.Itoa(params.Port)),
		Path:     "/" + params.Database,
		RawQuery: params.Options,
	}
	return ScopedConn{dsn: u.String()}, nil
}

// EnvValue returns the raw scoped connection string. It is the deliberate exit -- the
// sole way the credential-bearing DSN leaves store -- used only to inject the
// connection into a run's IRIS_DB_URL environment variable at spawn. No formatting or
// encoding path reaches it.
func (s ScopedConn) EnvValue() string { return s.dsn }

// IsZero reports whether the scoped connection is the zero value (no connection held).
func (s ScopedConn) IsZero() bool { return s.dsn == "" }

// Format implements fmt.Formatter, which fmt consults before every other formatting
// path -- String, GoString, and the struct reflection a numeric verb (%d, %o, %b)
// would otherwise fall through to and print the unexported field. It writes the
// redacted marker for every verb, so no verb can render the raw DSN.
func (ScopedConn) Format(f fmt.State, _ rune) { _, _ = f.Write([]byte(redactedScopedConn)) }

// String implements fmt.Stringer, redacting the DSN for direct String() callers.
func (ScopedConn) String() string { return redactedScopedConn }

// GoString implements fmt.GoStringer, redacting the DSN for direct GoString() callers.
func (ScopedConn) GoString() string { return redactedScopedConn }
