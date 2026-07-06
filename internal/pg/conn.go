package pg

import (
	"context"
	"errors"
)

// This file holds the data connection-opening seam. Every data connection derives
// from the single daemon-owned admin DSN (specification section 2): pg never
// accepts a raw connection string, only a ConnSource the daemon builds from the
// admin DSN, so no data connection can originate from anywhere else. The real
// pgx-backed Dialer lands in E02.3; a recording fake stands in until then.

// ConnSource yields the connection string pg dials for the data database. The
// daemon builds the sole production source from its admin DSN, so a ConnSource is
// the type-level guarantee that a data connection derives from that one DSN; pg
// exposes no entry point that takes a raw connection string.
type ConnSource interface {
	// ConnString returns the admin-derived connection string for the data database.
	ConnString() string
}

// Dialer opens a live connection to a data database connection string. It is the
// one place pg turns a connection string into a connection; the pgx-backed dialer
// arrives in E02.3 and a recording fake drives it in tests until then.
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
