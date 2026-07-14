//go:build conformance

package conformance

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"testing"
	"time"

	embeddedpostgres "github.com/fergusstrange/embedded-postgres"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/MateusAMP2119/iris-engine-cli/internal/declare"
	"github.com/MateusAMP2119/iris-engine-cli/internal/pg"
	"github.com/MateusAMP2119/iris-engine-cli/internal/store"
)

// insufficientPrivilege is Postgres' SQLSTATE for a privilege violation: the code the
// database itself raises when a role reads a table or column it was never granted, or
// connects to a database it may not. Asserting on this code is asserting that Postgres
// -- not application code -- refused the access.
const insufficientPrivilege = "42501"

// TestPipelineRolePostgresEnforcement stands up a real Postgres cluster the engine has
// never touched, ensures the meta and data databases, provisions one least-privilege
// pipeline login role through the live provisioning path (pg.ProvisionPipelineRole
// with the engine-minted credential and field grants on analytics.orders[id,amount]),
// and then connects to the cluster AS that role -- the exact credential the engine
// minted -- to prove two enforcement contracts against the live database:
//
//   - The pipeline role cannot connect to the meta control database (Postgres
//     refuses at CONNECT), while it can reach the data database -- so meta is
//     hidden, not the role broken.
//   - Reading a declared column (amount) succeeds, and reading a column the
//     pipeline did not declare (customer_id) fails at the database with
//     insufficient_privilege -- Postgres physically bounds the read.
//
// It shares one cluster and one provisioned role across both legs (both are properties
// of the same real role), and drives the pg provisioning code directly against the
// live cluster (the external_data_db conformance pattern) rather than the CLI, so the
// legs prove the DDL the engine issues, enforced by a real Postgres.
func TestPipelineRolePostgresEnforcement(t *testing.T) {
	const (
		superuser = "postgres"
		superpw   = "superpw"
	)
	port := freePort(t)
	dataDir := filepath.Join(t.TempDir(), "data")
	runtimeDir := filepath.Join(t.TempDir(), "runtime")

	cluster := embeddedpostgres.NewDatabase(embeddedpostgres.DefaultConfig().
		Version(embeddedpostgres.V18).
		Username(superuser).Password(superpw).Database("postgres").
		Port(port).
		DataPath(dataDir).RuntimePath(runtimeDir).
		StartTimeout(90 * time.Second))
	if err := cluster.Start(); err != nil {
		t.Fatalf("start bare Postgres cluster: %v", err)
	}
	t.Cleanup(func() { _ = cluster.Stop() })

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	dsnTo := func(db string) string {
		return fmt.Sprintf("postgres://%s:%s@localhost:%d/%s?sslmode=disable", superuser, superpw, port, db)
	}

	// The engine owns two databases in the cluster: the meta control database and the
	// data database. Ensure meta (the data database is ensured by pg.Connect below).
	superConn, err := pgx.Connect(ctx, dsnTo("postgres"))
	if err != nil {
		t.Fatalf("connect as superuser: %v", err)
	}
	if _, err := superConn.Exec(ctx, "CREATE DATABASE "+store.MetaDatabase); err != nil {
		_ = superConn.Close(ctx)
		t.Fatalf("create meta database: %v", err)
	}
	_ = superConn.Close(ctx)

	// The data-database admin client (as the engine opens it): ensures the data
	// database and issues every provisioning statement on the data connection.
	client, err := pg.Connect(ctx, testConnSource{dsn: dsnTo("postgres")})
	if err != nil {
		t.Fatalf("pg.Connect (data database): %v", err)
	}
	t.Cleanup(client.Close)

	// A declared table for the pipeline to be granted field access on.
	for _, stmt := range []string{
		`CREATE SCHEMA IF NOT EXISTS analytics`,
		`CREATE TABLE IF NOT EXISTS analytics.orders (
			id uuid PRIMARY KEY,
			customer_id uuid,
			amount numeric,
			created_at timestamptz
		)`,
	} {
		if err := client.Exec(ctx, stmt); err != nil {
			t.Fatalf("provision declared table: %v", err)
		}
	}

	// The engine mints the credential; the author supplies none. The pipeline declares
	// reads on analytics.orders[id, amount] -- customer_id and created_at are NOT
	// declared, so the role must never be able to read them.
	secret, err := store.GenerateSecret()
	if err != nil {
		t.Fatalf("GenerateSecret: %v", err)
	}
	role := pg.PipelineRoleName("ingest")
	grants := []declare.FieldGrant{
		{Schema: "analytics", Table: "orders", Field: "id", Access: declare.AccessRead},
		{Schema: "analytics", Table: "orders", Field: "amount", Access: declare.AccessRead},
	}

	// Provision the least-privilege role live: create it, set the engine-minted
	// credential, deny meta, grant data connect + schema usage, apply the field grants.
	if err := pg.ProvisionPipelineRole(ctx, client, pg.RoleProvision{
		Role:          role,
		CredentialDDL: store.RenderSetRolePassword(role, secret),
		MetaDatabase:  store.MetaDatabase,
		DataDatabase:  pg.DataDatabase,
		Grants:        grants,
	}); err != nil {
		t.Fatalf("ProvisionPipelineRole: %v", err)
	}

	// A second provision is a no-op: provisioning is idempotent.
	if err := pg.ProvisionPipelineRole(ctx, client, pg.RoleProvision{
		Role:          role,
		CredentialDDL: store.RenderSetRolePassword(role, secret),
		MetaDatabase:  store.MetaDatabase,
		DataDatabase:  pg.DataDatabase,
		Grants:        grants,
	}); err != nil {
		t.Fatalf("ProvisionPipelineRole is not idempotent: %v", err)
	}

	scopedData, err := store.BuildScopedConn(store.ScopedConnParams{
		Host: "localhost", Port: int(port), Database: pg.DataDatabase, Options: "sslmode=disable",
	}, role, secret)
	if err != nil {
		t.Fatalf("BuildScopedConn (data): %v", err)
	}
	scopedMeta, err := store.BuildScopedConn(store.ScopedConnParams{
		Host: "localhost", Port: int(port), Database: store.MetaDatabase, Options: "sslmode=disable",
	}, role, secret)
	if err != nil {
		t.Fatalf("BuildScopedConn (meta): %v", err)
	}

	t.Run("meta-hidden-from-pipeline", func(t *testing.T) {
		// The pipeline role connects to the data database it was granted -- proof the
		// role and its engine-minted credential work.
		dataConn, err := pgx.Connect(ctx, scopedData.EnvValue())
		if err != nil {
			t.Fatalf("pipeline role could not reach the data database it was granted: %v", err)
		}
		_ = dataConn.Close(ctx)

		// The same role connecting to the meta control database is refused by Postgres
		// at CONNECT: meta is hidden from the pipeline.
		metaConn, err := pgx.Connect(ctx, scopedMeta.EnvValue())
		if err == nil {
			_ = metaConn.Close(ctx)
			t.Fatal("pipeline role connected to the meta database; meta is not hidden from the pipeline")
		}
		var pgErr *pgconn.PgError
		if errors.As(err, &pgErr) {
			if pgErr.Code != insufficientPrivilege {
				t.Errorf("meta connect denied with SQLSTATE %s, want %s (insufficient_privilege): %v",
					pgErr.Code, insufficientPrivilege, err)
			}
		} else {
			t.Errorf("meta connect failed but not with a Postgres privilege error: %v", err)
		}
	})

	t.Run("grants-postgres-enforced", func(t *testing.T) {
		conn, err := pgx.Connect(ctx, scopedData.EnvValue())
		if err != nil {
			t.Fatalf("connect as pipeline role: %v", err)
		}
		defer func() { _ = conn.Close(ctx) }()

		// The declared read succeeds: SELECT of a granted column is allowed.
		var declared int
		if err := conn.QueryRow(ctx, "SELECT count(amount) FROM analytics.orders").Scan(&declared); err != nil {
			t.Fatalf("declared read of analytics.orders.amount was refused: %v", err)
		}

		// The undeclared read fails at the database: a column the pipeline never
		// declared is refused with insufficient_privilege -- Postgres physically bounds
		// the read, not application code.
		_, err = conn.Exec(ctx, "SELECT customer_id FROM analytics.orders")
		if err == nil {
			t.Fatal("undeclared read of analytics.orders.customer_id succeeded; Postgres did not enforce the grant")
		}
		var pgErr *pgconn.PgError
		if !errors.As(err, &pgErr) {
			t.Fatalf("undeclared read failed but not with a Postgres error: %v", err)
		}
		if pgErr.Code != insufficientPrivilege {
			t.Errorf("undeclared read denied with SQLSTATE %s, want %s (insufficient_privilege): %v",
				pgErr.Code, insufficientPrivilege, err)
		}
	})
}
