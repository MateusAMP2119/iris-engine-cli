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

// TestRunAttribution is the end-to-end proof that every captured write is attributed
// to its run, that attribution rides the injected connection, and that the journal
// layers concurrent and partial writes exactly as the provenance and undo consumers
// require (specification sections 4, 6.3 and 14). It stands up one real Postgres
// cluster the engine has never touched, provisions the partitioned journal and the
// real iris.capture() function through the live pg path, declares a user table with
// the three per-operation capture triggers, and mints a dedicated pipeline writer
// role. Each subtest then drives writes AS that role over a connection whose run id
// rides it exactly as the engine injects it at spawn -- via pg.InjectRunID merging the
// per-session iris.run_id setting onto the DSN, never a hand-issued SET -- and asserts
// the live journal.
//
// The six legs, one per contract:
//
//   - run-attribution-via-connection: a write on a connection carrying iris.run_id is
//     stamped with that run; a write on a connection carrying no run id is rejected and
//     leaves no row -- no journal row is ever keyed to a role without a run.
//   - capture-in-data-transaction: the journal stamp lives in the writer's own data-DB
//     transaction -- invisible until commit, gone on rollback, present on commit.
//   - write-attributed-same-txn: the stamp attributing a write to its run is written and
//     committed in the very transaction that made the write.
//   - capture-row-per-write: N writes by a run produce exactly N journal rows, each with
//     the right op, attributed to that run.
//   - journal-row-commit-ordered: two concurrent transactions contending on one row are
//     serialized by a row lock, and their journal ids for that row are strictly
//     commit-ordered (the trigger fires inside the writing transaction).
//   - partial-writes-attributed-revertible: a run that dead-letters mid-way leaves its
//     already-committed writes visible and attributed within its journal window
//     [journal_floor, journal_ceiling], each born undo=open (wipe-revertible).
//
// It drives the pg journal/capture DDL directly against the live cluster (the
// external_data_db conformance pattern) rather than the CLI, so the leg proves the
// exact behavior a real Postgres enforces. The managed embedded-postgres runtime is
// cached after the first run; the leg reports its own wall time.
func TestRunAttribution(t *testing.T) {
	start := time.Now()
	t.Cleanup(func() { t.Logf("run attribution conformance leg: %s", time.Since(start).Round(time.Millisecond)) })

	const (
		superuser = "postgres"
		superpw   = "superpw"
		writer    = "iris_attr_writer"
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
	// journal and the real iris.capture() function, then the user table and its capture
	// triggers.
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

	// Mint the pipeline writer role with exactly what a pipeline role needs: DML on its
	// table plus USAGE/EXECUTE to reach the SECURITY DEFINER capture function.
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

	// A superuser read/inspect connection for journal assertions and lock introspection.
	adminConn, err := pgx.Connect(ctx, dsnTo(pg.DataDatabase, superuser, superpw))
	if err != nil {
		t.Fatalf("connect admin read conn: %v", err)
	}
	t.Cleanup(func() { _ = adminConn.Close(ctx) })

	writerBaseDSN := dsnTo(pg.DataDatabase, writer, writerpw)

	// connWithRun opens a fresh writer connection carrying runID as the per-session
	// iris.run_id setting exactly as the engine injects it: the run id rides the DSN via
	// pg.InjectRunID (the mechanism under test), never a hand-issued SET.
	connWithRun := func(t *testing.T, runID int64) *pgx.Conn {
		t.Helper()
		conn, err := pgx.Connect(ctx, pg.InjectRunID(writerBaseDSN, runID))
		if err != nil {
			t.Fatalf("connect writer with run id %d: %v", runID, err)
		}
		t.Cleanup(func() { _ = conn.Close(ctx) })
		return conn
	}

	// spec: S04/run-attribution-via-connection
	t.Run("S04/run-attribution-via-connection", func(t *testing.T) {
		// Positive: the run id rides the connection; the trigger reads it in-transaction
		// and stamps the write with it -- no hand-issued SET anywhere.
		const attributedRun int64 = 71001
		wc := connWithRun(t, attributedRun)
		execWrite(ctx, t, wc, "INSERT INTO analytics.orders (id, amount) VALUES (100, 10)")
		assertCount(ctx, t, adminConn, 1,
			"SELECT count(*) FROM public.data_journal WHERE run_id=$1 AND schema='analytics' AND \"table\"='orders' AND row_pk='100' AND op='insert' AND pg_role=$2",
			attributedRun, writer)

		// Negative: a connection carrying NO run id cannot stamp -- the trigger has no
		// iris.run_id to read, so the write is rejected and leaves no journal row. No row
		// is ever keyed to a role without a run.
		bare, err := pgx.Connect(ctx, writerBaseDSN)
		if err != nil {
			t.Fatalf("connect writer without a run id: %v", err)
		}
		defer func() { _ = bare.Close(ctx) }()
		if _, err := bare.Exec(ctx, "INSERT INTO analytics.orders (id, amount) VALUES (200, 20)"); err == nil {
			t.Errorf("a write on a connection with no run id was accepted; it must be rejected so no row is keyed to a role without a run")
		}
		assertCount(ctx, t, adminConn, 0,
			"SELECT count(*) FROM public.data_journal WHERE schema='analytics' AND \"table\"='orders' AND row_pk='200'")
		// And the row itself never landed: the whole write (data + stamp) was rejected.
		assertCount(ctx, t, adminConn, 0, "SELECT count(*) FROM analytics.orders WHERE id=200")

		// A MANUAL run attributes identically: the manual-run plane injects the run-scoped
		// connection with the very same formula the lane path uses -- IRIS_DB_URL =
		// pg.InjectRunID(base, run id) (daemon.manualExec.injectedDBURL). Attribution is by
		// run id, never by cause, so a manually-run pipeline's captured write journals to
		// its own run just like a lane run's. Here the manual run's run id rides the
		// injected connection exactly as the daemon builds it at spawn.
		const manualRun int64 = 71011
		manualConn := connWithRun(t, manualRun) // == the manual plane's IRIS_DB_URL for this run
		execWrite(ctx, t, manualConn, "INSERT INTO analytics.orders (id, amount) VALUES (110, 11)")
		assertCount(ctx, t, adminConn, 1,
			"SELECT count(*) FROM public.data_journal WHERE run_id=$1 AND schema='analytics' AND \"table\"='orders' AND row_pk='110' AND op='insert' AND pg_role=$2",
			manualRun, writer)
	})

	// spec: S04/capture-in-data-transaction
	t.Run("S04/capture-in-data-transaction", func(t *testing.T) {
		const run int64 = 71002
		wc := connWithRun(t, run)

		// Inside an uncommitted transaction the stamp is invisible to another session:
		// it lives in the writer's own data-DB transaction, not an out-of-band write.
		if _, err := wc.Exec(ctx, "BEGIN"); err != nil {
			t.Fatalf("BEGIN: %v", err)
		}
		execWrite(ctx, t, wc, "INSERT INTO analytics.orders (id, amount) VALUES (300, 30)")
		assertCount(ctx, t, adminConn, 0,
			"SELECT count(*) FROM public.data_journal WHERE run_id=$1 AND row_pk='300'", run)

		// Rollback: the stamp rolls back WITH the write -- neither the row nor its stamp
		// survives.
		if _, err := wc.Exec(ctx, "ROLLBACK"); err != nil {
			t.Fatalf("ROLLBACK: %v", err)
		}
		assertCount(ctx, t, adminConn, 0,
			"SELECT count(*) FROM public.data_journal WHERE run_id=$1 AND row_pk='300'", run)
		assertCount(ctx, t, adminConn, 0, "SELECT count(*) FROM analytics.orders WHERE id=300")

		// Commit: the same write in a committed transaction lands both the row and
		// exactly one stamp -- capture commits atomically with the write.
		if _, err := wc.Exec(ctx, "BEGIN"); err != nil {
			t.Fatalf("BEGIN (commit path): %v", err)
		}
		execWrite(ctx, t, wc, "INSERT INTO analytics.orders (id, amount) VALUES (300, 30)")
		if _, err := wc.Exec(ctx, "COMMIT"); err != nil {
			t.Fatalf("COMMIT: %v", err)
		}
		assertCount(ctx, t, adminConn, 1,
			"SELECT count(*) FROM public.data_journal WHERE run_id=$1 AND row_pk='300' AND op='insert'", run)
	})

	// spec: S14/write-attributed-same-txn
	t.Run("S14/write-attributed-same-txn", func(t *testing.T) {
		const run int64 = 71003
		wc := connWithRun(t, run)

		// The write and its attributing stamp share one transaction: within the open
		// transaction the writer already sees its own stamp, attributed to its run, while
		// another session sees nothing until commit.
		if _, err := wc.Exec(ctx, "BEGIN"); err != nil {
			t.Fatalf("BEGIN: %v", err)
		}
		execWrite(ctx, t, wc, "INSERT INTO analytics.orders (id, amount) VALUES (400, 40)")

		// In-transaction the stamp is present and attributed to this run and role.
		assertCount(ctx, t, wc, 1,
			"SELECT count(*) FROM public.data_journal WHERE run_id=$1 AND row_pk='400' AND op='insert' AND pg_role=$2",
			run, writer)
		// From another session, still nothing: it rides the writer's uncommitted txn.
		assertCount(ctx, t, adminConn, 0,
			"SELECT count(*) FROM public.data_journal WHERE run_id=$1 AND row_pk='400'", run)

		if _, err := wc.Exec(ctx, "COMMIT"); err != nil {
			t.Fatalf("COMMIT: %v", err)
		}
		// Committed in the same transaction as the write: now visible and attributed.
		assertCount(ctx, t, adminConn, 1,
			"SELECT count(*) FROM public.data_journal WHERE run_id=$1 AND row_pk='400' AND op='insert' AND pg_role=$2",
			run, writer)
	})

	// spec: S04/capture-row-per-write
	t.Run("S04/capture-row-per-write", func(t *testing.T) {
		const run int64 = 71004
		wc := connWithRun(t, run)

		// Three distinct single-row writes -- an insert, then an update and a delete of
		// their own rows -- each produce exactly one journal row, attributed to the run,
		// with the right op.
		execWrite(ctx, t, wc, "INSERT INTO analytics.orders (id, amount) VALUES (500, 50)")
		execWrite(ctx, t, wc, "INSERT INTO analytics.orders (id, amount) VALUES (501, 51)")
		execWrite(ctx, t, wc, "UPDATE analytics.orders SET amount = 999 WHERE id = 500")
		execWrite(ctx, t, wc, "DELETE FROM analytics.orders WHERE id = 501")

		// Exactly four stamps for four writes, all attributed to this run.
		assertCount(ctx, t, adminConn, 4,
			"SELECT count(*) FROM public.data_journal WHERE run_id=$1", run)
		// One per write, with the op that made it.
		assertCount(ctx, t, adminConn, 1,
			"SELECT count(*) FROM public.data_journal WHERE run_id=$1 AND row_pk='500' AND op='insert'", run)
		assertCount(ctx, t, adminConn, 1,
			"SELECT count(*) FROM public.data_journal WHERE run_id=$1 AND row_pk='501' AND op='insert'", run)
		assertCount(ctx, t, adminConn, 1,
			"SELECT count(*) FROM public.data_journal WHERE run_id=$1 AND row_pk='500' AND op='update'", run)
		assertCount(ctx, t, adminConn, 1,
			"SELECT count(*) FROM public.data_journal WHERE run_id=$1 AND row_pk='501' AND op='delete'", run)
	})

	// spec: S06.3/journal-row-commit-ordered
	t.Run("S06.3/journal-row-commit-ordered", func(t *testing.T) {
		const (
			runA int64 = 71005
			runB int64 = 71006
			seed int64 = 71000
			row        = 900
		)
		// Seed the contended row (committed) so both runs UPDATE the same existing row.
		seedConn := connWithRun(t, seed)
		execWrite(ctx, t, seedConn, fmt.Sprintf("INSERT INTO analytics.orders (id, amount) VALUES (%d, 0)", row))

		connA := connWithRun(t, runA)
		connB := connWithRun(t, runB)
		bpid := backendPID(ctx, t, connB)

		// A takes the row lock: its UPDATE fires capture inside A's transaction (A's
		// journal id is assigned now) and A holds the row lock until it commits.
		if _, err := connA.Exec(ctx, "BEGIN"); err != nil {
			t.Fatalf("A BEGIN: %v", err)
		}
		execWrite(ctx, t, connA, fmt.Sprintf("UPDATE analytics.orders SET amount = amount + 1 WHERE id = %d", row))

		// B attempts the same-row UPDATE; it blocks on A's row lock. Run it on its own
		// goroutine and report completion through a channel.
		bDone := make(chan error, 1)
		go func() {
			if _, err := connB.Exec(ctx, "BEGIN"); err != nil {
				bDone <- fmt.Errorf("B BEGIN: %w", err)
				return
			}
			// Blocks here until A commits and releases the row lock.
			if _, err := connB.Exec(ctx, fmt.Sprintf("UPDATE analytics.orders SET amount = amount + 2 WHERE id = %d", row)); err != nil {
				bDone <- fmt.Errorf("B UPDATE: %w", err)
				return
			}
			if _, err := connB.Exec(ctx, "COMMIT"); err != nil {
				bDone <- fmt.Errorf("B COMMIT: %w", err)
				return
			}
			bDone <- nil
		}()

		// Synchronize on DB state, not a fixed sleep: wait until B is actually blocked on
		// A's lock (pg_blocking_pids(B) names A) before committing A.
		waitBlockedOn(ctx, t, adminConn, bpid)

		// A commits first: its stamp is durable and its row lock released, letting B
		// proceed. B then stamps with a strictly higher journal id and commits second.
		if _, err := connA.Exec(ctx, "COMMIT"); err != nil {
			t.Fatalf("A COMMIT: %v", err)
		}
		if err := <-bDone; err != nil {
			t.Fatalf("B contended update: %v", err)
		}

		// Journal ids for the contested row are strictly commit-ordered: A committed
		// first, so A's update stamp carries the lower id; B's the higher.
		idA := journalUpdateID(ctx, t, adminConn, runA, row)
		idB := journalUpdateID(ctx, t, adminConn, runB, row)
		if idA >= idB {
			t.Errorf("contested row %d: A committed first but its journal id %d is not below B's %d; ids are not commit-ordered", row, idA, idB)
		}
	})

	// spec: S06.3/partial-writes-attributed-revertible
	t.Run("S06.3/partial-writes-attributed-revertible", func(t *testing.T) {
		const run int64 = 71007

		// journal_floor: the journal high id at dispatch, before the run writes anything.
		floor := maxJournalID(ctx, t, adminConn)

		wc := connWithRun(t, run)
		// The run commits some writes as it goes (scripts commit as they go)...
		execWrite(ctx, t, wc, "INSERT INTO analytics.orders (id, amount) VALUES (600, 60)")
		execWrite(ctx, t, wc, "INSERT INTO analytics.orders (id, amount) VALUES (601, 61)")
		execWrite(ctx, t, wc, "INSERT INTO analytics.orders (id, amount) VALUES (602, 62)")

		// ...then dead-letters mid-way: the next statement fails (a duplicate key), so
		// its write and stamp roll back and it never reaches rows 603+. A run is not one
		// transaction; the committed partial writes survive.
		if _, err := wc.Exec(ctx, "INSERT INTO analytics.orders (id, amount) VALUES (600, 999)"); err == nil {
			t.Fatalf("the mid-way failing write was accepted; expected a duplicate-key rejection")
		}

		// journal_ceiling: the journal high id at the run's terminal (dead-letter)
		// transition.
		ceiling := maxJournalID(ctx, t, adminConn)

		// The partial writes remain visible and attributed to the run: exactly the three
		// committed inserts, each born undo=open (wipe-revertible when disposable).
		assertCount(ctx, t, adminConn, 3,
			"SELECT count(*) FROM public.data_journal WHERE run_id=$1 AND op='insert' AND undo='open'", run)
		// The failed write left no stamp: only the three committed writes are attributed.
		assertCount(ctx, t, adminConn, 3,
			"SELECT count(*) FROM public.data_journal WHERE run_id=$1", run)

		// The journal window [floor+1, ceiling] identifies exactly the run's partial
		// writes: every stamp of this run falls inside its window, and the window's
		// contents for this run are its whole write set -- the handle a wipe reverts.
		assertCount(ctx, t, adminConn, 3,
			"SELECT count(*) FROM public.data_journal WHERE run_id=$1 AND id > $2 AND id <= $3", run, floor, ceiling)
		assertCount(ctx, t, adminConn, 0,
			"SELECT count(*) FROM public.data_journal WHERE run_id=$1 AND (id <= $2 OR id > $3)", run, floor, ceiling)
	})
}

// backendPID returns the server-side backend process id of conn, so another session can
// introspect whether conn is blocked on a lock (deterministic synchronization, no
// fixed sleep).
func backendPID(ctx context.Context, t *testing.T, conn *pgx.Conn) int32 {
	t.Helper()
	var pid int32
	if err := conn.QueryRow(ctx, "SELECT pg_backend_pid()").Scan(&pid); err != nil {
		t.Fatalf("read backend pid: %v", err)
	}
	return pid
}

// waitBlockedOn polls until the backend pid is actually blocked waiting on another
// session's lock (pg_blocking_pids names at least one blocker), to a bounded deadline.
// It synchronizes on real database lock state rather than a fixed sleep, so the
// commit-ordering leg is deterministic: the test proceeds the instant the contender is
// provably waiting, and fails loudly if it never blocks.
func waitBlockedOn(ctx context.Context, t *testing.T, conn *pgx.Conn, pid int32) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for {
		var blocked bool
		if err := conn.QueryRow(ctx, "SELECT cardinality(pg_blocking_pids($1)) > 0", pid).Scan(&blocked); err != nil {
			t.Fatalf("poll blocking pids for backend %d: %v", pid, err)
		}
		if blocked {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("backend %d never blocked on the contended row lock within the deadline", pid)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// journalUpdateID returns the journal id of the single update stamp attributing the
// given row to the given run: the ordering key whose commit order the leg checks.
func journalUpdateID(ctx context.Context, t *testing.T, conn *pgx.Conn, run int64, row int) int64 {
	t.Helper()
	var id int64
	err := conn.QueryRow(ctx,
		"SELECT id FROM public.data_journal WHERE run_id=$1 AND schema='analytics' AND \"table\"='orders' AND row_pk=$2 AND op='update'",
		run, fmt.Sprintf("%d", row)).Scan(&id)
	if err != nil {
		t.Fatalf("read update journal id for run %d row %d: %v", run, row, err)
	}
	return id
}

// maxJournalID returns the journal high id: the max data_journal id, or 0 when empty.
// It models the journal_floor / journal_ceiling reads the dispatcher takes at a run's
// dispatch and terminal transition.
func maxJournalID(ctx context.Context, t *testing.T, conn *pgx.Conn) int64 {
	t.Helper()
	var id int64
	if err := conn.QueryRow(ctx, "SELECT COALESCE(max(id), 0) FROM public.data_journal").Scan(&id); err != nil {
		t.Fatalf("read journal high id: %v", err)
	}
	return id
}
