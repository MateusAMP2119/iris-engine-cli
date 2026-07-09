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

	"github.com/MateusAMP2119/iris-engine-cli/internal/pg"
)

// TestPostPromotionWritesStillCaptured is the end-to-end proof of promotion's
// half of the one-store doctrine against a real Postgres (specification
// sections 1, 5 and 12): after a pipeline's data is promoted, new writes to its
// tables are permanent -- born undo='promoted', outside wipe scope -- yet still
// captured in the journal at stamp cost (slim: no pre-image, no row copy).
// Promotion never stops capture; it only changes what the stamps are born as.
//
// spec: S05/post-promotion-writes-still-captured  (conformance; also claimed here)
//
// The leg stands up one real cluster, provisions the partitioned journal and
// the real iris.capture() function through the live pg path, declares a user
// table with the three per-operation capture triggers, and mints a pipeline
// writer role (the payload_modes pattern). It then:
//
//  1. drives pre-promotion writes as a disposable (wipe-eligible) run -- the
//     stamps are born open, the update keeping a spendable pre-image;
//  2. promotes for real: the live marker-only flip (pg.ExecutePromotionFlip)
//     against the live journal, and asserts it moved nothing -- same entry
//     count, pre-images retained, table rows untouched -- only undo flipped;
//  3. drives post-promotion writes as the now-permanent pipeline (the injected
//     iris.wipe_eligible setting off, exactly what the engine injects once
//     data_mode is permanent) and asserts every write is still captured, each
//     stamp born promoted with a NULL pre_image -- permanent, at stamp cost.
//
// spec: S05/post-promotion-writes-still-captured
func TestPostPromotionWritesStillCaptured(t *testing.T) {
	start := time.Now()
	t.Cleanup(func() {
		t.Logf("post-promotion capture conformance leg: %s", time.Since(start).Round(time.Millisecond))
	})

	const (
		superuser = "postgres"
		superpw   = "superpw"
		writer    = "iris_promo_writer"
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

	// Provision the journal, the capture function, the user table with its
	// capture triggers, and the pipeline writer role -- the live pg path.
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

	adminConn, err := pgx.Connect(ctx, dsnTo(pg.DataDatabase, superuser, superpw))
	if err != nil {
		t.Fatalf("connect admin read conn: %v", err)
	}
	t.Cleanup(func() { _ = adminConn.Close(ctx) })

	writerBaseDSN := dsnTo(pg.DataDatabase, writer, writerpw)

	// connFor opens a fresh writer connection carrying the run id plus the
	// write's wipe-eligibility -- the two per-session settings the engine injects
	// on a run's data connection at spawn. Pre-promotion the pipeline is
	// disposable (eligible on); post-promotion it is permanent (off).
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

	// Phase 1: the pipeline's pre-promotion life as a disposable run -- two
	// inserts and an update, stamped open, the update keeping a full pre-image.
	const preRun int64 = 91001
	pc := connFor(t, preRun, true)
	execWrite(ctx, t, pc, "INSERT INTO analytics.orders (id, amount) VALUES (1, 10), (2, 20)")
	execWrite(ctx, t, pc, "UPDATE analytics.orders SET amount = 11 WHERE id = 1")

	assertCount(ctx, t, adminConn, 3,
		"SELECT count(*) FROM public.data_journal WHERE run_id=$1 AND undo='open'", preRun)
	assertCount(ctx, t, adminConn, 1,
		"SELECT count(*) FROM public.data_journal WHERE run_id=$1 AND op='update' AND pre_image IS NOT NULL", preRun)

	// Phase 2: promote for real -- the live marker-only flip over the pipeline's
	// runs. The pipeline's open entries become promoted; nothing is copied,
	// moved, or deleted: same journal count, pre-image retained, rows intact.
	if err := pg.ExecutePromotionFlip(ctx, client, []int64{preRun}); err != nil {
		t.Fatalf("ExecutePromotionFlip: %v", err)
	}
	assertCount(ctx, t, adminConn, 3,
		"SELECT count(*) FROM public.data_journal WHERE run_id=$1", preRun)
	assertCount(ctx, t, adminConn, 3,
		"SELECT count(*) FROM public.data_journal WHERE run_id=$1 AND undo='promoted'", preRun)
	assertCount(ctx, t, adminConn, 0,
		"SELECT count(*) FROM public.data_journal WHERE run_id=$1 AND undo='open'", preRun)
	// Marker-only: the released update's pre-image is retained (compaction, not
	// promotion, reclaims it), and the table rows promotion "moved" are exactly
	// where the pipeline wrote them.
	assertCount(ctx, t, adminConn, 1,
		"SELECT count(*) FROM public.data_journal WHERE run_id=$1 AND op='update' AND pre_image IS NOT NULL", preRun)
	assertCount(ctx, t, adminConn, 2, "SELECT count(*) FROM analytics.orders")
	assertCount(ctx, t, adminConn, 1, "SELECT count(*) FROM analytics.orders WHERE id=1 AND amount=11")

	// Phase 3: post-promotion writes -- the pipeline now runs permanent (the
	// engine injects wipe-eligibility off). Every operation is STILL captured,
	// one stamp per write, each born promoted with a NULL pre_image: permanent,
	// at stamp cost, no row copy anywhere.
	const postRun int64 = 91002
	qc := connFor(t, postRun, false)
	execWrite(ctx, t, qc, "INSERT INTO analytics.orders (id, amount) VALUES (3, 30)")
	execWrite(ctx, t, qc, "UPDATE analytics.orders SET amount = 12 WHERE id = 1")
	execWrite(ctx, t, qc, "DELETE FROM analytics.orders WHERE id = 2")

	// Still captured: one journal stamp per post-promotion write.
	assertCount(ctx, t, adminConn, 3,
		"SELECT count(*) FROM public.data_journal WHERE run_id=$1", postRun)
	// Permanent: every stamp born promoted -- outside wipe scope from birth.
	assertCount(ctx, t, adminConn, 3,
		"SELECT count(*) FROM public.data_journal WHERE run_id=$1 AND undo='promoted'", postRun)
	assertCount(ctx, t, adminConn, 0,
		"SELECT count(*) FROM public.data_journal WHERE run_id=$1 AND undo='open'", postRun)
	// At stamp cost: slim stamps only, even for the update and the delete -- no
	// pre-image, no row copy.
	assertCount(ctx, t, adminConn, 0,
		"SELECT count(*) FROM public.data_journal WHERE run_id=$1 AND pre_image IS NOT NULL", postRun)
	// And the journal forgot nothing: both eras' stamps coexist in the one store.
	assertCount(ctx, t, adminConn, 6,
		"SELECT count(*) FROM public.data_journal WHERE run_id IN ($1,$2)", preRun, postRun)
}
