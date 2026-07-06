package store

import (
	"context"
	"errors"
	"testing"

	"github.com/jackc/pgx/v5/pgconn"
)

// TestEnsureMetaDatabaseRaceTolerant proves the meta-database create-if-missing path
// tolerates a concurrent creator: when the probe finds meta absent but the CREATE
// DATABASE then loses a race (Postgres duplicate_database, 42P04), the outcome is
// success -- the database exists, which is the goal -- rather than a fatal error. A
// non-42P04 create error still propagates.
//
// spec: S02/one-leader-sole-dispatcher
func TestEnsureMetaDatabaseRaceTolerant(t *testing.T) {
	t.Run("S02/one-leader-sole-dispatcher", func(t *testing.T) {
		absent := func(context.Context) (bool, error) { return false, nil }
		present := func(context.Context) (bool, error) { return true, nil }

		t.Run("a concurrent create (42P04 duplicate_database) is treated as success", func(t *testing.T) {
			dup := &pgconn.PgError{Code: duplicateDatabaseCode, Message: `database "meta" already exists`}
			created := false
			create := func(context.Context) error { created = true; return dup }

			if err := ensureMetaDatabaseOn(context.Background(), absent, create); err != nil {
				t.Fatalf("ensureMetaDatabaseOn with a 42P04 loser returned %v, want nil (database exists)", err)
			}
			if !created {
				t.Error("create seam was not attempted")
			}
		})

		t.Run("an existing database skips the create entirely", func(t *testing.T) {
			create := func(context.Context) error { t.Fatal("create attempted though meta already exists"); return nil }
			if err := ensureMetaDatabaseOn(context.Background(), present, create); err != nil {
				t.Errorf("ensureMetaDatabaseOn with meta present = %v, want nil", err)
			}
		})

		t.Run("a non-42P04 create error propagates", func(t *testing.T) {
			boom := errors.New("permission denied")
			create := func(context.Context) error { return boom }
			if err := ensureMetaDatabaseOn(context.Background(), absent, create); !errors.Is(err, boom) {
				t.Errorf("ensureMetaDatabaseOn create error = %v, want it to wrap %v", err, boom)
			}
		})

		t.Run("a different SQLSTATE is not mistaken for duplicate_database", func(t *testing.T) {
			other := &pgconn.PgError{Code: "42501", Message: "permission denied"}
			create := func(context.Context) error { return other }
			if err := ensureMetaDatabaseOn(context.Background(), absent, create); err == nil {
				t.Error("a non-duplicate PgError was swallowed as success")
			}
		})
	})
}
