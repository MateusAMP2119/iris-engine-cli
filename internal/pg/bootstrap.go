package pg

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5/pgconn"
)

// This file bootstraps the dedicated data database, mirroring how store bootstraps
// meta (store/bootstrap.go, store.Client.ensureMetaDatabase). The engine runs on one
// cluster behind one admin DSN (specification section 2), and it owns two databases
// in that cluster: the meta control database and the data database, where the declared
// schemas and the public.data_journal live (specification section 4).
//
// Why a dedicated, engine-created data database rather than the admin DSN's own
// database: in external mode the DSN typically points at a database the cluster
// superuser owns (e.g. the default `postgres`), while the engine authenticates as a
// non-superuser admin role carrying only CREATEDB/CREATEROLE. That admin cannot create
// schemas in a database it does not own, so provisioning would be denied
// ("permission denied for database postgres"). The engine therefore creates its own
// data database with a plain CREATE DATABASE (the admin's CREATEDB right makes the
// admin its owner), exactly as it creates meta, and points the data pool at it. This
// is the surface the spec names "the data database"; managed mode uses the same name,
// so the two modes are one code path.

// DataDatabase is the fixed name of the dedicated data database in the cluster: where
// the declared schemas and the data journal live, distinct from the meta control
// database (store.MetaDatabase). The engine creates it admin-owned so the admin role
// can provision schemas into it.
const DataDatabase = "data"

// duplicateDatabaseCode is Postgres' SQLSTATE for duplicate_database: a CREATE
// DATABASE that lost a race to another candidate creating the same database first.
const duplicateDatabaseCode = "42P04"

// CreateDataDatabaseDDL is the statement that creates the dedicated data database: a
// plain CREATE DATABASE (hence the admin role needs CREATEDB, and becomes the owner).
// Postgres has no IF NOT EXISTS for CREATE DATABASE; the create-if-missing guard is
// applied by the caller against pg_database before issuing it.
func CreateDataDatabaseDDL() string {
	return "CREATE DATABASE " + DataDatabase + ";"
}

// DataExistsQuery is the catalog probe answering the create-if-missing guard: whether
// the dedicated data database already exists, so the caller issues CreateDataDatabaseDDL
// only when it does not. It runs on the admin/maintenance connection, never on data.
// The datname is a bound $1 argument (DataDatabase), parameterized like every other
// catalog query in the package rather than concatenated into the SQL text.
const DataExistsQuery = "SELECT 1 FROM pg_database WHERE datname = $1"

// DropDataDatabaseDDL is the statement that drops the dedicated data database in full:
// the engine-uninstall teardown of the data plane (specification sections 4 and 12),
// which takes the journal and every provisioned schema with it. Like CREATE DATABASE
// it runs on the admin/maintenance connection, never on a connection to data. IF
// EXISTS keeps the teardown idempotent when data was already dropped or never created.
func DropDataDatabaseDDL() string {
	return "DROP DATABASE IF EXISTS " + DataDatabase + ";"
}

// ensureDataDatabaseOn runs the create-if-missing logic over injected probe and create
// seams, so its race handling is provable with no live Postgres (mirroring store's
// ensureMetaDatabaseOn). It probes for data and, if absent, creates it -- tolerating a
// concurrent create: another candidate creating data between the probe and the CREATE
// makes CREATE DATABASE fail with duplicate_database (42P04), which is not an error but
// the goal already met (the database exists), so it is treated as success.
func ensureDataDatabaseOn(ctx context.Context, exists func(context.Context) (bool, error), create func(context.Context) error) error {
	present, err := exists(ctx)
	if err != nil {
		return fmt.Errorf("pg: probe data database: %w", err)
	}
	if present {
		return nil
	}
	if err := create(ctx); err != nil {
		if isDuplicateDatabase(err) {
			return nil
		}
		return fmt.Errorf("pg: create data database: %w", err)
	}
	return nil
}

// isDuplicateDatabase reports whether err is Postgres' duplicate_database (42P04): a
// CREATE DATABASE lost the race to a concurrent candidate that created data first.
func isDuplicateDatabase(err error) bool {
	var pgErr *pgconn.PgError
	return errors.As(err, &pgErr) && pgErr.Code == duplicateDatabaseCode
}
