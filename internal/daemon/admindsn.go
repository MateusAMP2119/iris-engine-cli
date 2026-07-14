// Package daemon owns the engine's lifecycle state that lives only in the running
// process: foremost the admin DSN, the one Postgres credential every engine
// connection derives from. It sits above store and pg in the import graph: the
// daemon holds the admin DSN and hands store (meta) and pg (data) a derived
// connection source, so neither database client ever sees a raw, user-supplied
// connection string.
//
// # The admin DSN chain
//
// One daemon-owned admin DSN is resolved at startup with the strict precedence
// --pg-dsn > IRIS_PG_DSN > iris.toml pg_dsn and no default. The precedence itself
// is the config package's (the same pg_dsn key and IRIS_PG_DSN it already
// resolves); Resolve layers the admin-DSN-specific semantics on top of the resolved
// config.Settings: fail fast with no default, hold the DSN only in memory, redact
// it from every formatting path, and derive every Postgres connection from it.
//
// # Managed vs external, reconciled
//
// Two things must be read together: the admin DSN chain has "no default, fail
// fast", yet the zero-config default is an engine-managed Postgres the engine mints
// its own superuser for. They reconcile at the call site, not in Resolve. Resolve
// reports ErrNoAdminDSN when no layer set a DSN; the caller decides what that
// means:
//
//   - `iris engine start` treats ErrNoAdminDSN as "managed mode": it mints and
//     dials its own managed instance, no external DSN required.
//   - a caller that requires an external DSN (external-mode startup, or a
//     daemonless lifecycle command targeting external Postgres) fails fast,
//     surfacing ErrNoAdminDSN's guidance.
//
// So "no default, fail fast" is the admin-DSN resolution's contract wherever an
// external admin DSN is required; managed mode is the answer when none is set and
// the engine may run its own Postgres. Resolve is the single resolution point;
// the branch is the caller's.
package daemon

import (
	"context"
	"errors"
	"fmt"

	"github.com/MateusAMP2119/iris-lakehouse/internal/config"
	"github.com/MateusAMP2119/iris-lakehouse/internal/pg"
	"github.com/MateusAMP2119/iris-lakehouse/internal/store"
)

// redacted is what every formatting path renders in place of the admin DSN, so a
// stray %v, %s, %#v, or String() in a log line can never leak the credential.
const redacted = "AdminDSN(REDACTED)"

// ErrNoAdminDSN is returned by Resolve when no configuration layer set an admin
// DSN: --pg-dsn, IRIS_PG_DSN, and iris.toml pg_dsn are all empty. There is no
// default. A caller requiring an external DSN fails fast on it; `iris engine start`
// treats it as the signal to run managed Postgres.
var ErrNoAdminDSN = errors.New("daemon: no admin DSN configured; set --pg-dsn, IRIS_PG_DSN, or pg_dsn in iris.toml, or run the engine-managed Postgres")

// AdminDSN is the one daemon-owned Postgres admin connection string, held only in
// memory. Its raw value never leaves the process through a formatting or encoding
// path: it has one unexported field, implements fmt.Stringer and fmt.GoStringer
// to redact, and exposes the raw string only to the connection layer, and only
// through a derived ConnectionSource (Source). Every Postgres connection the
// engine opens derives from it.
type AdminDSN struct {
	// conn is the raw admin connection string. Unexported so no reflection-based
	// encoder (encoding/json, etc.) can serialize it, keeping the DSN memory-only.
	conn string
}

// Resolve resolves the admin DSN from the already-resolved engine configuration.
// cfg.PgDSN carries the value of the documented chain (--pg-dsn > IRIS_PG_DSN >
// iris.toml pg_dsn, resolved by the config package). Resolve applies the
// admin-DSN-specific fail-fast: an empty DSN yields ErrNoAdminDSN with no default,
// and a non-empty one is wrapped as the memory-held AdminDSN. See the package doc
// for how a caller reconciles ErrNoAdminDSN with managed mode.
func Resolve(cfg config.Settings) (AdminDSN, error) {
	if cfg.PgDSN == "" {
		return AdminDSN{}, ErrNoAdminDSN
	}
	return AdminDSN{conn: cfg.PgDSN}, nil
}

// Format implements fmt.Formatter, which fmt consults before every other
// formatting path -- String, GoString, and, crucially, the struct reflection a
// numeric verb (%d, %o, %b) would otherwise fall through to and print the
// unexported field verbatim. It writes the redacted marker for every verb, so no
// verb can render the raw DSN. String and GoString remain for direct, non-fmt
// callers.
func (AdminDSN) Format(f fmt.State, _ rune) { _, _ = f.Write([]byte(redacted)) }

// String implements fmt.Stringer, redacting the DSN for direct String() callers.
func (AdminDSN) String() string { return redacted }

// GoString implements fmt.GoStringer, redacting the DSN for direct GoString()
// callers.
func (AdminDSN) GoString() string { return redacted }

// connString returns the raw admin connection string. It is unexported: only this
// package reads the raw DSN, and only to build a derived ConnectionSource for the
// connection layer.
func (a AdminDSN) connString() string { return a.conn }

// Source derives a ConnectionSource from the admin DSN: the single point the raw
// DSN crosses from the daemon to the store/pg connection layer. It is the only
// way to obtain a ConnectionSource that carries a connection string, so every
// connection opened from a Source derives from this one admin DSN.
func (a AdminDSN) Source() ConnectionSource { return ConnectionSource{conn: a.connString()} }

// ConnectionSource is the admin-derived connection string the store and pg
// connection seams accept. Its field is unexported, so the only ConnectionSource
// carrying a DSN is one the daemon derived from the admin DSN (AdminDSN.Source);
// a zero value is inert. It also redacts under formatting, so passing it through
// the connection layer never risks a leaked DSN in a log line.
type ConnectionSource struct {
	conn string
}

// ConnString returns the admin-derived connection string, satisfying the
// store.ConnSource and pg.ConnSource seams. This is the deliberate, explicit exit
// the connection layer uses; no formatting or encoding path reaches it.
func (c ConnectionSource) ConnString() string { return c.conn }

// Format implements fmt.Formatter, redacting the connection string under every
// verb (fmt consults it before String, GoString, or struct reflection), so
// passing a source through the connection layer never risks a leaked DSN in a log
// line regardless of the verb used.
func (ConnectionSource) Format(f fmt.State, _ rune) { _, _ = f.Write([]byte(redacted)) }

// String implements fmt.Stringer, redacting the connection string for direct
// String() callers.
func (ConnectionSource) String() string { return redacted }

// GoString implements fmt.GoStringer, redacting the connection string for direct
// GoString() callers.
func (ConnectionSource) GoString() string { return redacted }

// Connect opens the engine's database connections — meta through the store seam,
// data through the pg seam — each derived from the single admin DSN: both
// connections take their string from the admin DSN's derived source, so no engine
// connection originates from any other string. The dialers are injected, so the
// derivation is provable with recording fakes and no live Postgres. The daemon's
// live startup path opens the same two connections through the pgx-backed clients
// (store.Connect, pg.Connect), which take the very same derived source.
func (a AdminDSN) Connect(ctx context.Context, meta store.Dialer, data pg.Dialer) error {
	src := a.Source()
	if err := store.Open(ctx, src, meta); err != nil {
		return fmt.Errorf("daemon: open meta connection: %w", err)
	}
	if err := pg.Open(ctx, src, data); err != nil {
		return fmt.Errorf("daemon: open data connection: %w", err)
	}
	return nil
}
