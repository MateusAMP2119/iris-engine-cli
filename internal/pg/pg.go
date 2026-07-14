// Package pg is the data database client seam: the only code that talks to the
// data database. It owns connection injection, DDL and grant reconcile, drift, and
// the journal and its triggers; two clients, two databases, never crossed (store
// owns meta, pg owns data).
//
// This is the minimal, behavior-focused seam the later epics extend: E03/E04
// land the real pgx-backed implementation that renders and applies the CREATE /
// ALTER / GRANT and trigger DDL, plus connection injection and drift. It is
// deliberately small today -- a single statement-issuing method -- so a recording
// fake (internal/pg/pgtest) can capture the exact DDL a test issues, and that
// captured DDL is diffed byte-for-byte against golden files with no live Postgres.
// A golden diff is a contract diff.
package pg

import "context"

// DB is the data database client seam. Exec issues one DDL or grant statement
// against the data database; a real pgx-backed implementation and a recording
// fake both satisfy it. The interface will grow query, connection-injection, and
// transaction methods in later epics.
type DB interface {
	// Exec issues one SQL statement (a CREATE / ALTER / GRANT or trigger DDL)
	// against the data database.
	Exec(ctx context.Context, sql string) error
}
