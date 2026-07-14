package pg

import (
	"context"
	"errors"
)

// This file holds the data connection-opening seam. Every data connection derives
// from the single daemon-owned admin DSN: pg never accepts a raw connection
// string, only a ConnSource the daemon builds from the admin DSN, so no data
// connection can originate from anywhere else. Nothing in production implements
// Dialer -- the live pgx client (Connect, live.go) is what actually opens the data
// database, from a ConnSource like everything else. Dialer exists so the
// derivation itself is provable: the daemon drives Open with a recording fake and
// no live Postgres, and the fake sees the admin-derived string and no other.

// ConnSource yields the connection string pg dials for the data database. The
// daemon builds the sole production source from its admin DSN, so a ConnSource is
// the type-level guarantee that a data connection derives from that one DSN; pg
// exposes no entry point that takes a raw connection string.
type ConnSource interface {
	// ConnString returns the admin-derived connection string for the data database.
	ConnString() string
}

// Dialer opens a live connection to a data database connection string. Only tests
// implement it -- a recording fake that captures the string it is handed rather
// than dialing it -- so the derivation from the admin DSN can be proved with no
// live Postgres; the pgx-backed Connect (live.go) is the production path onto the
// data database.
type Dialer interface {
	// Dial opens a connection to connString.
	Dial(ctx context.Context, connString string) error
}

// Open dials the data database connection derived from src through dialer. src is
// the admin-derived source the daemon built; pg issues the connection from
// src.ConnString() and from no other string, so every data connection derives
// from the single daemon-owned admin DSN.
func Open(ctx context.Context, src ConnSource, dialer Dialer) error {
	if src == nil {
		return errors.New("pg: nil connection source")
	}
	if dialer == nil {
		return errors.New("pg: nil dialer")
	}
	return dialer.Dial(ctx, src.ConnString())
}
