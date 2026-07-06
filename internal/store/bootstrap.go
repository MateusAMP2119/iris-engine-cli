package store

import (
	"context"
	"fmt"
)

// This file holds the embedded meta-DDL bootstrap entry points. All engine state
// lives in Postgres behind the single --pg-dsn (specification section 2): the
// embedded DDL is applied create-if-missing at install and re-checked at each
// leader election, with no self-migration ledger, no version gate, and no local
// state store (never SQLite, never .iris/state.db).

// Execer issues one SQL statement against a database. Bootstrap and EnsureSchema
// issue the embedded meta DDL through it; a real Postgres connection and a
// recording fake (internal/store/storetest.Recorder) both satisfy it, so the
// emitted DDL is asserted with no live Postgres.
type Execer interface {
	// Exec issues one SQL statement (a CREATE DATABASE / CREATE TABLE / CREATE
	// INDEX) against the target database.
	Exec(ctx context.Context, sql string) error
}

// CreateMetaDatabaseDDL is the statement that creates the dedicated meta database
// in the cluster: a plain CREATE DATABASE (hence the admin role needs CREATEDB).
// Postgres has no IF NOT EXISTS for CREATE DATABASE; the create-if-missing guard
// is applied by the caller against pg_database before issuing it.
func CreateMetaDatabaseDDL() string {
	return "CREATE DATABASE " + MetaDatabase + ";"
}

// MetaExistsQuery is the catalog probe that answers the create-if-missing guard:
// it reports whether the dedicated meta database already exists in the cluster, so
// bootstrap issues CreateMetaDatabaseDDL only when it does not (CREATE DATABASE has
// no IF NOT EXISTS). It runs on the admin/maintenance connection, never on meta.
const MetaExistsQuery = "SELECT 1 FROM pg_database WHERE datname = '" + MetaDatabase + "'"

// DropMetaDatabaseDDL is the statement that drops the dedicated meta database in
// full: the engine uninstall teardown (specification section 4). Dropping the
// database takes every control table -- and so every captured provenance row,
// endpoint, and access ledger entry -- with it. Like CREATE DATABASE it runs on
// the admin/maintenance connection, never on a connection to meta itself. IF
// EXISTS makes the teardown idempotent when meta was already dropped or never
// created.
func DropMetaDatabaseDDL() string {
	return "DROP DATABASE IF EXISTS " + MetaDatabase + ";"
}

// Bootstrap creates the dedicated meta database through the cluster connection,
// then ensures its schema through the meta connection: the one-time engine
// install path (specification section 4, "iris engine install"). cluster runs the
// CREATE DATABASE against the admin/maintenance database; meta runs the table DDL
// against the freshly-created meta database. In tests a single recorder can stand
// in for both to capture the whole bootstrap inventory in order.
func Bootstrap(ctx context.Context, cluster, meta Execer) error {
	if err := cluster.Exec(ctx, CreateMetaDatabaseDDL()); err != nil {
		return fmt.Errorf("store: create meta database: %w", err)
	}
	if err := EnsureSchema(ctx, meta); err != nil {
		return fmt.Errorf("store: ensure meta schema: %w", err)
	}
	return nil
}

// EnsureSchema issues the embedded meta DDL create-if-missing through exec: one
// CREATE TABLE IF NOT EXISTS per control table plus its secondary indexes, in
// roster order. It is applied at bootstrap and re-checked at each leader election
// (specification section 4); the statements are idempotent, so a re-check emits
// the identical sequence and touches nothing that already exists. There is no
// ALTER, no DROP, no self-migration ledger, and no version gate: meta is engine
// memory, not migration-managed.
func EnsureSchema(ctx context.Context, exec Execer) error {
	for _, stmt := range MetaSchema().DDL() {
		if err := exec.Exec(ctx, stmt); err != nil {
			return fmt.Errorf("store: apply meta DDL: %w", err)
		}
	}
	return nil
}
