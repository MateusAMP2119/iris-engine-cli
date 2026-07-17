//go:build conformance

package conformance

import (
	"context"
	"fmt"
	"path/filepath"
	"strings"
	"testing"
	"time"

	embeddedpostgres "github.com/fergusstrange/embedded-postgres"
	"github.com/jackc/pgx/v5"

	"github.com/MateusAMP2119/iris-lakehouse/internal/pg"
)

// TestCaptureEmission is the end-to-end proof that the always-on write-capture
// triggers actually capture. It stands up a real Postgres cluster the engine has
// never touched, provisions the partitioned journal and the
// real iris.capture() function through the live pg path, declares a user table with
// the three per-operation capture triggers, then connects AS a dedicated writer role
// with a run id riding its session and drives multi-row INSERT / UPDATE / DELETE
// statements. It asserts, against the live journal:
//
//   - a 10-row INSERT lands exactly 10 journal rows, op=insert, pre_image null,
//     attributed to the writer role and the session's run id, undo=open;
//   - a 10-row UPDATE lands 10 rows, op=update, each carrying the full prior row as
//     its pre_image;
//   - a 10-row DELETE lands 10 rows, op=delete, each carrying the deleted row's
//     pre_image;
//   - and, via pg_stat_user_functions, each 10-row statement fired iris.capture()
//     exactly ONCE -- statement-level, not once per row (a 10-row load = one trigger,
//     not 10).
//
// It drives the pg journal/capture DDL directly against the live cluster (the
// external_data_db conformance pattern) rather than the CLI, so the leg proves the
// exact DDL the engine issues, enforced by a real Postgres. The managed
// embedded-postgres runtime is cached after the first run; the leg reports its own
// wall time.
func TestCaptureEmission(t *testing.T) {
	start := time.Now()
	t.Cleanup(func() { t.Logf("capture emission conformance leg: %s", time.Since(start).Round(time.Millisecond)) })

	const (
		superuser = "postgres"
		superpw   = "superpw"
		writer    = "iris_capture_writer"
		writerpw  = "writer_pw"
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

	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()

	dsnTo := func(db, user, pw string) string {
		return fmt.Sprintf("postgres://%s:%s@localhost:%d/%s?sslmode=disable", user, pw, port, db)
	}

	// The data-database admin client, exactly as the engine opens it.
	client, err := pg.Connect(ctx, testConnSource{dsn: dsnTo("postgres", superuser, superpw)})
	if err != nil {
		t.Fatalf("pg.Connect (data database): %v", err)
	}
	t.Cleanup(client.Close)

	// Provision capture: the journal (parent, index, open tail partition, select
	// grant) and the real iris.capture() function -- exactly what provisioning ends
	// by ensuring.
	if err := pg.EnsureJournal(ctx, client); err != nil {
		t.Fatalf("EnsureJournal: %v", err)
	}
	if err := client.EnsureCaptureFunction(ctx); err != nil {
		t.Fatalf("EnsureCaptureFunction: %v", err)
	}

	// A declared user table and its three per-operation capture triggers.
	for _, stmt := range []string{
		`CREATE SCHEMA IF NOT EXISTS analytics`,
		`CREATE TABLE analytics.orders (id integer PRIMARY KEY, amount numeric NOT NULL)`,
	} {
		if err := client.Exec(ctx, stmt); err != nil {
			t.Fatalf("create user table (%q): %v", stmt, err)
		}
	}
	for _, trig := range pg.RenderCaptureTriggers("analytics", "orders") {
		if err := client.Exec(ctx, trig); err != nil {
			t.Fatalf("install capture trigger: %v\n%s", err, trig)
		}
	}

	// Track function calls at the database level so every new session (the writer's)
	// records iris.capture() invocation counts, the statement-level proof.
	if err := client.Exec(ctx, "ALTER DATABASE "+pg.DataDatabase+" SET track_functions = 'all'"); err != nil {
		t.Fatalf("enable function-call tracking: %v", err)
	}

	// Mint the dedicated writer role and grant it DML on its OWN table. The
	// iris-schema USAGE + capture EXECUTE that let its write reach the always-on
	// capture function are no longer hand-written here: they ride the production
	// provisioning path (pg.RenderCaptureReachabilityGrants, the exact grants
	// ProvisionPipelineRole issues for every pipeline role). If provisioning stopped
	// granting them, the capture assertions below would stop firing -- that is the
	// real proof the pipeline-role provisioning covers capture out of the box (the
	// function is SECURITY DEFINER, so the journal write itself runs as the owner).
	grantStmts := []string{
		fmt.Sprintf("CREATE ROLE %s LOGIN NOSUPERUSER NOCREATEDB NOCREATEROLE PASSWORD '%s'", writer, writerpw),
		fmt.Sprintf("GRANT CONNECT ON DATABASE %s TO %s", pg.DataDatabase, writer),
		fmt.Sprintf("GRANT USAGE ON SCHEMA analytics TO %s", writer),
		fmt.Sprintf("GRANT SELECT, INSERT, UPDATE, DELETE ON analytics.orders TO %s", writer),
	}
	grantStmts = append(grantStmts, pg.RenderCaptureReachabilityGrants(writer)...)
	for _, stmt := range grantStmts {
		if err := client.Exec(ctx, stmt); err != nil {
			t.Fatalf("mint writer role (%q): %v", stmt, err)
		}
	}

	// A read connection (superuser) for journal assertions and function-stat reads.
	adminConn, err := pgx.Connect(ctx, dsnTo(pg.DataDatabase, superuser, superpw))
	if err != nil {
		t.Fatalf("connect admin read conn: %v", err)
	}
	defer func() { _ = adminConn.Close(ctx) }()

	// The writer connection: a run id rides its session (the injected-connection
	// contract RunIDSetting names; here the trigger reads it in-transaction).
	writerConn, err := pgx.Connect(ctx, dsnTo(pg.DataDatabase, writer, writerpw))
	if err != nil {
		t.Fatalf("connect as writer role: %v", err)
	}
	defer func() { _ = writerConn.Close(ctx) }()

	const (
		insertRun int64 = 4242
		updateRun int64 = 5252
		deleteRun int64 = 6262
		rowCount  int64 = 10
	)

	// --- INSERT: one statement, 10 rows -> 10 stamps, op=insert, no pre-image. ---
	setRun(ctx, t, writerConn, insertRun)
	resetFunctionStats(ctx, t, adminConn)
	execWrite(ctx, t, writerConn, "INSERT INTO analytics.orders (id, amount) SELECT g, g * 10 FROM generate_series(1, 10) AS g")
	forceStatFlush(ctx, t, writerConn)

	assertCount(ctx, t, adminConn, rowCount,
		"SELECT count(*) FROM public.data_journal WHERE run_id=$1", insertRun)
	assertCount(ctx, t, adminConn, rowCount,
		"SELECT count(*) FROM public.data_journal WHERE run_id=$1 AND op='insert' AND pg_role=$2 AND undo='open' AND pre_image IS NULL",
		insertRun, writer)
	assertCount(ctx, t, adminConn, rowCount,
		"SELECT count(DISTINCT row_pk) FROM public.data_journal WHERE run_id=$1 AND row_pk::int BETWEEN 1 AND 10", insertRun)
	assertCaptureFiredOnce(ctx, t, adminConn, "10-row INSERT")

	// --- UPDATE: one statement, 10 rows -> 10 stamps, op=update, full pre-image. ---
	setRun(ctx, t, writerConn, updateRun)
	resetFunctionStats(ctx, t, adminConn)
	execWrite(ctx, t, writerConn, "UPDATE analytics.orders SET amount = amount + 1")
	forceStatFlush(ctx, t, writerConn)

	assertCount(ctx, t, adminConn, rowCount,
		"SELECT count(*) FROM public.data_journal WHERE run_id=$1 AND op='update' AND pre_image IS NOT NULL", updateRun)
	// The pre-image is the prior row: row 5's amount was 50 before the +1 update.
	assertPreImageContains(ctx, t, adminConn, updateRun, "5", `"amount":50`)
	assertCaptureFiredOnce(ctx, t, adminConn, "10-row UPDATE")

	// --- DELETE: one statement, 10 rows -> 10 stamps, op=delete, full pre-image. ---
	setRun(ctx, t, writerConn, deleteRun)
	resetFunctionStats(ctx, t, adminConn)
	execWrite(ctx, t, writerConn, "DELETE FROM analytics.orders")
	forceStatFlush(ctx, t, writerConn)

	assertCount(ctx, t, adminConn, rowCount,
		"SELECT count(*) FROM public.data_journal WHERE run_id=$1 AND op='delete' AND pre_image IS NOT NULL", deleteRun)
	// Row 5 was updated to 51, so its deleted pre-image carries the post-update value.
	assertPreImageContains(ctx, t, adminConn, deleteRun, "5", `"amount":51`)
	assertCaptureFiredOnce(ctx, t, adminConn, "10-row DELETE")
}

// setRun sets the per-session run id the capture trigger reads in-transaction.
func setRun(ctx context.Context, t *testing.T, conn *pgx.Conn, run int64) {
	t.Helper()
	if _, err := conn.Exec(ctx, fmt.Sprintf("SET iris.run_id = '%d'", run)); err != nil {
		t.Fatalf("set run id %d on the writer session: %v", run, err)
	}
}

// exec runs one statement on conn, failing on error.
func execWrite(ctx context.Context, t *testing.T, conn *pgx.Conn, sql string) {
	t.Helper()
	if _, err := conn.Exec(ctx, sql); err != nil {
		t.Fatalf("exec %q: %v", sql, err)
	}
}

// resetFunctionStats zeroes the database's cumulative statistics so the next flushed
// iris.capture() call count reflects only the statement under test.
func resetFunctionStats(ctx context.Context, t *testing.T, conn *pgx.Conn) {
	t.Helper()
	if _, err := conn.Exec(ctx, "SELECT pg_stat_reset()"); err != nil {
		t.Fatalf("reset function stats: %v", err)
	}
}

// forceStatFlush forces the writer backend to flush its pending cumulative stats
// immediately rather than waiting out the throttle interval, so the assertion reads a
// current count without a fixed sleep.
func forceStatFlush(ctx context.Context, t *testing.T, conn *pgx.Conn) {
	t.Helper()
	if _, err := conn.Exec(ctx, "SELECT pg_stat_force_next_flush()"); err != nil {
		t.Fatalf("force stat flush: %v", err)
	}
}

// assertCaptureFiredOnce proves the last statement fired iris.capture() exactly once
// (statement-level), never once per row. It polls the flushed function stats to a
// bounded deadline rather than sleeping a fixed interval.
func assertCaptureFiredOnce(ctx context.Context, t *testing.T, conn *pgx.Conn, what string) {
	t.Helper()
	const q = "SELECT COALESCE(sum(calls), 0) FROM pg_stat_user_functions WHERE schemaname='iris' AND funcname='capture'"
	deadline := time.Now().Add(5 * time.Second)
	var calls int64
	for {
		if err := conn.QueryRow(ctx, q).Scan(&calls); err != nil {
			t.Fatalf("read iris.capture() call count: %v", err)
		}
		if calls > 0 || time.Now().After(deadline) {
			break
		}
		time.Sleep(25 * time.Millisecond)
	}
	if calls != 1 {
		t.Errorf("%s fired iris.capture() %d times, want exactly 1 (statement-level, not once per row)", what, calls)
	}
}

// assertCount fails unless the scalar count query returns want.
func assertCount(ctx context.Context, t *testing.T, conn *pgx.Conn, want int64, sql string, args ...any) {
	t.Helper()
	var got int64
	if err := conn.QueryRow(ctx, sql, args...).Scan(&got); err != nil {
		t.Fatalf("count query %q: %v", sql, err)
	}
	if got != want {
		t.Errorf("count = %d, want %d\n  query: %s\n  args: %v", got, want, sql, args)
	}
}

// assertPreImageContains fails unless the pre_image of the (run, row_pk) stamp
// contains sub, proving the journal carries the full prior row.
func assertPreImageContains(ctx context.Context, t *testing.T, conn *pgx.Conn, run int64, rowPK, sub string) {
	t.Helper()
	var pre string
	err := conn.QueryRow(ctx,
		"SELECT pre_image::text FROM public.data_journal WHERE run_id=$1 AND row_pk=$2", run, rowPK).Scan(&pre)
	if err != nil {
		t.Fatalf("read pre_image for run %d row %s: %v", run, rowPK, err)
	}
	if !strings.Contains(pre, sub) {
		t.Errorf("pre_image for run %d row %s = %q, want it to contain %q", run, rowPK, pre, sub)
	}
}
