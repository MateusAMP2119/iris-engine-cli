package daemon

import (
	"context"
	"errors"
	"fmt"
	"strings"
)

// This file holds the startup privilege check for the admin DSN. Before any lane
// runs, the daemon validates that the admin role the DSN authenticates as holds the
// privileges the engine needs: CREATEROLE (to mint pipeline and data-PAT roles),
// CREATEDB (to create the meta database), and ownership of every managed schema.
// Superuser is never required — a plain role with those grants passes — and a
// superuser is accepted, not demanded. A missing privilege fails fast, naming what
// is missing, so a misconfigured DSN is caught at startup rather than mid-run.

// PrivilegeQuery is the catalog query the real reader runs to snapshot the admin
// role's cluster privileges: the current session role's CREATEROLE, CREATEDB, and
// superuser bits from pg_roles. rolsuper is selected only so the check can accept
// a superuser; it is never a requirement. The pgx-backed reader that runs this
// query lands in E02.3; the check logic here is proven with a scripted fake.
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
// admin DSN points at (PrivilegeQuery plus the managed-schema ownership check).
// The pgx-backed reader lands in E02.3; a scripted fake drives CheckPrivileges in
// tests, so the check needs no live Postgres.
type PrivilegeReader interface {
	// ReadPrivileges returns the admin role's current privilege snapshot.
	ReadPrivileges(ctx context.Context) (AdminPrivileges, error)
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
