package pg

import (
	"context"
	"errors"
	"testing"

	"github.com/jackc/pgx/v5/pgconn"
)

// TestEnsureDataDatabaseRaceTolerant proves the data-database create-if-missing path
// tolerates a concurrent creator, mirroring the meta path (store.ensureMetaDatabaseOn):
// when the probe finds data absent but the CREATE DATABASE then loses a race (Postgres
// duplicate_database, 42P04), the outcome is success -- the database exists, which is
// the goal -- rather than a fatal error; an already-present database issues no create;
// and a non-42P04 create error still propagates. This is the pure core of the
// external-mode fix that lets apply (and its provisioning) run against an admin-owned
// data database, proven with injected seams and no live Postgres.
func TestEnsureDataDatabaseRaceTolerant(t *testing.T) {
	t.Run("apply-repeat-noop", func(t *testing.T) {
		absent := func(context.Context) (bool, error) { return false, nil }
		present := func(context.Context) (bool, error) { return true, nil }

		t.Run("a concurrent create (42P04 duplicate_database) is treated as success", func(t *testing.T) {
			dup := &pgconn.PgError{Code: duplicateDatabaseCode, Message: `database "data" already exists`}
			created := false
			create := func(context.Context) error { created = true; return dup }
			if err := ensureDataDatabaseOn(context.Background(), absent, create); err != nil {
				t.Fatalf("ensureDataDatabaseOn with a 42P04 loser returned %v, want nil (database exists)", err)
			}
			if !created {
				t.Error("the create was not attempted for an absent data database")
			}
		})

		t.Run("an already-present data database issues no create", func(t *testing.T) {
			create := func(context.Context) error {
				t.Error("create was issued for an already-present data database")
				return nil
			}
			if err := ensureDataDatabaseOn(context.Background(), present, create); err != nil {
				t.Errorf("ensureDataDatabaseOn with data present = %v, want nil", err)
			}
		})

		t.Run("a non-42P04 create error propagates", func(t *testing.T) {
			boom := errors.New("disk full")
			create := func(context.Context) error { return boom }
			if err := ensureDataDatabaseOn(context.Background(), absent, create); !errors.Is(err, boom) {
				t.Errorf("ensureDataDatabaseOn create error = %v, want it to wrap %v", err, boom)
			}
		})

		t.Run("a probe error propagates", func(t *testing.T) {
			boom := errors.New("catalog unreachable")
			probe := func(context.Context) (bool, error) { return false, boom }
			create := func(context.Context) error {
				t.Error("create was issued after a probe error")
				return nil
			}
			if err := ensureDataDatabaseOn(context.Background(), probe, create); !errors.Is(err, boom) {
				t.Errorf("ensureDataDatabaseOn probe error = %v, want it to wrap %v", err, boom)
			}
		})
	})
}
