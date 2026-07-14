package daemon

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/MateusAMP2119/iris-lakehouse/internal/store"
)

// This file holds the privilege check for the admin DSN: the validation that the
// admin role the DSN authenticates as holds the privileges the engine needs --
// CREATEROLE (to mint pipeline and data-PAT roles), CREATEDB (to create the meta
// database), and ownership of every managed schema. Superuser is never required —
// a plain role with those grants passes — and a superuser is accepted, not
// demanded. A missing privilege fails fast, naming what is missing, so a
// misconfigured DSN can be caught before any lane runs rather than mid-run.
//
// The check runs at both startup edges: engine install preflights the admin DSN
// before the first CREATE DATABASE, and daemon start preflights it before the
// meta connect (which lazily creates meta) and the read-pool role provisioning
// -- so a DSN that was never viable fails with a named grant, not a raw
// Postgres permission error mid-sequence. The live read is store's
// (AdminRolePrivileges, one short-lived admin connection); NewAdminPrivilegeReader
// adapts it to the seam here.

// PrivilegeQuery is the catalog query a reader runs to snapshot the admin role's
// cluster privileges: the current session role's CREATEROLE, CREATEDB, and
// superuser bits from pg_roles. rolsuper is selected only so the check can accept
// a superuser; it is never a requirement. The live reader
// (store.AdminRolePrivileges) runs this shape with the role name added.
const PrivilegeQuery = "SELECT rolcreaterole, rolcreatedb, rolsuper FROM pg_roles WHERE rolname = current_user"

// ErrInsufficientPrivilege is the sentinel a failed privilege check wraps: the
// admin DSN's role lacks a required privilege. Callers test it with errors.Is; the
// wrapped message names the missing privilege(s).
var ErrInsufficientPrivilege = errors.New("daemon: admin DSN role lacks a required privilege")

// AdminPrivileges is the admin role's privilege snapshot the startup check
// validates: its cluster-privilege bits read from pg_roles plus which managed
// schemas it does not own. It is what a PrivilegeReader returns.
type AdminPrivileges struct {
	// Role is the role the admin DSN authenticates as (current_user).
	Role string
	// CreateRole is the role's rolcreaterole bit: required (the engine mints
	// pipeline and data-PAT roles).
	CreateRole bool
	// CreateDB is the role's rolcreatedb bit: required (the engine creates the meta
	// database).
	CreateDB bool
	// Superuser is the role's rolsuper bit: accepted but never required. The check
	// never reads it as a gate; it is carried only so operators and diagnostics can
	// see the role is over-privileged.
	Superuser bool
	// UnownedManagedSchemas lists the managed schemas the admin role does not own.
	// Empty means it owns every managed schema (the required state); a non-empty
	// list fails the check, naming the schemas.
	UnownedManagedSchemas []string
}

// PrivilegeReader reads the admin role's privilege snapshot from the cluster the
// admin DSN points at. NewAdminPrivilegeReader is the live implementation (over
// store.AdminRolePrivileges); a scripted fake drives CheckPrivileges in tests, so
// the check logic needs no live Postgres.
type PrivilegeReader interface {
	// ReadPrivileges returns the admin role's current privilege snapshot.
	ReadPrivileges(ctx context.Context) (AdminPrivileges, error)
}

// NewAdminPrivilegeReader returns the live PrivilegeReader over the admin DSN:
// one short-lived admin/maintenance connection reading the session role's
// cluster-privilege bits (store.AdminRolePrivileges). UnownedManagedSchemas is
// always empty here: the engine's DDL runs under CREATE privileges, never schema
// ownership, so no ownership requirement exists to preflight -- the field stays
// for a future tier that introduces one.
func NewAdminPrivilegeReader(adminDSN string) PrivilegeReader {
	return adminPrivilegeReader{dsn: adminDSN}
}

// adminPrivilegeReader is the store-backed live PrivilegeReader.
type adminPrivilegeReader struct{ dsn string }

// ReadPrivileges reads the admin role's privilege bits from pg_roles.
func (r adminPrivilegeReader) ReadPrivileges(ctx context.Context) (AdminPrivileges, error) {
	role, createRole, createDB, superuser, err := store.AdminRolePrivileges(ctx, r.dsn)
	if err != nil {
		return AdminPrivileges{}, err
	}
	return AdminPrivileges{Role: role, CreateRole: createRole, CreateDB: createDB, Superuser: superuser}, nil
}

// CheckPrivileges reads the admin role's privileges through r and validates the
// engine's requirements: CREATEROLE, CREATEDB, and ownership of every managed
// schema. Superuser is never required and never rejected — a non-superuser role
// with the three grants passes, a superuser is accepted. A missing privilege fails
// fast with ErrInsufficientPrivilege, naming every missing privilege so the
// operator sees exactly what the admin DSN needs.
func CheckPrivileges(ctx context.Context, r PrivilegeReader) error {
	p, err := r.ReadPrivileges(ctx)
	if err != nil {
		return fmt.Errorf("daemon: read admin privileges: %w", err)
	}

	// A superuser can do everything the individual grants confer, whatever its
	// rolcreaterole/rolcreatedb bits say (a role created with bare SUPERUSER has
	// them false), so it passes outright -- accepted, never demanded.
	if p.Superuser {
		return nil
	}

	var missing []string
	if !p.CreateRole {
		missing = append(missing, "CREATEROLE")
	}
	if !p.CreateDB {
		missing = append(missing, "CREATEDB")
	}
	if len(p.UnownedManagedSchemas) > 0 {
		missing = append(missing, "ownership of managed schema(s) "+strings.Join(p.UnownedManagedSchemas, ", "))
	}
	if len(missing) == 0 {
		return nil
	}

	role := p.Role
	if role == "" {
		role = "current_user"
	}
	return fmt.Errorf("%w: role %q is missing %s (superuser is not required)", ErrInsufficientPrivilege, role, strings.Join(missing, "; "))
}
