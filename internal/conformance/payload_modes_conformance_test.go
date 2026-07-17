//go:build conformance

package conformance

import (
	"context"
	"fmt"
	"path/filepath"
	"testing"
	"time"

	embeddedpostgres "github.com/fergusstrange/embedded-postgres"
	"github.com/jackc/pgx/v5"

	"github.com/MateusAMP2119/iris-lakehouse/internal/pg"
)

// TestPayloadTiersAndModes is the end-to-end proof that capture is unconditional
// across data modes while the payload TIER is mode-aware: a full pre-image only where
// undo can spend it, a slim stamp everywhere else. It stands up one real Postgres
// cluster the engine has never touched, provisions
// the partitioned journal and the real iris.capture() function through the live pg
// path, declares a user table with the three per-operation capture triggers, and mints
// a dedicated pipeline writer role. Each subtest drives writes AS that role over a
// connection carrying a run id (via pg.InjectRunID, the harness's attributed form)
// and the write's wipe-eligibility (the per-session iris.wipe_eligible setting the
// trigger reads in-transaction), then asserts the live journal.
//
// The three legs, one per subtest:
//
//   - capture-regardless-of-mode: a disposable (wipe-eligible) run and a permanent
//     (born-promoted) run each land exactly one journal row per write -- capture is on
//     in both modes -- and only the tier differs (the disposable update keeps a full
//     pre-image born open; the permanent update is a slim stamp born promoted).
//   - pre-image-wipe-eligible-only: a full pre-image lands on a wipe-eligible update
//     and delete and is null on inserts; a permanent-mode update/delete born promoted
//     is a slim stamp (null pre-image) too.
//   - preimage-only-where-undo: journal-wide, every row carrying a pre-image is an open
//     (wipe-eligible) update or delete -- exactly where undo can spend it -- and never a
//     permanent write; a permanent write never records a row copy.
//
// It drives the pg journal/capture DDL directly against the live cluster (the
// external_data_db conformance pattern) rather than the CLI, so the leg proves the
// exact behavior a real Postgres enforces. The managed embedded-postgres runtime is
// cached after the first run; the leg reports its own wall time.
func TestPayloadTiersAndModes(t *testing.T) {
	start := time.Now()
	t.Cleanup(func() {
		t.Logf("payload tiers and modes conformance leg: %s", time.Since(start).Round(time.Millisecond))
	})

	const (
		superuser = "postgres"
		superpw   = "superpw"
		writer    = "iris_payload_writer"
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

	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()

	dsnTo := func(db, user, pw string) string {
		return fmt.Sprintf("postgres://%s:%s@localhost:%d/%s?sslmode=disable", user, pw, port, db)
	}

	// The data-database admin client, exactly as the engine opens it: provision the
	// journal and the real iris.capture() function, then the user table and its
	// capture triggers.
	client, err := pg.Connect(ctx, testConnSource{dsn: dsnTo("postgres", superuser, superpw)})
	if err != nil {
		t.Fatalf("pg.Connect (data database): %v", err)
	}
	t.Cleanup(client.Close)

	if err := pg.EnsureJournal(ctx, client); err != nil {
		t.Fatalf("EnsureJournal: %v", err)
	}
	if err := client.EnsureCaptureFunction(ctx); err != nil {
		t.Fatalf("EnsureCaptureFunction: %v", err)
	}
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

	// Mint the pipeline writer role with exactly what a pipeline role needs: DML on
	// its table plus USAGE/EXECUTE to reach the SECURITY DEFINER capture function.
	for _, stmt := range []string{
		fmt.Sprintf("CREATE ROLE %s LOGIN NOSUPERUSER NOCREATEDB NOCREATEROLE PASSWORD '%s'", writer, writerpw),
		fmt.Sprintf("GRANT CONNECT ON DATABASE %s TO %s", pg.DataDatabase, writer),
		fmt.Sprintf("GRANT USAGE ON SCHEMA analytics TO %s", writer),
		fmt.Sprintf("GRANT SELECT, INSERT, UPDATE, DELETE ON analytics.orders TO %s", writer),
		fmt.Sprintf("GRANT USAGE ON SCHEMA iris TO %s", writer),
		fmt.Sprintf("GRANT EXECUTE ON FUNCTION iris.capture() TO %s", writer),
	} {
		if err := client.Exec(ctx, stmt); err != nil {
			t.Fatalf("mint writer role (%q): %v", stmt, err)
		}
	}

	// A superuser read connection for journal assertions.
	adminConn, err := pgx.Connect(ctx, dsnTo(pg.DataDatabase, superuser, superpw))
	if err != nil {
		t.Fatalf("connect admin read conn: %v", err)
	}
	t.Cleanup(func() { _ = adminConn.Close(ctx) })

	writerBaseDSN := dsnTo(pg.DataDatabase, writer, writerpw)

	// connFor opens a fresh writer connection carrying runID as the per-session
	// iris.run_id setting (via pg.InjectRunID, the harness's attributed form) plus
	// the write's wipe-eligibility as iris.wipe_eligible -- exactly the two settings the
	// engine injects on a run's data connection at spawn. A disposable run is
	// wipe-eligible; a permanent run is not (its writes are born promoted).
	connFor := func(t *testing.T, runID int64, wipeEligible bool) *pgx.Conn {
		t.Helper()
		conn, err := pgx.Connect(ctx, pg.InjectRunID(writerBaseDSN, runID))
		if err != nil {
			t.Fatalf("connect writer with run id %d: %v", runID, err)
		}
		t.Cleanup(func() { _ = conn.Close(ctx) })
		val := "off"
		if wipeEligible {
			val = "on"
		}
		if _, err := conn.Exec(ctx, fmt.Sprintf("SET %s = '%s'", pg.WipeEligibleSetting, val)); err != nil {
			t.Fatalf("set %s=%s on run %d: %v", pg.WipeEligibleSetting, val, runID, err)
		}
		return conn
	}

	t.Run("capture-regardless-of-mode", func(t *testing.T) {
		// A disposable (wipe-eligible) run: insert then update its own row.
		const dispRun int64 = 82001
		dc := connFor(t, dispRun, true)
		execWrite(ctx, t, dc, "INSERT INTO analytics.orders (id, amount) VALUES (1000, 10)")
		execWrite(ctx, t, dc, "UPDATE analytics.orders SET amount = 11 WHERE id = 1000")

		// A permanent (born-promoted) run: insert then update its own row.
		const permRun int64 = 82002
		pc := connFor(t, permRun, false)
		execWrite(ctx, t, pc, "INSERT INTO analytics.orders (id, amount) VALUES (2000, 20)")
		execWrite(ctx, t, pc, "UPDATE analytics.orders SET amount = 21 WHERE id = 2000")

		// Every write is captured regardless of the data mode: two stamps per run.
		assertCount(ctx, t, adminConn, 2, "SELECT count(*) FROM public.data_journal WHERE run_id=$1", dispRun)
		assertCount(ctx, t, adminConn, 2, "SELECT count(*) FROM public.data_journal WHERE run_id=$1", permRun)

		// Only the payload tier differs: the disposable update carries a full pre-image
		// and is born open; the permanent update is a slim stamp born promoted.
		assertCount(ctx, t, adminConn, 1,
			"SELECT count(*) FROM public.data_journal WHERE run_id=$1 AND op='update' AND pre_image IS NOT NULL AND undo='open'", dispRun)
		assertCount(ctx, t, adminConn, 1,
			"SELECT count(*) FROM public.data_journal WHERE run_id=$1 AND op='update' AND pre_image IS NULL AND undo='promoted'", permRun)
	})

	t.Run("pre-image-wipe-eligible-only", func(t *testing.T) {
		// A prior run seeds the rows the disposable run will update and delete, so each
		// (run, row_pk) the assertions read carries exactly one stamp.
		const seedRun int64 = 82003
		sc := connFor(t, seedRun, true)
		execWrite(ctx, t, sc, "INSERT INTO analytics.orders (id, amount) VALUES (1100, 50), (1101, 60)")

		// Disposable (wipe-eligible) run: a fresh insert (no pre-image), an update of a
		// seeded row (full prior row), and a delete of a seeded row (full prior row).
		const dispRun int64 = 82013
		dc := connFor(t, dispRun, true)
		execWrite(ctx, t, dc, "INSERT INTO analytics.orders (id, amount) VALUES (1102, 70)")
		execWrite(ctx, t, dc, "UPDATE analytics.orders SET amount = amount + 1 WHERE id = 1100")
		execWrite(ctx, t, dc, "DELETE FROM analytics.orders WHERE id = 1101")

		// Even under a wipe-eligible run, an insert carries no pre-image.
		assertCount(ctx, t, adminConn, 1,
			"SELECT count(*) FROM public.data_journal WHERE run_id=$1 AND op='insert' AND pre_image IS NULL", dispRun)
		// The wipe-eligible update carries the full prior row (amount was 50).
		assertCount(ctx, t, adminConn, 1,
			"SELECT count(*) FROM public.data_journal WHERE run_id=$1 AND op='update' AND pre_image IS NOT NULL", dispRun)
		assertPreImageContains(ctx, t, adminConn, dispRun, "1100", `"amount":50`)
		// The wipe-eligible delete carries the deleted row (amount 60).
		assertCount(ctx, t, adminConn, 1,
			"SELECT count(*) FROM public.data_journal WHERE run_id=$1 AND op='delete' AND pre_image IS NOT NULL", dispRun)
		assertPreImageContains(ctx, t, adminConn, dispRun, "1101", `"amount":60`)

		// Permanent run: a write born promoted records a slim stamp for EVERY op, even
		// an update and a delete -- no pre-image anywhere, all born promoted.
		const permRun int64 = 82004
		pc := connFor(t, permRun, false)
		execWrite(ctx, t, pc, "INSERT INTO analytics.orders (id, amount) VALUES (2100, 70)")
		execWrite(ctx, t, pc, "UPDATE analytics.orders SET amount = 71 WHERE id = 2100")
		execWrite(ctx, t, pc, "DELETE FROM analytics.orders WHERE id = 2100")

		assertCount(ctx, t, adminConn, 3, "SELECT count(*) FROM public.data_journal WHERE run_id=$1", permRun)
		assertCount(ctx, t, adminConn, 0,
			"SELECT count(*) FROM public.data_journal WHERE run_id=$1 AND pre_image IS NOT NULL", permRun)
		assertCount(ctx, t, adminConn, 3,
			"SELECT count(*) FROM public.data_journal WHERE run_id=$1 AND undo='promoted'", permRun)
	})

	t.Run("preimage-only-where-undo", func(t *testing.T) {
		const (
			dispRun int64 = 82005
			permRun int64 = 82006
		)
		dc := connFor(t, dispRun, true)
		execWrite(ctx, t, dc, "INSERT INTO analytics.orders (id, amount) VALUES (1200, 80)")
		execWrite(ctx, t, dc, "UPDATE analytics.orders SET amount = 81 WHERE id = 1200")

		pc := connFor(t, permRun, false)
		execWrite(ctx, t, pc, "INSERT INTO analytics.orders (id, amount) VALUES (2200, 90)")
		execWrite(ctx, t, pc, "UPDATE analytics.orders SET amount = 91 WHERE id = 2200")

		// The core invariant: across both runs, every row that carries a pre-image is an
		// open (wipe-eligible) update or delete -- exactly where undo can spend it -- and
		// nowhere else. A slim stamp (insert or born-promoted) never carries a row copy.
		assertCount(ctx, t, adminConn, 0,
			"SELECT count(*) FROM public.data_journal WHERE run_id IN ($1,$2) AND pre_image IS NOT NULL AND NOT (undo='open' AND op IN ('update','delete'))",
			dispRun, permRun)
		// The undo budget is non-empty: the disposable update kept a spendable pre-image.
		assertCount(ctx, t, adminConn, 1,
			"SELECT count(*) FROM public.data_journal WHERE run_id=$1 AND pre_image IS NOT NULL AND undo='open' AND op='update'", dispRun)
		// The permanent run recorded no row copy at all: slim stamps only.
		assertCount(ctx, t, adminConn, 0,
			"SELECT count(*) FROM public.data_journal WHERE run_id=$1 AND pre_image IS NOT NULL", permRun)
	})
}
