package daemon_test

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/MateusAMP2119/iris-lakehouse/internal/daemon"
)

// scriptedPrivileges is a scripted fake of the startup privilege reader: it
// returns a fixed AdminPrivileges snapshot (or an injected error) without a live
// Postgres, so the privilege-check logic is driven at integration tier with fakes.
type scriptedPrivileges struct {
	priv daemon.AdminPrivileges
	err  error
}

func (s scriptedPrivileges) ReadPrivileges(context.Context) (daemon.AdminPrivileges, error) {
	return s.priv, s.err
}

// TestAdminDSNPrivilegeCheck proves startup validates the admin DSN holds
// CREATEROLE, CREATEDB, and managed-schema ownership, failing fast when a privilege
// is missing and never requiring superuser: a plain non-superuser role with the
// three grants passes, and a superuser is accepted but never demanded.
func TestAdminDSNPrivilegeCheck(t *testing.T) {
	ctx := context.Background()

	t.Run("non-superuser with all privileges passes", func(t *testing.T) {
		r := scriptedPrivileges{priv: daemon.AdminPrivileges{
			Role: "iris_admin", CreateRole: true, CreateDB: true, Superuser: false,
		}}
		if err := daemon.CheckPrivileges(ctx, r); err != nil {
			t.Errorf("CheckPrivileges(non-superuser, all grants) = %v, want nil (superuser never required)", err)
		}
	})

	t.Run("superuser accepted but not required", func(t *testing.T) {
		r := scriptedPrivileges{priv: daemon.AdminPrivileges{
			Role: "postgres", CreateRole: true, CreateDB: true, Superuser: true,
		}}
		if err := daemon.CheckPrivileges(ctx, r); err != nil {
			t.Errorf("CheckPrivileges(superuser) = %v, want nil (superuser accepted)", err)
		}
	})

	t.Run("missing CREATEROLE fails fast naming it", func(t *testing.T) {
		r := scriptedPrivileges{priv: daemon.AdminPrivileges{
			Role: "weak", CreateRole: false, CreateDB: true,
		}}
		err := daemon.CheckPrivileges(ctx, r)
		if !errors.Is(err, daemon.ErrInsufficientPrivilege) {
			t.Fatalf("CheckPrivileges(no CREATEROLE) = %v, want ErrInsufficientPrivilege", err)
		}
		if !strings.Contains(err.Error(), "CREATEROLE") {
			t.Errorf("error %q does not name the missing CREATEROLE privilege", err)
		}
	})

	t.Run("missing CREATEDB fails fast naming it", func(t *testing.T) {
		r := scriptedPrivileges{priv: daemon.AdminPrivileges{
			Role: "weak", CreateRole: true, CreateDB: false,
		}}
		err := daemon.CheckPrivileges(ctx, r)
		if !errors.Is(err, daemon.ErrInsufficientPrivilege) {
			t.Fatalf("CheckPrivileges(no CREATEDB) = %v, want ErrInsufficientPrivilege", err)
		}
		if !strings.Contains(err.Error(), "CREATEDB") {
			t.Errorf("error %q does not name the missing CREATEDB privilege", err)
		}
	})

	t.Run("missing managed-schema ownership fails fast naming it", func(t *testing.T) {
		r := scriptedPrivileges{priv: daemon.AdminPrivileges{
			Role: "weak", CreateRole: true, CreateDB: true,
			UnownedManagedSchemas: []string{"meta.public"},
		}}
		err := daemon.CheckPrivileges(ctx, r)
		if !errors.Is(err, daemon.ErrInsufficientPrivilege) {
			t.Fatalf("CheckPrivileges(unowned schema) = %v, want ErrInsufficientPrivilege", err)
		}
		if !strings.Contains(err.Error(), "meta.public") {
			t.Errorf("error %q does not name the unowned managed schema", err)
		}
	})

	t.Run("all privileges missing are reported together", func(t *testing.T) {
		r := scriptedPrivileges{priv: daemon.AdminPrivileges{
			Role: "weak", CreateRole: false, CreateDB: false,
			UnownedManagedSchemas: []string{"meta.public", "analytics"},
		}}
		err := daemon.CheckPrivileges(ctx, r)
		if !errors.Is(err, daemon.ErrInsufficientPrivilege) {
			t.Fatalf("CheckPrivileges(all missing) = %v, want ErrInsufficientPrivilege", err)
		}
		for _, want := range []string{"CREATEROLE", "CREATEDB", "meta.public", "analytics"} {
			if !strings.Contains(err.Error(), want) {
				t.Errorf("error %q does not name missing %q", err, want)
			}
		}
	})

	t.Run("reader error is propagated", func(t *testing.T) {
		sentinel := errors.New("connection refused")
		r := scriptedPrivileges{err: sentinel}
		if err := daemon.CheckPrivileges(ctx, r); !errors.Is(err, sentinel) {
			t.Errorf("CheckPrivileges(reader error) = %v, want it to wrap the reader error", err)
		}
	})

	t.Run("privilege query reads the current role from pg_roles", func(t *testing.T) {
		q := strings.ToLower(daemon.PrivilegeQuery)
		for _, want := range []string{"rolcreaterole", "rolcreatedb", "rolsuper", "pg_roles", "current_user"} {
			if !strings.Contains(q, want) {
				t.Errorf("PrivilegeQuery %q does not reference %q", daemon.PrivilegeQuery, want)
			}
		}
	})
}

// TestSuperuserBypassesGrantBits proves a bare superuser passes the check even
// with its rolcreaterole/rolcreatedb bits false: a role created with just
// SUPERUSER can do everything the individual grants confer, so requiring the
// bits would refuse a working DSN.
func TestSuperuserBypassesGrantBits(t *testing.T) {
	t.Run("superuser-bypasses-grant-bits", func(t *testing.T) {
		r := scriptedPrivileges{priv: daemon.AdminPrivileges{
			Role: "root_only", CreateRole: false, CreateDB: false, Superuser: true,
		}}
		if err := daemon.CheckPrivileges(context.Background(), r); err != nil {
			t.Errorf("CheckPrivileges(bare superuser) = %v, want nil (a superuser holds every capability the grants confer)", err)
		}
	})
}
