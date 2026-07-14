//go:build conformance

package conformance

import (
	"fmt"
	"net"
	"os"
	"sync"
	"testing"
	"time"

	embeddedpostgres "github.com/fergusstrange/embedded-postgres"
)

// This file makes the conformance suite self-sufficient: the legs that need a
// SHARED external Postgres cluster (two daemons on one meta, failover handover,
// weak-role preflights) no longer skip without IRIS_PG_DSN -- the suite boots
// its own embedded Postgres once per run, the same lean pinned binary the
// engine's managed mode ships, and exports its DSN so the tests and every child
// iris binary see it exactly as they would CI's cluster. An explicitly set
// IRIS_PG_DSN still wins untouched (the CI path), so this is a local-DX
// fallback, never a behaviour fork: no Docker, no system Postgres, one
// `go test -tags conformance` on a clean machine.

var (
	clusterOnce sync.Once
	cluster     *embeddedpostgres.EmbeddedPostgres
	clusterDSN  string
	clusterErr  error
)

// requireSharedCluster returns the shared external-cluster DSN, booting the
// suite-owned embedded Postgres on first need when IRIS_PG_DSN is not set. The
// DSN is also exported as IRIS_PG_DSN, so freshDatabases, metaDSN/dataDSN, and
// the iris child processes all resolve the same cluster with no further plumbing.
func requireSharedCluster(t *testing.T) string {
	t.Helper()
	clusterOnce.Do(bootSharedCluster)
	if clusterErr != nil {
		t.Fatalf("boot suite-shared embedded Postgres: %v", clusterErr)
	}
	return clusterDSN
}

// bootSharedCluster resolves the shared cluster exactly once: the ambient
// IRIS_PG_DSN when set, else a fresh embedded Postgres (superuser role, so the
// admin can mint the weak roles the preflight legs need).
func bootSharedCluster() {
	if ext := os.Getenv("IRIS_PG_DSN"); ext != "" {
		clusterDSN = ext
		return
	}

	port, err := freeClusterPort()
	if err != nil {
		clusterErr = fmt.Errorf("pick port: %w", err)
		return
	}
	dataDir, err := os.MkdirTemp("", "iris-conf-pg-data-*")
	if err != nil {
		clusterErr = fmt.Errorf("data dir: %w", err)
		return
	}
	runtimeDir, err := os.MkdirTemp("", "iris-conf-pg-rt-*")
	if err != nil {
		clusterErr = fmt.Errorf("runtime dir: %w", err)
		return
	}

	const superuser, superpw = "postgres", "conformance-pw" //nolint:gosec // G101: throwaway credential for the suite-owned local test cluster.
	pg := embeddedpostgres.NewDatabase(embeddedpostgres.DefaultConfig().
		Version(embeddedpostgres.V18).
		Username(superuser).Password(superpw).Database("postgres").
		Port(port).
		DataPath(dataDir).RuntimePath(runtimeDir).
		StartTimeout(120 * time.Second))
	if err := pg.Start(); err != nil {
		clusterErr = fmt.Errorf("start embedded Postgres: %w", err)
		return
	}
	cluster = pg
	clusterDSN = fmt.Sprintf("postgres://%s:%s@localhost:%d/postgres?sslmode=disable", superuser, superpw, port)
	// Export the DSN so every existing env read -- the helpers here and the iris
	// binaries the tests spawn -- targets the suite cluster with no plumbing.
	if err := os.Setenv("IRIS_PG_DSN", clusterDSN); err != nil {
		clusterErr = fmt.Errorf("export IRIS_PG_DSN: %w", err)
	}
}

// freeClusterPort picks a free TCP port for the suite cluster (freePort needs a
// *testing.T; the boot runs outside any one test).
func freeClusterPort() (uint32, error) {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return 0, err
	}
	defer func() { _ = l.Close() }()
	return uint32(l.Addr().(*net.TCPAddr).Port), nil //nolint:gosec // G115: a TCP port always fits.
}

// TestMain tears the suite-owned cluster down after the run; a suite riding an
// ambient IRIS_PG_DSN booted nothing and stops nothing.
func TestMain(m *testing.M) {
	code := m.Run()
	if cluster != nil {
		_ = cluster.Stop()
	}
	os.Exit(code)
}
