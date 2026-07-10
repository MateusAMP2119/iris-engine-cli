package pg

import (
	"context"
	"fmt"

	"github.com/MateusAMP2119/iris-engine-cli/internal/declare"
)

// This file is the live data-PAT read-role surface of specification sections 4
// and 7, the read-side analogue of the pipeline-role provisioning in roles.go. A
// data PAT owns an engine-managed read-only Postgres role that is NOLOGIN --
// assumed via SET ROLE on the shared read pool, never connected to directly -- so
// its provisioning differs from a pipeline login role in three ways: the role is
// NOLOGIN (no credential, no injected connection), it is granted membership TO
// the engine read-pool login (so the pool's login may SET ROLE to it), and its
// grants are read-only (SELECT on the granted fields). The engine's own read-pool
// login is ensured here too: the one identity the shared read pool connects as,
// which holds no table grants of its own and reads only through the data-PAT role
// it SET ROLEs into.
//
// pg owns the data cluster, so this CREATE ROLE / GRANT DDL is issued here beside
// every other provisioning statement (store owns the meta access ledger's truth;
// the two never cross). Roles are cluster-global, so this runs on the same
// data-database connection every other provisioning statement rides.

// EngineReadPoolRole is the fixed name of the engine's own read-pool login role:
// the single identity the shared read pool connects as (specification section 7).
// It holds no table grants of its own; every data-surface read runs under the
// caller PAT's role, assumed via SET ROLE, so this login is only a connection
// identity that the data-PAT roles are granted membership to.
const EngineReadPoolRole = "iris_engine_read"

// ReadPoolLoginProvision is the request to ensure the engine's read-pool login
// role on the data cluster: the fixed role name (EngineReadPoolRole), the
// credential-bearing DDL the meta layer renders (store.RenderSetRolePassword), and
// the two databases the login is bounded to. It carries no field grants -- the
// login reads only through the data-PAT roles it SET ROLEs into.
type ReadPoolLoginProvision struct {
	// Role is the read-pool login role name (EngineReadPoolRole).
	Role string
	// CredentialDDL is the ALTER ROLE ... PASSWORD statement the meta layer renders
	// from the engine-minted secret. It carries the raw credential; the provisioner
	// executes it in order but never constructs or logs it. Non-empty: a login role
	// always carries an engine-managed credential.
	CredentialDDL string
	// MetaDatabase is the control database the login must not reach (store.MetaDatabase).
	MetaDatabase string
	// DataDatabase is the data database the login connects to (DataDatabase).
	DataDatabase string
}

// ProvisionReadPoolLogin ensures the engine's read-pool login role exists on the
// data cluster with exactly its connection identity, issuing the DDL through db in
// order (specification section 7). It is idempotent: the role is created (with its
// least-privilege attributes baked in) if missing, and every credential/GRANT/REVOKE
// is idempotent, so a re-provision (including a credential rotation on daemon
// restart) is a safe no-op-or-update. Crucially it never re-asserts the role's
// attributes with an ALTER ROLE -- changing an existing role's SUPERUSER attribute
// requires the SUPERUSER attribute (PG16+), which the engine's non-superuser
// CREATEROLE admin lacks -- so a repeat daemon start never hard-fails on the already
// provisioned login. The ordered steps: create LOGIN (with attributes) if missing,
// set the engine-minted credential, deny the meta database, grant CONNECT on data.
func ProvisionReadPoolLogin(ctx context.Context, db DB, spec ReadPoolLoginProvision) error {
	stmts, err := renderProvisionReadPoolLogin(spec)
	if err != nil {
		return fmt.Errorf("pg: provision read-pool login %q: %w", spec.Role, err)
	}
	for _, stmt := range stmts {
		if err := db.Exec(ctx, stmt); err != nil {
			return fmt.Errorf("pg: provision read-pool login %q: %w", spec.Role, err)
		}
	}
	return nil
}

// renderProvisionReadPoolLogin renders the ordered DDL for the engine read-pool
// login. It validates the request first (a login role always carries a credential
// and names a meta and data database), so provisioning never issues a login with
// no password or one that could reach meta. It is pure, so the exact statement
// stream is derivable without a live cluster.
func renderProvisionReadPoolLogin(spec ReadPoolLoginProvision) ([]string, error) {
	if spec.Role == "" {
		return nil, fmt.Errorf("role name is empty")
	}
	if spec.CredentialDDL == "" {
		return nil, fmt.Errorf("credential DDL is empty (a login role must carry an engine-managed credential)")
	}
	if spec.MetaDatabase == "" {
		return nil, fmt.Errorf("meta database name is empty")
	}
	if spec.DataDatabase == "" {
		return nil, fmt.Errorf("data database name is empty")
	}

	role := quoteIdentifier(spec.Role)
	meta := quoteIdentifier(spec.MetaDatabase)
	data := quoteIdentifier(spec.DataDatabase)

	return []string{
		// Create the login with its least-privilege attributes baked in at creation --
		// LOGIN plus the NOSUPERUSER/NOCREATEDB/NOCREATEROLE defaults spelled out. The
		// attributes are set AT CREATE, never re-asserted by a later ALTER ROLE:
		// changing an existing role's SUPERUSER attribute requires the SUPERUSER
		// attribute itself (PG16+), so a re-asserting `ALTER ROLE ... NOSUPERUSER`
		// would fail for the engine's non-superuser CREATEROLE admin on every
		// subsequent daemon start. Creating with these attributes is allowed for a
		// CREATEROLE admin (it never grants an attribute it lacks). On a repeat start
		// the role already exists and the DO block skips creation; the credential and
		// database-scoping statements below are idempotent and require only ownership
		// the engine's admin already holds, so a daemon start never hard-fails because
		// a previous run already provisioned the login.
		fmt.Sprintf(`DO $iris_read_pool_login$
BEGIN
    IF NOT EXISTS (SELECT FROM pg_roles WHERE rolname = %s) THEN
        CREATE ROLE %s LOGIN NOSUPERUSER NOCREATEDB NOCREATEROLE;
    END IF;
END
$iris_read_pool_login$;`, quoteStringLiteral(spec.Role), role),
		spec.CredentialDDL,
		fmt.Sprintf("REVOKE CONNECT ON DATABASE %s FROM PUBLIC;", meta),
		fmt.Sprintf("REVOKE ALL ON DATABASE %s FROM %s;", meta, role),
		fmt.Sprintf("GRANT CONNECT ON DATABASE %s TO %s;", data, role),
	}, nil
}

// DataPATRoleProvision is the request to provision one data-PAT read-only role on
// the data cluster (specification sections 4 and 7): the NOLOGIN role name
// (store.DataPATRoleName), the engine read-pool login it grants membership to (so
// the pool may SET ROLE to it), the meta database it must not reach, the data
// database it reads, and the field-level read grants recorded at mint.
type DataPATRoleProvision struct {
	// Role is the data PAT's NOLOGIN read-role name, created NOLOGIN and never
	// altered to LOGIN (assumed via SET ROLE on the read path).
	Role string
	// PoolLoginRole is the engine read-pool login role membership is granted to, so
	// the pool's login may SET ROLE to this role (EngineReadPoolRole). Non-empty:
	// without the grant the read pool could never assume the role.
	PoolLoginRole string
	// MetaDatabase is the control database the role must not reach (store.MetaDatabase).
	MetaDatabase string
	// DataDatabase is the data database the role reads (DataDatabase).
	DataDatabase string
	// Grants are the field-level read grants; each distinct schema is granted USAGE
	// so the column grants resolve. Every grant is a read grant.
	Grants []declare.FieldGrant
}

// ProvisionDataPATRole ensures a data PAT's read-only role exists on the data
// cluster with exactly its declared read access and membership in the engine
// read-pool login, issuing the DDL through db in order (specification sections 4
// and 7). It is idempotent: the role is created NOLOGIN (with its least-privilege
// attributes baked in) if missing, and every GRANT/REVOKE is idempotent, so a retry
// after a partial failure re-issues cleanly. It never re-asserts the attributes with
// an ALTER ROLE, which would require the SUPERUSER attribute the engine's non-
// superuser CREATEROLE admin lacks (PG16+). The ordered steps are:
//
//  1. create the role NOLOGIN NOSUPERUSER NOCREATEDB NOCREATEROLE if it does not yet
//     exist -- never LOGIN, so the role holds no credential and can only be assumed;
//  2. deny the meta database -- revoke CONNECT from PUBLIC (default-deny) and from
//     the role -- so the control plane is unreachable to the data PAT (section 2);
//  3. grant CONNECT on the data database and USAGE on each granted schema;
//  4. apply each field-level SELECT grant (RenderGrant);
//  5. grant the role TO the engine read-pool login WITH SET (so the pool may SET ROLE
//     to it) but INHERIT FALSE (so the pool never silently holds its grants).
//
// It stops and returns on the first failing statement, naming it.
func ProvisionDataPATRole(ctx context.Context, db DB, spec DataPATRoleProvision) error {
	stmts, err := renderProvisionDataPATRole(spec)
	if err != nil {
		return fmt.Errorf("pg: provision data-PAT role %q: %w", spec.Role, err)
	}
	for _, stmt := range stmts {
		if err := db.Exec(ctx, stmt); err != nil {
			return fmt.Errorf("pg: provision data-PAT role %q: %w", spec.Role, err)
		}
	}
	return nil
}

// renderProvisionDataPATRole renders the ordered provisioning DDL for a data-PAT
// read role. It validates the request first (a NOLOGIN role names a pool login, a
// meta and data database), then emits create/deny/grant/membership in order (the
// attributes ride the CREATE, never a re-asserting ALTER). It is pure, so the exact
// statement stream is derivable without a live
// cluster; ProvisionDataPATRole issues it. A read grant carrying a write access
// kind is refused (a data-PAT role is read-only).
func renderProvisionDataPATRole(spec DataPATRoleProvision) ([]string, error) {
	if spec.Role == "" {
		return nil, fmt.Errorf("role name is empty")
	}
	if spec.PoolLoginRole == "" {
		return nil, fmt.Errorf("pool login role is empty (the read pool must be able to SET ROLE to this role)")
	}
	if spec.MetaDatabase == "" {
		return nil, fmt.Errorf("meta database name is empty")
	}
	if spec.DataDatabase == "" {
		return nil, fmt.Errorf("data database name is empty")
	}
	for _, g := range spec.Grants {
		if g.Access != declare.AccessRead {
			return nil, fmt.Errorf("grant on %s.%s.%s has access %q; a data-PAT role holds read grants only",
				g.Schema, g.Table, g.Field, g.Access)
		}
	}

	role := quoteIdentifier(spec.Role)
	pool := quoteIdentifier(spec.PoolLoginRole)
	meta := quoteIdentifier(spec.MetaDatabase)
	data := quoteIdentifier(spec.DataDatabase)

	stmts := []string{
		// 1. Create the role NOLOGIN with its least-privilege attributes baked in at
		// creation -- NOLOGIN plus the NOSUPERUSER/NOCREATEDB/NOCREATEROLE defaults
		// spelled out. The attributes are set AT CREATE, never re-asserted by a later
		// ALTER ROLE: changing an existing role's SUPERUSER attribute requires the
		// SUPERUSER attribute itself (PG16+), so a re-asserting ALTER would fail for
		// the engine's non-superuser CREATEROLE admin. A data-PAT role is created once
		// per mint (unique per token id), so creation is the only path; the DO block's
		// existence guard keeps a re-provision idempotent.
		fmt.Sprintf(`DO $iris_data_pat_role$
BEGIN
    IF NOT EXISTS (SELECT FROM pg_roles WHERE rolname = %s) THEN
        CREATE ROLE %s NOLOGIN NOSUPERUSER NOCREATEDB NOCREATEROLE;
    END IF;
END
$iris_data_pat_role$;`, quoteStringLiteral(spec.Role), role),
		// 2. Deny the meta control database.
		fmt.Sprintf("REVOKE CONNECT ON DATABASE %s FROM PUBLIC;", meta),
		fmt.Sprintf("REVOKE ALL ON DATABASE %s FROM %s;", meta, role),
		// 3. Grant CONNECT on the data database.
		fmt.Sprintf("GRANT CONNECT ON DATABASE %s TO %s;", data, role),
	}

	// 3 (cont.). USAGE on each distinct granted schema, in deterministic order.
	for _, schema := range distinctSchemas(spec.Grants) {
		stmts = append(stmts, fmt.Sprintf("GRANT USAGE ON SCHEMA %s TO %s;", quoteIdentifier(schema), role))
	}

	// 4. Each field-level SELECT grant.
	for _, g := range spec.Grants {
		ddl, err := RenderGrant(spec.Role, g.Schema, g.Table, g.Field, g.Access)
		if err != nil {
			return nil, err
		}
		stmts = append(stmts, ddl)
	}

	// 5. Membership: the read-pool login may SET ROLE to this role but never inherits
	// its privileges. SET TRUE lets the pool assume the role on the read path (PG16+
	// no longer implies SET from CREATEROLE); INHERIT FALSE keeps the pool login from
	// automatically holding this data-PAT's read grants when it has NOT set the role
	// (an ambient self-read must not silently gain every data PAT's grants). The grant
	// is idempotent -- a re-grant is a benign notice.
	stmts = append(stmts, fmt.Sprintf("GRANT %s TO %s WITH INHERIT FALSE, SET TRUE;", role, pool))

	return stmts, nil
}
