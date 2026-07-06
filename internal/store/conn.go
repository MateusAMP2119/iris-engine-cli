package store

import (
	"context"
	"errors"
)

// This file holds the meta connection-opening seam. Every meta connection derives
// from the single daemon-owned admin DSN (specification section 2): store never
// accepts a raw connection string, only a ConnSource the daemon builds from the
// admin DSN, so no meta connection can originate from anywhere else. The real
// pgx-backed Dialer lands in E02.3; a recording fake stands in until then.

// ConnSource yields the connection string store dials for the meta database. The
// daemon builds the sole production source from its admin DSN, so a ConnSource is
// the type-level guarantee that a meta connection derives from that one DSN;
// store exposes no entry point that takes a raw connection string.
type ConnSource interface {
	// ConnString returns the admin-derived connection string for the meta database.
	ConnString() string
}

// Dialer opens a live connection to a meta database connection string. It is the
// one place store turns a connection string into a connection; the pgx-backed
// dialer arrives in E02.3 and a recording fake drives it in tests until then.
type Dialer interface {
	// Dial opens a connection to connString.
	Dial(ctx context.Context, connString string) error
}

// Open dials the meta database connection derived from src through dialer. src is
// the admin-derived source the daemon built; store issues the connection from
// src.ConnString() and from no other string, so every meta connection derives
// from the single daemon-owned admin DSN.
func Open(ctx context.Context, src ConnSource, dialer Dialer) error {
	if src == nil {
		return errors.New("store: nil connection source")
	}
	if dialer == nil {
		return errors.New("store: nil dialer")
	}
	return dialer.Dial(ctx, src.ConnString())
}
