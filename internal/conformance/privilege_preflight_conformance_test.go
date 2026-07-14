//go:build conformance

package conformance

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
)

// This file is the admin-DSN privilege preflight leg: `iris engine install`
// against a role missing a required grant fails fast with the grant NAMED,
// before any database is created -- instead of reporting success and letting
// `iris engine start` die later on a raw Postgres permission error.

// TestInstallPreflightsAdminPrivileges mints a CREATEDB-only role (no
// CREATEROLE) on the shared external cluster and proves install refuses it,
// naming the missing grant, and creates nothing.
func TestInstallPreflightsAdminPrivileges(t *testing.T) {
	// The weak role is minted on the shared external cluster: the suite-owned
	// embedded one, or an ambient IRIS_PG_DSN (managed mode boots as the
	// superuser, which always passes the preflight).
	ext := requireSharedCluster(t)
	freshDatabases(t)
	bin := Build(t)
	ws := shortWorkspace(t)

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	admin, err := pgx.Connect(ctx, ext)
	if err != nil {
		t.Fatalf("connect admin cluster: %v", err)
	}
	defer func() { _ = admin.Close(ctx) }()

	// A role with CREATEDB but not CREATEROLE: enough for install's own CREATE
	// DATABASE statements, so before the preflight this DSN installed cleanly and
	// only died at daemon start (read-pool role mint).
	const weak = "iris_weak_admin"
	_, _ = admin.Exec(ctx, "DROP ROLE IF EXISTS "+weak)
	if _, err := admin.Exec(ctx, fmt.Sprintf("CREATE ROLE %s LOGIN CREATEDB PASSWORD 'weakpw'", weak)); err != nil {
		t.Fatalf("mint CREATEDB-only role: %v", err)
	}
	t.Cleanup(func() {
		cctx, ccancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer ccancel()
		conn, cerr := pgx.Connect(cctx, ext)
		if cerr != nil {
			return
		}
		defer func() { _ = conn.Close(cctx) }()
		_, _ = conn.Exec(cctx, "DROP ROLE IF EXISTS "+weak)
	})

	cfg, err := pgx.ParseConfig(ext)
	if err != nil {
		t.Fatalf("parse IRIS_PG_DSN: %v", err)
	}
	weakDSN := fmt.Sprintf("postgres://%s:weakpw@%s:%d/%s?sslmode=disable", weak, cfg.Host, cfg.Port, cfg.Database)

	res := bin.Run(t, RunOptions{
		Args:    []string{"engine", "install"},
		Env:     []string{"IRIS_PG_DSN=" + weakDSN},
		Dir:     ws,
		Timeout: 2 * time.Minute,
	})
	if res.ExitCode == 0 {
		t.Fatalf("install succeeded against a CREATEROLE-less admin DSN; the preflight must refuse\nstdout:\n%s\nstderr:\n%s", res.Stdout, res.Stderr)
	}
	combined := string(res.Stdout) + string(res.Stderr)
	if !strings.Contains(combined, "CREATEROLE") || !strings.Contains(combined, weak) {
		t.Errorf("refusal must name the role and the missing CREATEROLE grant:\n%s", combined)
	}
	// Nothing was created: the preflight ran before the first CREATE DATABASE.
	var n int
	if err := admin.QueryRow(ctx, "SELECT count(*) FROM pg_database WHERE datname IN ('meta', 'data')").Scan(&n); err != nil {
		t.Fatalf("count engine databases: %v", err)
	}
	if n != 0 {
		t.Errorf("a refused install still created %d engine database(s); the preflight must run before CREATE DATABASE", n)
	}
}
