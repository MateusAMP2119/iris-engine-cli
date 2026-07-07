package pg

import (
	"context"
	"fmt"
	"sort"
	"strings"

	"github.com/MateusAMP2119/iris-engine-cli/internal/declare"
)

// This file is the live pipeline-role surface of specification sections 4 and 7: the
// least-privilege Postgres login role the engine ensures for each pipeline, the grants
// it applies onto the data database, and the meta-database denial that keeps the
// control plane unreachable to a pipeline. pg owns the data cluster, so the CREATE
// ROLE / GRANT / REVOKE DDL is issued here (store owns the meta access ledger's truth;
// the two never cross). Roles are cluster-global, so this DDL runs on the same
// data-database admin connection every other provisioning statement rides.

// pipelineRolePrefix is the fixed prefix of every engine-managed pipeline login role
// name, so a role in the cluster is recognizably engine-owned and never collides with
// a hand-created role.
const pipelineRolePrefix = "iris_pipeline_"

// PipelineRoleName derives the cluster-unique Postgres login-role name for a
// pipeline's least-privilege role (roles.pg_role): the fixed engine prefix followed by
// the pipeline name. It is the single derivation both the live role provisioning (pg)
// and the meta access ledger (store, via the caller) use, so the ledger's pg_role and
// the created role are always the same name.
func PipelineRoleName(pipeline string) string {
	return pipelineRolePrefix + pipeline
}

// RoleProvision is the request to provision one least-privilege pipeline login role on
// the data cluster (specification sections 4 and 7): the role name, the credential-
// bearing password DDL the meta layer renders, the meta database the role must not
// reach, the data database it connects to, and the declared field grants.
type RoleProvision struct {
	// Role is the pipeline's login-role name (PipelineRoleName), created NOLOGIN then
	// altered LOGIN with the least-privilege attributes.
	Role string
	// CredentialDDL is the ALTER ROLE ... PASSWORD statement the meta layer renders
	// from the engine-minted secret (store.RenderSetRolePassword). It carries the raw
	// credential; the provisioner executes it in order but never constructs or logs
	// it. It must be non-empty: a login role always has an engine-managed credential.
	CredentialDDL string
	// MetaDatabase is the control database the role must not reach (store.MetaDatabase);
	// provisioning revokes CONNECT so the pipeline can never open a meta session.
	MetaDatabase string
	// DataDatabase is the data database the role connects to (DataDatabase).
	DataDatabase string
	// Grants are the declared field-level grants applied onto the data database; each
	// distinct schema is granted USAGE so the column grants resolve.
	Grants []declare.FieldGrant
}

// ProvisionPipelineRole ensures a pipeline's least-privilege login role exists on the
// data cluster with exactly its declared access, issuing the DDL through db in order
// (specification sections 4 and 7). It is idempotent: the role is created if missing
// then always re-asserted, and every GRANT/REVOKE is idempotent, so a re-provision
// (including a credential rotation) is a safe no-op-or-update. The ordered steps are:
//
//  1. create the role NOLOGIN if it does not yet exist;
//  2. assert its least-privilege attributes (LOGIN, NOSUPERUSER, NOCREATEDB,
//     NOCREATEROLE);
//  3. set the engine-minted credential (the meta-rendered CredentialDDL);
//  4. deny the meta database -- revoke CONNECT from PUBLIC (default-deny) and from the
//     role -- so the control plane is unreachable to the pipeline (section 2);
//  5. grant CONNECT on the data database and USAGE on each granted schema;
//  6. apply each declared field grant (RenderGrant).
//
// It stops and returns on the first failing statement, naming it; because every step
// is idempotent, a retry after a partial failure re-issues cleanly.
func ProvisionPipelineRole(ctx context.Context, db DB, spec RoleProvision) error {
	stmts, err := renderProvisionPipelineRole(spec)
	if err != nil {
		return fmt.Errorf("pg: provision pipeline role %q: %w", spec.Role, err)
	}
	for _, stmt := range stmts {
		if err := db.Exec(ctx, stmt); err != nil {
			return fmt.Errorf("pg: provision pipeline role %q: %w", spec.Role, err)
		}
	}
	return nil
}

// renderProvisionPipelineRole renders the ordered provisioning DDL for a pipeline role.
// It validates the request first (a login role always carries a credential and names a
// meta and data database), so provisioning never issues a role with no password or a
// role that could reach meta. It is pure, so the exact statement stream is derivable
// without a live cluster; ProvisionPipelineRole issues it.
func renderProvisionPipelineRole(spec RoleProvision) ([]string, error) {
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

	stmts := []string{
		// 1. Create the role NOLOGIN if missing. The role name is a quoted identifier
		// for CREATE ROLE and a quoted string literal for the pg_roles existence check.
		fmt.Sprintf(`DO $iris_pipeline_role$
BEGIN
    IF NOT EXISTS (SELECT FROM pg_roles WHERE rolname = %s) THEN
        CREATE ROLE %s NOLOGIN;
    END IF;
END
$iris_pipeline_role$;`, quoteStringLiteral(spec.Role), role),
		// 2. Assert least-privilege attributes (idempotent).
		fmt.Sprintf("ALTER ROLE %s WITH LOGIN NOSUPERUSER NOCREATEDB NOCREATEROLE;", role),
		// 3. Set the engine-minted credential (rendered by the meta layer).
		spec.CredentialDDL,
		// 4. Deny the meta control database: default-deny for every non-owner role, plus
		// an explicit role-scoped revoke.
		fmt.Sprintf("REVOKE CONNECT ON DATABASE %s FROM PUBLIC;", meta),
		fmt.Sprintf("REVOKE ALL ON DATABASE %s FROM %s;", meta, role),
		// 5. Grant CONNECT on the data database.
		fmt.Sprintf("GRANT CONNECT ON DATABASE %s TO %s;", data, role),
	}

	// 5 (cont.). USAGE on each distinct granted schema, in deterministic order, so the
	// column grants resolve.
	for _, schema := range distinctSchemas(spec.Grants) {
		stmts = append(stmts, fmt.Sprintf("GRANT USAGE ON SCHEMA %s TO %s;", quoteIdentifier(schema), role))
	}

	// 6. Each declared field grant.
	for _, g := range spec.Grants {
		ddl, err := RenderGrant(spec.Role, g.Schema, g.Table, g.Field, g.Access)
		if err != nil {
			return nil, err
		}
		stmts = append(stmts, ddl)
	}

	return stmts, nil
}

// distinctSchemas returns the distinct schemas named by grants, sorted, so the schema
// USAGE grants are deterministic and each schema is granted once.
func distinctSchemas(grants []declare.FieldGrant) []string {
	seen := map[string]struct{}{}
	var out []string
	for _, g := range grants {
		if _, ok := seen[g.Schema]; ok {
			continue
		}
		seen[g.Schema] = struct{}{}
		out = append(out, g.Schema)
	}
	sort.Strings(out)
	return out
}

// quoteStringLiteral renders s as a Postgres string literal, doubling every embedded
// single quote (the standard escape; standard_conforming_strings is on by default). It
// is used for the pg_roles existence check's role-name literal in the create DDL.
func quoteStringLiteral(s string) string {
	return `'` + strings.ReplaceAll(s, `'`, `''`) + `'`
}
