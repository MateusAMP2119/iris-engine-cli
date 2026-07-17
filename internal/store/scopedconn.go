package store

import (
	"fmt"
	"net"
	"net/url"
	"strconv"
	"strings"
)

// Least-privilege scoped DSN over an engine-managed role, built beside Secret so the raw credential never leaves store outside its deliberate exits.

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

// ScopedConn is a least-privilege data-database DSN for an engine-managed role; the read pool builds one per data-surface login, runs never receive one (#206). Redacted by every fmt path; EnvValue is the sole raw exit.
type ScopedConn struct {
	// dsn is the raw scoped connection string. Unexported so no reflection-based
	// encoder (encoding/json, etc.) can serialize it, keeping the credential-bearing
	// DSN memory-only until the deliberate run-environment injection.
	dsn string
}

// ScopedParamsFromDSN derives the scoped-connection parameters from a
// postgres:// DSN: the host, port, database, and raw options of the connection
// the params will re-target with a different identity. The DSN's own user and
// password are deliberately dropped -- the whole point of the scoped connection
// is that the run authenticates as its own least-privilege role, never as the
// DSN's (admin) identity.
func ScopedParamsFromDSN(dsn string) (ScopedConnParams, error) {
	u, err := url.Parse(dsn)
	if err != nil {
		return ScopedConnParams{}, fmt.Errorf("store: parse DSN for scoped params: %w", err)
	}
	port := 5432
	if p := u.Port(); p != "" {
		if port, err = strconv.Atoi(p); err != nil {
			return ScopedConnParams{}, fmt.Errorf("store: parse DSN port for scoped params: %w", err)
		}
	}
	return ScopedConnParams{
		Host:     u.Hostname(),
		Port:     port,
		Database: strings.TrimPrefix(u.Path, "/"),
		Options:  u.RawQuery,
	}, nil
}

// BuildScopedConn assembles the scoped connection for a pipeline's login role
// from the data-database coordinates, the role name, and the engine-minted
// credential. The role must be non-empty and the secret non-zero (a login role
// always carries a credential); an empty role yields ErrInvalidRoleOwner and a
// zero secret yields ErrEmptySecret. The userinfo is URL-encoded, so a credential
// containing a URL metacharacter still yields a valid DSN, and the raw secret is
// read exactly once here (through the package-private reveal) and sealed inside
// the returned ScopedConn.
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

// EnvValue returns the raw DSN: the sole exit, consumed by the read pool.
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
