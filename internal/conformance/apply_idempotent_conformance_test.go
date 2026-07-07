//go:build conformance

package conformance

import (
	"context"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/MateusAMP2119/iris-engine-cli/internal/daemon"
	"github.com/MateusAMP2119/iris-engine-cli/internal/fixtures"
	"github.com/MateusAMP2119/iris-engine-cli/internal/pg"
)

// TestApplyRepeatNoop drives the real iris binary end to end against a running daemon
// and real Postgres, and proves iris declare apply is idempotent: repeating an apply
// -- including its schema provisioning -- changes nothing (specification section 13,
// definition of done). It installs the engine, starts a detached daemon over the
// golden sample workspace, applies the ingest composer and its three pipelines
// (upstream-first), snapshots the persisted registry (meta) and the provisioned data
// catalog, re-applies every declaration, and asserts both snapshots are byte-identical:
// the registry upsert is a no-op and provisioning re-emits no schema change.
//
// spec: S13/apply-repeat-noop
func TestApplyRepeatNoop(t *testing.T) {
	t.Run("S13/apply-repeat-noop", func(t *testing.T) {
		start := time.Now()
		bin := Build(t)
		ws := shortWorkspace(t)
		copyGoldenWorkspace(t, ws)
		socket := filepath.Join(ws, ".iris", "iris.sock")

		// Install (external: no-op under IRIS_PG_DSN; managed: cached download), then
		// start the daemon detached so it connects to real Postgres, elects, and can
		// resolve declarations and the schemas/ tree against this workspace.
		bin.Run(t, RunOptions{Args: []string{"engine", "install"}, Dir: ws, Timeout: 5 * time.Minute}).RequireExit(t, 0)
		bin.Run(t, RunOptions{Args: []string{"engine", "start", "-d"}, Dir: ws, Timeout: 2 * time.Minute}).RequireExit(t, 0)
		t.Cleanup(func() {
			bin.Run(t, RunOptions{Args: []string{"engine", "stop"}, Dir: ws, Timeout: 30 * time.Second})
		})

		readyCtx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
		if err := WaitForSocket(readyCtx, socket); err != nil {
			cancel()
			t.Fatalf("daemon socket never became ready: %v", err)
		}
		cancel()
		if !waitForLeader(t, socket) {
			t.Fatal("daemon never became leader; cannot apply against the single writer")
		}

		// The apply order registers the graph upstream-first (specification section 13,
		// step 2): the ingest composer first, then extract_orders (no depends_on), then
		// reset_counters (composer-ordered only), then load_orders (depends_on
		// extract_orders). Schema provisioning rides each apply.
		targets := []string{
			"pipelines/ingest",
			"pipelines/ingest/extract_orders",
			"pipelines/ingest/reset_counters",
			"pipelines/ingest/load_orders",
		}
		applyAll := func(pass string) {
			for _, tgt := range targets {
				res := bin.Run(t, RunOptions{Args: []string{"declare", "apply", tgt}, Dir: ws})
				res.RequireExit(t, 0)
			}
			_ = pass
		}

		// First apply: register the graph and provision the schemas/ tree.
		applyAll("first")
		metaBefore := snapshotMeta(t, ws)
		catalogBefore := snapshotCatalog(t, ws)

		// Re-apply every declaration: idempotent, including schema provisioning.
		applyAll("repeat")
		metaAfter := snapshotMeta(t, ws)
		catalogAfter := snapshotCatalog(t, ws)

		if metaBefore != metaAfter {
			t.Errorf("re-apply changed the persisted registry (apply not idempotent):\n--- before ---\n%s\n--- after ---\n%s", metaBefore, metaAfter)
		}
		if catalogBefore != catalogAfter {
			t.Errorf("re-apply changed the provisioned data catalog (schema provisioning not idempotent):\n--- before ---\n%s\n--- after ---\n%s", catalogBefore, catalogAfter)
		}

		// Guard against a vacuous pass: the first apply must have actually registered
		// the four pipelines and provisioned the two tables, so "unchanged" means
		// "unchanged from a real applied state".
		if !strings.Contains(metaBefore, "load_orders") || !strings.Contains(metaBefore, "extract_orders") {
			t.Errorf("registry snapshot does not carry the applied pipelines; the apply did not register the graph:\n%s", metaBefore)
		}
		if !strings.Contains(catalogBefore, "analytics | orders") || !strings.Contains(catalogBefore, "raw | orders_staging") {
			t.Errorf("catalog snapshot does not carry the provisioned tables; provisioning did not run:\n%s", catalogBefore)
		}
		if !strings.Contains(catalogBefore, "iris_capture_ins_analytics_orders") {
			t.Errorf("catalog snapshot does not carry the installed capture triggers; provisioning did not install them:\n%s", catalogBefore)
		}

		t.Logf("S13/apply-repeat-noop runtime: %s", time.Since(start).Round(time.Millisecond))
	})
}

// copyGoldenWorkspace copies the golden sample workspace's pipelines/ and schemas/
// trees into ws, so the daemon (running with ws as its workspace) resolves the same
// declarations and tables the fixtures ship, in a writable throwaway location its
// .iris state can live under.
func copyGoldenWorkspace(t *testing.T, ws string) {
	t.Helper()
	golden := fixtures.WorkspaceGolden()
	for _, sub := range []string{"pipelines", "schemas"} {
		if err := copyTree(filepath.Join(golden, sub), filepath.Join(ws, sub)); err != nil {
			t.Fatalf("copy golden %s into workspace: %v", sub, err)
		}
	}
}

// copyTree recursively copies the directory src to dst, preserving the traversable
// 0755 dirs / 0644 files a workspace tree carries.
func copyTree(src, dst string) error {
	return filepath.WalkDir(src, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(src, path)
		if err != nil {
			return err
		}
		target := filepath.Join(dst, rel)
		if d.IsDir() {
			return os.MkdirAll(target, 0o755)
		}
		data, err := os.ReadFile(path) //nolint:gosec // G304: fixture path under the repo, test input.
		if err != nil {
			return err
		}
		if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
			return err
		}
		return os.WriteFile(target, data, 0o644) //nolint:gosec // G306: workspace file, world-readable by design.
	})
}

// snapshotMeta returns a deterministic dump of the persisted registry in the meta
// database: the pipelines, dependencies, lanes, and applied-migration ledger rows the
// apply writes. Two applies of the same declarations must produce the identical dump.
func snapshotMeta(t *testing.T, ws string) string {
	t.Helper()
	conn := connectPG(t, metaDSN(t, ws))
	defer func() { _ = conn.Close(context.Background()) }()

	var b strings.Builder
	dumpQuery(t, &b, conn, "pipelines",
		`SELECT name, folder, run::text, artifact, data_mode FROM pipelines ORDER BY name`)
	dumpQuery(t, &b, conn, "dependencies",
		`SELECT from_pipeline, to_pipeline FROM dependencies ORDER BY from_pipeline, to_pipeline`)
	dumpQuery(t, &b, conn, "lanes",
		`SELECT lane, pipeline, pos FROM lanes ORDER BY lane, pos`)
	dumpQuery(t, &b, conn, "migrations",
		`SELECT "schema", "table", migration_id, coalesce(parent, ''), checksum FROM migrations ORDER BY "schema", "table", migration_id`)
	return b.String()
}

// snapshotCatalog returns a deterministic dump of the provisioned data catalog: the
// declared tables' columns, the installed capture triggers, and the journal presence.
// Idempotent provisioning re-emits no schema change, so two applies produce the
// identical dump.
func snapshotCatalog(t *testing.T, ws string) string {
	t.Helper()
	conn := connectPG(t, dataDSN(t, ws))
	defer func() { _ = conn.Close(context.Background()) }()

	var b strings.Builder
	dumpQuery(t, &b, conn, "columns",
		`SELECT table_schema, table_name, column_name, data_type
		 FROM information_schema.columns
		 WHERE table_schema IN ('raw', 'analytics')
		 ORDER BY table_schema, table_name, ordinal_position`)
	dumpQuery(t, &b, conn, "capture_triggers",
		`SELECT n.nspname, c.relname, t.tgname
		 FROM pg_trigger t
		 JOIN pg_class c ON c.oid = t.tgrelid
		 JOIN pg_namespace n ON n.oid = c.relnamespace
		 WHERE NOT t.tgisinternal AND t.tgname LIKE 'iris_capture\_%'
		 ORDER BY n.nspname, c.relname, t.tgname`)
	dumpQuery(t, &b, conn, "journal",
		`SELECT count(*) FROM pg_class c JOIN pg_namespace n ON n.oid = c.relnamespace
		 WHERE n.nspname = 'public' AND c.relname = 'data_journal'`)
	return b.String()
}

// dumpQuery appends a labeled, row-per-line rendering of the query result to b. Every
// column is rendered with %v in selected order, so the dump is deterministic and
// diffable across two applies.
func dumpQuery(t *testing.T, b *strings.Builder, conn *pgx.Conn, label, sql string) {
	t.Helper()
	rows, err := conn.Query(context.Background(), sql)
	if err != nil {
		t.Fatalf("snapshot %s query: %v", label, err)
	}
	defer rows.Close()
	fmt.Fprintf(b, "[%s]\n", label)
	for rows.Next() {
		vals, err := rows.Values()
		if err != nil {
			t.Fatalf("snapshot %s values: %v", label, err)
		}
		parts := make([]string, len(vals))
		for i, v := range vals {
			parts[i] = fmt.Sprintf("%v", v)
		}
		fmt.Fprintf(b, "  %s\n", strings.Join(parts, " | "))
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("snapshot %s rows: %v", label, err)
	}
}

// connectPG opens an independent pgx connection to dsn, failing the test on error.
func connectPG(t *testing.T, dsn string) *pgx.Conn {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	conn, err := pgx.Connect(ctx, dsn)
	if err != nil {
		t.Fatalf("connect Postgres %q: %v", dsn, err)
	}
	return conn
}

// dataDSN returns a connection string an independent client uses to read the engine's
// data database (the schemas and journal the provisioner writes), distinct from metaDSN
// which targets the meta control database. Both modes target the engine-created data
// database (pg.DataDatabase), not the admin DSN's own database: external mode points
// IRIS_PG_DSN's connection at it; managed mode reconstructs the local managed-Postgres
// DSN to it.
func dataDSN(t *testing.T, ws string) string {
	t.Helper()
	if ext := os.Getenv("IRIS_PG_DSN"); ext != "" {
		cfg, err := pgx.ParseConfig(ext)
		if err != nil {
			t.Fatalf("parse IRIS_PG_DSN: %v", err)
		}
		cfg.Database = pg.DataDatabase
		return pgxConnString(cfg)
	}
	pgDir := filepath.Join(ws, ".iris", "pg")
	port := readPostmasterPort(t, filepath.Join(pgDir, "data", "postmaster.pid"))
	pwBytes, err := os.ReadFile(filepath.Join(pgDir, "superuser.pw")) //nolint:gosec // G304: engine-owned managed credential under the test's own workspace.
	if err != nil {
		t.Fatalf("read managed superuser credential: %v", err)
	}
	pw := strings.TrimSpace(string(pwBytes))
	return fmt.Sprintf("postgres://%s:%s@localhost:%s/%s?sslmode=disable",
		daemon.ManagedSuperuser, pw, port, pg.DataDatabase)
}
