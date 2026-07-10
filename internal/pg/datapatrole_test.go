package pg_test

import (
	"context"
	"strings"
	"testing"

	"github.com/MateusAMP2119/iris-engine-cli/internal/declare"
	"github.com/MateusAMP2119/iris-engine-cli/internal/pg"
	"github.com/MateusAMP2119/iris-engine-cli/internal/pg/pgtest"
)

// This file proves the data-PAT read-role and engine read-pool login provisioning
// DDL of specification sections 4 and 7: a data PAT owns a NOLOGIN read role,
// assumed via SET ROLE by the engine read-pool login, granted read-only on its
// mint fields and membership in the pool login so the pool can assume it. It
// drives the real render/exec code through the recording fake, no live Postgres.

// TestProvisionDataPATRoleNologinSetRole proves the data-PAT role provisioning is
// a NOLOGIN read role (never LOGIN, no credential), denied the meta database,
// granted only its declared read fields, and made assumable by the engine
// read-pool login via a membership grant -- the SET ROLE cycle's precondition.
//
// spec: S04/data-pat-role-nologin-set-role
func TestProvisionDataPATRoleNologinSetRole(t *testing.T) {
	rec := pgtest.New()
	spec := pg.DataPATRoleProvision{
		Role:          "iris_pat_abc123",
		PoolLoginRole: pg.EngineReadPoolRole,
		MetaDatabase:  "meta",
		DataDatabase:  pg.DataDatabase,
		Grants: []declare.FieldGrant{
			{Schema: "analytics", Table: "orders", Field: "id", Access: declare.AccessRead},
			{Schema: "analytics", Table: "orders", Field: "amount", Access: declare.AccessRead},
		},
	}
	if err := pg.ProvisionDataPATRole(context.Background(), rec, spec); err != nil {
		t.Fatalf("ProvisionDataPATRole: %v", err)
	}
	stmts := rec.Statements()
	joined := strings.Join(stmts, "\n")

	// NOLOGIN, never LOGIN: the role is assumed, never connected to.
	if !strings.Contains(joined, "CREATE ROLE \"iris_pat_abc123\" NOLOGIN") {
		t.Errorf("role is not created NOLOGIN:\n%s", joined)
	}
	if strings.Contains(joined, "WITH LOGIN") || strings.Contains(joined, "PASSWORD") {
		t.Errorf("data-PAT role must never be a login role or carry a credential:\n%s", joined)
	}
	// Attributes are baked in at CREATE, never re-asserted by an ALTER ROLE: a
	// re-asserting `ALTER ROLE ... NOSUPERUSER` requires the SUPERUSER attribute and
	// so fails for the engine's non-superuser CREATEROLE admin (the CI regression).
	if strings.Contains(joined, "ALTER ROLE") {
		t.Errorf("no ALTER ROLE may be emitted; attributes are set at CREATE:\n%s", joined)
	}
	// Meta is denied.
	if !strings.Contains(joined, "REVOKE CONNECT ON DATABASE \"meta\" FROM PUBLIC;") ||
		!strings.Contains(joined, "REVOKE ALL ON DATABASE \"meta\" FROM \"iris_pat_abc123\";") {
		t.Errorf("meta database is not denied:\n%s", joined)
	}
	// Only the granted fields (SELECT), plus schema USAGE.
	if !strings.Contains(joined, "GRANT USAGE ON SCHEMA \"analytics\" TO \"iris_pat_abc123\";") {
		t.Errorf("schema USAGE not granted:\n%s", joined)
	}
	if !strings.Contains(joined, `GRANT SELECT ("id") ON "analytics"."orders" TO "iris_pat_abc123";`) ||
		!strings.Contains(joined, `GRANT SELECT ("amount") ON "analytics"."orders" TO "iris_pat_abc123";`) {
		t.Errorf("field SELECT grants not rendered:\n%s", joined)
	}
	// Membership in the pool login is the SET ROLE precondition, and it is last. It
	// grants SET (so the pool may assume the role) but not INHERIT (so the pool never
	// silently holds the data-PAT's grants without setting the role).
	last := stmts[len(stmts)-1]
	if last != `GRANT "iris_pat_abc123" TO "iris_engine_read" WITH INHERIT FALSE, SET TRUE;` {
		t.Errorf("last statement is not the pool-login SET-only membership grant: %q", last)
	}
}

// TestProvisionDataPATRoleRejectsWriteGrant proves a write-access grant is refused:
// a data-PAT role holds read grants only.
//
// spec: S04/data-pat-role-nologin-set-role
func TestProvisionDataPATRoleRejectsWriteGrant(t *testing.T) {
	rec := pgtest.New()
	err := pg.ProvisionDataPATRole(context.Background(), rec, pg.DataPATRoleProvision{
		Role:          "iris_pat_x",
		PoolLoginRole: pg.EngineReadPoolRole,
		MetaDatabase:  "meta",
		DataDatabase:  pg.DataDatabase,
		Grants:        []declare.FieldGrant{{Schema: "analytics", Table: "orders", Field: "amount", Access: declare.AccessWrite}},
	})
	if err == nil {
		t.Fatalf("expected a refusal for a write grant on a read-only data-PAT role")
	}
	if len(rec.Statements()) != 0 {
		t.Errorf("no DDL should be issued when the request is refused: %v", rec.Statements())
	}
}

// TestProvisionReadPoolLogin proves the engine read-pool login is a LOGIN role
// with the engine-minted credential, denied meta and granted CONNECT on data,
// holding no table grants of its own.
//
// spec: S07/read-pool-set-role-cycle
func TestProvisionReadPoolLogin(t *testing.T) {
	rec := pgtest.New()
	spec := pg.ReadPoolLoginProvision{ //nolint:gosec // G101: test-only fake DSN, not a real credential
		Role:          pg.EngineReadPoolRole,
		CredentialDDL: `ALTER ROLE "iris_engine_read" WITH PASSWORD 'sekret';`,
		MetaDatabase:  "meta",
		DataDatabase:  pg.DataDatabase,
	}
	if err := pg.ProvisionReadPoolLogin(context.Background(), rec, spec); err != nil {
		t.Fatalf("ProvisionReadPoolLogin: %v", err)
	}
	joined := strings.Join(rec.Statements(), "\n")
	if !strings.Contains(joined, "CREATE ROLE \"iris_engine_read\" LOGIN") {
		t.Errorf("read-pool login not created LOGIN:\n%s", joined)
	}
	if !strings.Contains(joined, `ALTER ROLE "iris_engine_read" WITH PASSWORD 'sekret';`) {
		t.Errorf("credential DDL not issued:\n%s", joined)
	}
	if !strings.Contains(joined, "GRANT CONNECT ON DATABASE \"data\" TO \"iris_engine_read\";") {
		t.Errorf("CONNECT on data not granted:\n%s", joined)
	}
	if strings.Contains(joined, "GRANT SELECT") {
		t.Errorf("the read-pool login must hold no table grants of its own:\n%s", joined)
	}
	// The attributes are set on the CREATE (inside the DO block), never re-asserted by
	// an attribute-changing ALTER ROLE: a `ALTER ROLE ... NOSUPERUSER` requires the
	// SUPERUSER attribute and fails for the engine's non-superuser CREATEROLE admin on
	// every repeat daemon start -- the CI regression this guards against. (The
	// credential is itself an `ALTER ROLE ... PASSWORD`, which is fine; only an
	// attribute re-assertion is forbidden.)
	for _, stmt := range rec.Statements() {
		if strings.Contains(stmt, "ALTER ROLE") && strings.Contains(stmt, "NOSUPERUSER") {
			t.Errorf("attribute-changing ALTER ROLE emitted (fails for a non-superuser admin): %q", stmt)
		}
	}
	if !strings.Contains(joined, "CREATE ROLE \"iris_engine_read\" LOGIN NOSUPERUSER NOCREATEDB NOCREATEROLE") {
		t.Errorf("login attributes are not baked into CREATE:\n%s", joined)
	}
}
