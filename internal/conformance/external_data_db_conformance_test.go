//go:build conformance

package conformance

import (
	"context"
	"fmt"
	"net"
	"path/filepath"
	"testing"
	"time"

	embeddedpostgres "github.com/fergusstrange/embedded-postgres"
	"github.com/jackc/pgx/v5"

	"github.com/MateusAMP2119/iris-engine-cli/internal/pg"
)

// testConnSource is a pg.ConnSource over a fixed DSN, so a test can drive pg.Connect
// against a hand-built cluster/role exactly as the daemon drives it from the admin DSN.
type testConnSource struct{ dsn string }

func (s testConnSource) ConnString() string { return s.dsn }

// TestExternalDataDatabaseAdminOwned reproduces the external-mode CI shape and proves
// the data-database fix (one cluster, one admin DSN; the
// engine bootstraps the databases it owns, mirroring how it creates meta). The CI
// Postgres exposes a non-superuser admin role (CREATEDB, but it does not own the DSN's
// default database, which the cluster superuser owns). Before the fix, the data pool
// targeted that default database, so provisioning's CREATE SCHEMA failed with
// "permission denied for database postgres". After the fix, pg.Connect ensures an
// admin-owned data database and points the pool at it, so provisioning succeeds.
//
// The reproduction stands up a bare Postgres cluster (no iris engine has touched it),
// mints a non-superuser CREATEDB role, and drives pg.Connect + the provisioning
// capture-function step as that role -- the exact statement that failed in CI.
func TestExternalDataDatabaseAdminOwned(t *testing.T) {
	t.Run("apply-repeat-noop", func(t *testing.T) {
		const (
			superuser = "postgres"
			superpw   = "superpw"
			admin     = "ci_admin"
			adminpw   = "ci_admin_pw"
		)
		port := freePort(t)
		dataDir := filepath.Join(t.TempDir(), "data")
		runtimeDir := filepath.Join(t.TempDir(), "runtime")

		// A bare cluster the iris engine has never touched: the superuser owns the
		// default 'postgres' database, exactly like a CI service container.
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

		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()

		// Mint the non-superuser admin role the CI DSN authenticates as: CREATEDB (it
		// can create its own databases) but NOT a superuser and NOT the owner of the
		// default 'postgres' database.
		superDSN := fmt.Sprintf("postgres://%s:%s@localhost:%d/postgres?sslmode=disable", superuser, superpw, port)
		superConn, err := pgx.Connect(ctx, superDSN)
		if err != nil {
			t.Fatalf("connect as superuser: %v", err)
		}
		if _, err := superConn.Exec(ctx, fmt.Sprintf("CREATE ROLE %s LOGIN CREATEDB PASSWORD '%s'", admin, adminpw)); err != nil {
			_ = superConn.Close(ctx)
			t.Fatalf("create non-superuser admin role: %v", err)
		}
		_ = superConn.Close(ctx)

		// Drive pg.Connect exactly as the daemon does in external mode: the admin DSN's
		// database is the superuser-owned default 'postgres'. The fix must NOT provision
		// there; it must ensure an admin-owned data database and target it.
		adminDSN := fmt.Sprintf("postgres://%s:%s@localhost:%d/postgres?sslmode=disable", admin, adminpw, port)
		client, err := pg.Connect(ctx, testConnSource{dsn: adminDSN})
		if err != nil {
			t.Fatalf("pg.Connect as the non-superuser admin: %v", err)
		}
		t.Cleanup(client.Close)

		// The provisioning capture-function step is the exact statement that failed in
		// CI ("permission denied for database postgres"). It must now succeed, because
		// the pool targets the admin-owned data database.
		if err := client.EnsureCaptureFunction(ctx); err != nil {
			t.Fatalf("EnsureCaptureFunction as the non-superuser admin failed; the data pool is not on an admin-owned database: %v", err)
		}
		// And a representative schema/table provisioning statement lands too.
		if err := client.Exec(ctx, `CREATE SCHEMA IF NOT EXISTS analytics`); err != nil {
			t.Fatalf("CREATE SCHEMA as the non-superuser admin failed; provisioning cannot write the data database: %v", err)
		}

		// Confirm provisioning landed in the engine-owned data database, not the DSN's
		// default 'postgres' database: the iris and analytics schemas exist in 'data'.
		dataConn, err := pgx.Connect(ctx, fmt.Sprintf("postgres://%s:%s@localhost:%d/%s?sslmode=disable", admin, adminpw, port, pg.DataDatabase))
		if err != nil {
			t.Fatalf("connect to the engine-owned data database %q: %v", pg.DataDatabase, err)
		}
		defer func() { _ = dataConn.Close(ctx) }()
		var n int
		if err := dataConn.QueryRow(ctx,
			"SELECT count(*) FROM information_schema.schemata WHERE schema_name IN ('iris', 'analytics')").Scan(&n); err != nil {
			t.Fatalf("read schemata in the data database: %v", err)
		}
		if n != 2 {
			t.Errorf("data database %q has %d of the provisioned schemas (iris, analytics), want 2; the pool did not target it", pg.DataDatabase, n)
		}
	})
}

// freePort returns a currently-free TCP port for the throwaway cluster to bind, typed
// as the uint32 embedded-postgres's Port expects. The brief close-then-reuse window is
// acceptable for a single local test process. A TCP port is always in [1, 65535]; the
// explicit bounds check makes the narrowing conversion provably safe (and guards a
// hypothetical port 0 the ephemeral bind should never return).
func freePort(t *testing.T) uint32 {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("reserve a free port: %v", err)
	}
	defer func() { _ = l.Close() }()
	port := l.Addr().(*net.TCPAddr).Port
	if port < 1 || port > 65535 {
		t.Fatalf("reserved TCP port %d is out of range [1, 65535]", port)
	}
	//nolint:gosec // G115: a net.Listen TCP port is always in [0,65535], and the bounds
	// check above proves port is in [1,65535], well within uint32; gosec's flow analysis
	// does not recognize the guard, so the conversion is annotated at the call site.
	return uint32(port)
}
