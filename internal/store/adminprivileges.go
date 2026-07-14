package store

import (
	"context"
	"fmt"

	"github.com/jackc/pgx/v5"
)

// This file is the live read behind the daemon's admin-DSN privilege preflight:
// one short-lived connection on the admin/maintenance DSN reading the session
// role's cluster-privilege bits from pg_roles. The validation itself is the
// daemon's (CheckPrivileges); store owns only the pgx read, keeping the daemon
// free of database clients.

// adminPrivilegesSQL reads the current session role's name and cluster-privilege
// bits: CREATEROLE (the engine mints pipeline and data-PAT roles), CREATEDB (the
// engine creates the meta and data databases), and the superuser bit (accepted,
// never required). It is daemon.PrivilegeQuery with the role name added.
const adminPrivilegesSQL = `SELECT rolname, rolcreaterole, rolcreatedb, rolsuper FROM pg_roles WHERE rolname = current_user`

// AdminRolePrivileges reads the admin role's cluster-privilege bits on one
// short-lived admin/maintenance connection: the role the DSN authenticates as,
// its CREATEROLE and CREATEDB grants, and whether it is a superuser.
func AdminRolePrivileges(ctx context.Context, adminDSN string) (role string, createRole, createDB, superuser bool, err error) {
	conn, err := pgx.Connect(ctx, adminDSN)
	if err != nil {
		return "", false, false, false, fmt.Errorf("store: open admin connection for privilege read: %w", err)
	}
	defer func() { _ = conn.Close(ctx) }()

	if err := conn.QueryRow(ctx, adminPrivilegesSQL).Scan(&role, &createRole, &createDB, &superuser); err != nil {
		return "", false, false, false, fmt.Errorf("store: read admin role privileges: %w", err)
	}
	return role, createRole, createDB, superuser, nil
}
