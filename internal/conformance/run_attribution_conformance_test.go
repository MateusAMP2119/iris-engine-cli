//go:build conformance

package conformance

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
)

// TestRunAttribution is the end-to-end proof that under the turn protocol (#206)
// every captured write is attributed to exactly its run. Pipelines hold no database
// credentials: a pipeline speaks row frames on stdout, and the ENGINE performs the
// writes on its own connection inside ONE data transaction per turn, with the run id
// riding that transaction (SET LOCAL iris.run_id) so the capture trigger journals
// each row to the minted run. It boots one real engine over two frame-speaking
// pipelines -- a phased writer and a phased protocol violator -- and asserts the
// live journal and run records.
//
// The legs, one per subtest, each the turn-protocol successor of the old
// injected-connection contract:
//
//   - run-attribution-via-frames: a turn's emitted rows journal with exactly the
//     minted run's id -- one stamp per frame, op and row key exact, born undo=open.
//   - engine-role-and-no-pipeline-credentials: journal stamps carry ONE writing
//     role, the engine's own (never a pipeline role), and the pipeline roles hold
//     zero database backends -- the engine mediates every database access.
//   - capture-row-per-frame: N row frames journal exactly N stamps with the upsert
//     ops -- the first write of a row key journals op=insert, a re-produced key
//     journals op=update and lands the new values. (A delete is unreachable via
//     frames: the protocol only upserts, so the delete op has no frame-path leg.)
//   - journal-rows-commit-ordered: turns of a pipeline are serialized on its lane,
//     so consecutive producing turns' journal ids are strictly commit-ordered.
//   - violating-turn-commits-nothing: a row frame naming an undeclared table is a
//     protocol violation -- the run is minted directly dead_lettered (cause=loop,
//     exit_code NULL, dead_letters reason=failed with the offending line quoted)
//     and the turn's whole data transaction commits NOTHING, valid earlier frames
//     included; the failed dead letter engages the no-retry brake, and a manual
//     release recovers and commits normally.
//   - turn-window-attributes-whole-write-set: a producing run's recorded journal
//     window [journal_floor, journal_ceiling] contains its entire write set -- the
//     handle a wipe reverts.
//   - quiet-loop-parks-and-manual-run-attributes: drained pipelines answer quiet
//     turns that record NOTHING (no run rows accumulate; the lanes park), and a
//     manual run (minted directly running, one protocol turn) journals its emitted
//     row to its own run id -- attribution is by run id, never by cause.
func TestRunAttribution(t *testing.T) {
	start := time.Now()
	t.Cleanup(func() { t.Logf("run attribution conformance leg: %s", time.Since(start).Round(time.Millisecond)) })

	ensurePython(t)
	freshDatabases(t)
	bin := Build(t)
	ws := shortWorkspace(t)
	writeAttrWorkspace(t, ws)
	stop := startEngine(t, bin, ws)
	defer stop()
	bin.Run(t, RunOptions{Args: []string{"declare", "apply", "pipelines/attr_writer"}, Dir: ws, Timeout: time.Minute}).RequireExit(t, 0)
	bin.Run(t, RunOptions{Args: []string{"declare", "apply", "pipelines/attr_atomic"}, Dir: ws, Timeout: time.Minute}).RequireExit(t, 0)

	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Minute)
	defer cancel()
	meta, err := pgx.Connect(ctx, metaDSN(t, ws))
	if err != nil {
		t.Fatalf("connect meta: %v", err)
	}
	defer func() { _ = meta.Close(ctx) }()
	data, err := pgx.Connect(ctx, dataDSN(t, ws))
	if err != nil {
		t.Fatalf("connect data: %v", err)
	}
	defer func() { _ = data.Close(ctx) }()

	// Drain the phased scripts: the loop drives attr_writer through its two
	// producing turns and attr_atomic through its violating turn (whose failed
	// dead letter engages the no-retry brake until the manual release below),
	// then both answer quiet turns and park. Every wait is on recorded meta
	// state, never elapsed time.
	awaitCount(ctx, t, meta, 2, 120*time.Second,
		"SELECT count(*) FROM runs WHERE pipeline='attr_writer' AND cause='loop' AND state='succeeded'")
	awaitCount(ctx, t, meta, 1, 120*time.Second,
		"SELECT count(*) FROM runs WHERE pipeline='attr_atomic' AND cause='loop' AND state='dead_lettered'")
	// The violation engaged the loop's no-retry brake (a failed dead letter is
	// never retried on its own), so the recovery turn is an operator release: a
	// manual run, executing the phased script's producing turn.
	bin.Run(t, RunOptions{Args: []string{"pipeline", "run", "attr_atomic"}, Dir: ws, Timeout: 2 * time.Minute}).RequireExit(t, 0)
	awaitCount(ctx, t, meta, 1, 120*time.Second,
		"SELECT count(*) FROM runs WHERE pipeline='attr_atomic' AND cause='manual' AND state='succeeded'")

	// The four recorded runs, in commit order: attr_writer's two producing turns,
	// attr_atomic's violating turn and its manual recovery run.
	w1 := scanID(ctx, t, meta, "SELECT min(id) FROM runs WHERE pipeline='attr_writer' AND state='succeeded'")
	w2 := scanID(ctx, t, meta, "SELECT max(id) FROM runs WHERE pipeline='attr_writer' AND state='succeeded'")
	violated := scanID(ctx, t, meta, "SELECT id FROM runs WHERE pipeline='attr_atomic' AND state='dead_lettered'")
	recovered := scanID(ctx, t, meta, "SELECT id FROM runs WHERE pipeline='attr_atomic' AND state='succeeded'")
	if w1 == w2 {
		t.Fatalf("attr_writer's two producing turns share run id %d; want two distinct minted runs", w1)
	}

	t.Run("run-attribution-via-frames", func(t *testing.T) {
		// The first producing turn emitted two rows; each journals one stamp keyed
		// to exactly that turn's minted run id, with the exact op and row key, born
		// undo=open (wipe-revertible). No hand-issued SET anywhere: the engine set
		// the run id on the turn's own data transaction.
		assertCount(ctx, t, data, 1,
			`SELECT count(*) FROM public.data_journal WHERE run_id=$1 AND schema='attrdata' AND "table"='orders' AND row_pk='500' AND op='insert' AND undo='open'`, w1)
		assertCount(ctx, t, data, 1,
			`SELECT count(*) FROM public.data_journal WHERE run_id=$1 AND schema='attrdata' AND "table"='orders' AND row_pk='501' AND op='insert' AND undo='open'`, w1)
		// Exactly its own two stamps, nothing more: attribution is exact per run.
		assertCount(ctx, t, data, 2, "SELECT count(*) FROM public.data_journal WHERE run_id=$1", w1)
	})

	t.Run("engine-role-and-no-pipeline-credentials", func(t *testing.T) {
		// One role stamps every attrdata journal row: the engine's own data role.
		// A pipeline role can never appear -- the pipeline process holds no
		// database credentials at all under the turn protocol.
		var roles int
		var role string
		if err := data.QueryRow(ctx,
			"SELECT count(DISTINCT pg_role), min(pg_role) FROM public.data_journal WHERE schema='attrdata'").Scan(&roles, &role); err != nil {
			t.Fatalf("read journal writing roles: %v", err)
		}
		if roles != 1 {
			t.Errorf("attrdata journal rows carry %d distinct pg_roles, want exactly the engine's 1", roles)
		}
		if strings.HasPrefix(role, "iris_pipeline_") {
			t.Errorf("journal pg_role %q is a pipeline role; the engine performs every write under its own role", role)
		}
		// The successor of the old no-run-id rejection: a pipeline cannot reach the
		// database at all, so its role holds zero backends.
		assertCount(ctx, t, data, 0,
			"SELECT count(*) FROM pg_stat_activity WHERE usename IN ('iris_pipeline_attr_writer','iris_pipeline_attr_atomic')")
	})

	t.Run("capture-row-per-frame", func(t *testing.T) {
		// The second producing turn re-emitted row key 500 and emitted new key 502:
		// two frames, exactly two stamps attributed to that run, the re-produced
		// key journaling op=update (the engine upserts on the primary key) and the
		// new key op=insert.
		assertCount(ctx, t, data, 1,
			"SELECT count(*) FROM public.data_journal WHERE run_id=$1 AND row_pk='500' AND op='update'", w2)
		assertCount(ctx, t, data, 1,
			"SELECT count(*) FROM public.data_journal WHERE run_id=$1 AND row_pk='502' AND op='insert'", w2)
		assertCount(ctx, t, data, 2, "SELECT count(*) FROM public.data_journal WHERE run_id=$1", w2)
		// The upsert landed the re-produced key's new values.
		assertCount(ctx, t, data, 1, "SELECT count(*) FROM attrdata.orders WHERE id=500 AND amount=999")
	})

	t.Run("journal-rows-commit-ordered", func(t *testing.T) {
		// Turns of one pipeline are serialized on its lane, each committing its own
		// data transaction: the first turn's stamps all carry strictly lower
		// journal ids than the second's.
		maxW1 := scanID(ctx, t, data, "SELECT max(id) FROM public.data_journal WHERE run_id=$1", w1)
		minW2 := scanID(ctx, t, data, "SELECT min(id) FROM public.data_journal WHERE run_id=$1", w2)
		if maxW1 >= minW2 {
			t.Errorf("turn 1 committed first but its journal high id %d is not below turn 2's low id %d; ids are not commit-ordered", maxW1, minW2)
		}
	})

	t.Run("violating-turn-commits-nothing", func(t *testing.T) {
		// The violating turn emitted a valid declared row (ledger 600) and then a
		// row frame naming an undeclared table. The violation dead-letters the run:
		// minted directly dead_lettered, cause=loop, exit_code NULL (no exit is
		// involved), with the worklist row naming the violation and quoting the
		// offending line.
		var cause string
		var exitCode *int32
		if err := meta.QueryRow(ctx, "SELECT cause, exit_code FROM runs WHERE id=$1", violated).Scan(&cause, &exitCode); err != nil {
			t.Fatalf("read dead-lettered run %d: %v", violated, err)
		}
		if cause != "loop" {
			t.Errorf("violating run's cause = %q, want loop (a fresh turn's failure always records)", cause)
		}
		if exitCode != nil {
			t.Errorf("violating run's exit_code = %d, want NULL (a protocol violation is not a process exit)", *exitCode)
		}
		var reason, errText string
		if err := meta.QueryRow(ctx, "SELECT reason, coalesce(error,'') FROM dead_letters WHERE run_id=$1", violated).Scan(&reason, &errText); err != nil {
			t.Fatalf("read dead letter for run %d: %v", violated, err)
		}
		if reason != "failed" {
			t.Errorf("dead letter reason = %q, want failed", reason)
		}
		if !strings.Contains(errText, "turn protocol violation") || !strings.Contains(errText, "attrdata.forbidden") {
			t.Errorf("dead letter error does not name the violation with the offending table: %q", errText)
		}

		// The whole turn is ONE atomic data transaction: the violation commits
		// NOTHING, the valid earlier frame included -- no row, no stamp, nothing
		// keyed to the dead-lettered run.
		assertCount(ctx, t, data, 0, "SELECT count(*) FROM attrdata.ledger WHERE id=600")
		assertCount(ctx, t, data, 0,
			`SELECT count(*) FROM public.data_journal WHERE schema='attrdata' AND "table"='ledger' AND row_pk='600'`)
		assertCount(ctx, t, data, 0, "SELECT count(*) FROM public.data_journal WHERE run_id=$1", violated)

		// The manual release (a fresh one-shot worker: the violated session was
		// recycled and the brake parked the loop) recovers: its row commits and
		// journals to its own run, and the table holds exactly the recovery row --
		// the violating turn's row never landed.
		assertCount(ctx, t, data, 1,
			"SELECT count(*) FROM public.data_journal WHERE run_id=$1 AND row_pk='601' AND op='insert' AND undo='open'", recovered)
		assertCount(ctx, t, data, 1, "SELECT count(*) FROM public.data_journal WHERE run_id=$1", recovered)
		assertCount(ctx, t, data, 1, "SELECT count(*) FROM attrdata.ledger WHERE id=601 AND amount=61")
		assertCount(ctx, t, data, 1, "SELECT count(*) FROM attrdata.ledger")
	})

	t.Run("turn-window-attributes-whole-write-set", func(t *testing.T) {
		// A producing turn's run row records the journal window its own data
		// transaction read: every stamp of the run falls inside
		// (journal_floor, journal_ceiling] -- the whole write set a wipe reverts.
		for _, leg := range []struct {
			run    int64
			stamps int64
		}{{w1, 2}, {w2, 2}} {
			var floor, ceiling *int64
			if err := meta.QueryRow(ctx, "SELECT journal_floor, journal_ceiling FROM runs WHERE id=$1", leg.run).Scan(&floor, &ceiling); err != nil {
				t.Fatalf("read journal window of run %d: %v", leg.run, err)
			}
			if floor == nil || ceiling == nil {
				t.Fatalf("run %d recorded no journal window; a producing turn always stamps [floor, ceiling]", leg.run)
			}
			assertCount(ctx, t, data, leg.stamps,
				"SELECT count(*) FROM public.data_journal WHERE run_id=$1 AND id > $2 AND id <= $3", leg.run, *floor, *ceiling)
			assertCount(ctx, t, data, 0,
				"SELECT count(*) FROM public.data_journal WHERE run_id=$1 AND (id <= $2 OR id > $3)", leg.run, *floor, *ceiling)
		}
		// The manual recovery run is pre-minted (no dispatch-time floor); its
		// terminal ceiling still bounds its whole write set.
		var recCeiling *int64
		if err := meta.QueryRow(ctx, "SELECT journal_ceiling FROM runs WHERE id=$1", recovered).Scan(&recCeiling); err != nil {
			t.Fatalf("read journal ceiling of run %d: %v", recovered, err)
		}
		if recCeiling == nil {
			t.Fatal("manual recovery run recorded no journal ceiling")
		}
		assertCount(ctx, t, data, 0,
			"SELECT count(*) FROM public.data_journal WHERE run_id=$1 AND id > $2", recovered, *recCeiling)
	})

	t.Run("quiet-loop-parks-and-manual-run-attributes", func(t *testing.T) {
		// Drained, both pipelines answer quiet turns: a quiet turn records NOTHING
		// -- no run row, no watermark bump -- so the lanes park and the run set
		// stays frozen. (The old contract's perpetual succeeded runs hold only for
		// turns that produce rows.)
		before := scanID(ctx, t, meta, "SELECT coalesce(max(id),0) FROM runs")
		time.Sleep(3 * time.Second)
		after := scanID(ctx, t, meta, "SELECT coalesce(max(id),0) FROM runs")
		if after != before {
			t.Fatalf("quiet turns minted run rows (max id %d -> %d); a quiet turn must record nothing", before, after)
		}

		// A manual run executes as one protocol turn, minted directly running and
		// completing running -> succeeded. The marker steers the parked script's
		// next turn to emit one row; only the manual cause can wake the parked
		// lane, so the emitting turn is the manual one. Attribution is by run id,
		// never by cause: the row journals to the manual run's own id.
		if err := os.WriteFile(filepath.Join(ws, "pipelines", "attr_writer", "manual.marker"), nil, 0o644); err != nil {
			t.Fatalf("write manual marker: %v", err)
		}
		bin.Run(t, RunOptions{Args: []string{"pipeline", "run", "attr_writer"}, Dir: ws, Timeout: 2 * time.Minute}).RequireExit(t, 0)
		awaitCount(ctx, t, meta, 1, 30*time.Second,
			"SELECT count(*) FROM runs WHERE pipeline='attr_writer' AND cause='manual' AND state='succeeded'")
		manualID := scanID(ctx, t, meta, "SELECT id FROM runs WHERE pipeline='attr_writer' AND cause='manual'")

		// The immediate manual run stamped its process handle just after spawn.
		var handle *int64
		if err := meta.QueryRow(ctx, "SELECT handle FROM runs WHERE id=$1", manualID).Scan(&handle); err != nil {
			t.Fatalf("read manual run handle: %v", err)
		}
		if handle == nil {
			t.Errorf("manual run %d recorded no process handle; an immediate manual run stamps it after spawn", manualID)
		}

		// Its one emitted row journals to exactly its own run id.
		assertCount(ctx, t, data, 1,
			`SELECT count(*) FROM public.data_journal WHERE run_id=$1 AND schema='attrdata' AND "table"='orders' AND row_pk='510' AND op='insert'`, manualID)
		assertCount(ctx, t, data, 1, "SELECT count(*) FROM public.data_journal WHERE run_id=$1", manualID)

		// The exactness sweep: every stamp on the writer's table belongs to one of
		// its three recorded runs -- no journal row is ever keyed to a run that did
		// not make it.
		assertCount(ctx, t, data, 5,
			`SELECT count(*) FROM public.data_journal WHERE schema='attrdata' AND "table"='orders'`)
		assertCount(ctx, t, data, 0,
			`SELECT count(*) FROM public.data_journal WHERE schema='attrdata' AND "table"='orders' AND run_id NOT IN ($1, $2, $3)`, w1, w2, manualID)
	})
}

// attrOrdersSchemaYAML declares the attrdata.orders table the writer targets.
const attrOrdersSchemaYAML = "schema: attrdata\ntable: orders\ncolumns:\n  - name: id\n    type: int\n    primary_key: true\n  - name: amount\n    type: int\n"

// attrLedgerSchemaYAML declares the attrdata.ledger table the atomicity pipeline targets.
const attrLedgerSchemaYAML = "schema: attrdata\ntable: ledger\ncolumns:\n  - name: id\n    type: int\n    primary_key: true\n  - name: amount\n    type: int\n"

// attrPhasePrelude persists a per-pipeline phase in the pipeline folder, across
// turns AND worker restarts (the violation leg's session is recycled), so each
// scripted phase happens exactly once no matter how many turns the loop drives.
const attrPhasePrelude = `
def phase():
    try:
        with open("phase") as f:
            return f.read().strip()
    except FileNotFoundError:
        return "1"

def advance(p):
    with open("phase", "w") as f:
        f.write(p)
`

// attrWriterScript is the frame-speaking writer: turn one inserts row keys 500
// and 501, turn two re-produces 500 (an engine upsert: op=update) and inserts
// 502, and every later turn is quiet -- unless the test dropped the manual
// marker, in which case the turn (the manual one: only a manual cause wakes the
// parked lane) emits the manual-attribution row. It opens no database connection.
const attrWriterScript = "import os\n" + PyTurnPrelude + attrPhasePrelude + `
def on_turn(turn, rows):
    p = phase()
    if p == "1":
        advance("2")
        emit("attrdata.orders", {"id": 500, "amount": 50})
        emit("attrdata.orders", {"id": 501, "amount": 51})
    elif p == "2":
        advance("3")
        emit("attrdata.orders", {"id": 500, "amount": 999})
        emit("attrdata.orders", {"id": 502, "amount": 52})
    elif os.path.exists("manual.marker"):
        os.remove("manual.marker")
        emit("attrdata.orders", {"id": 510, "amount": 11})
    done(turn)

turn_loop(on_turn)
`

// attrAtomicScript is the frame-speaking violator: turn one emits a valid
// declared row and then a row frame naming an undeclared table (the protocol
// violation that dead-letters the turn and commits nothing -- the phase file
// advances first, so the violation happens exactly once even though the engine
// recycles the session), turn two emits the recovery row, and every later turn
// is quiet.
const attrAtomicScript = PyTurnPrelude + attrPhasePrelude + `
def on_turn(turn, rows):
    p = phase()
    if p == "1":
        advance("2")
        emit("attrdata.ledger", {"id": 600, "amount": 60})
        emit("attrdata.forbidden", {"id": 1})
    elif p == "2":
        advance("3")
        emit("attrdata.ledger", {"id": 601, "amount": 61})
    done(turn)

turn_loop(on_turn)
`

// writeAttrWorkspace lays out the attribution workspace: the two attrdata schema
// tables plus the two frame-speaking pipeline folders.
func writeAttrWorkspace(t *testing.T, ws string) {
	t.Helper()
	for table, yaml := range map[string]string{"orders": attrOrdersSchemaYAML, "ledger": attrLedgerSchemaYAML} {
		dir := filepath.Join(ws, "schemas", "attrdata", table)
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatalf("mkdir schema %s: %v", table, err)
		}
		if err := os.WriteFile(filepath.Join(dir, "table.yaml"), []byte(yaml), 0o644); err != nil {
			t.Fatalf("write table.yaml for %s: %v", table, err)
		}
	}
	writePipelineFolder(t, ws, "attr_writer",
		"name: attr_writer\nrun: [python, main.py]\nwrites:\n  - table: attrdata.orders\n    fields: [id, amount]\n", attrWriterScript)
	writePipelineFolder(t, ws, "attr_atomic",
		"name: attr_atomic\nrun: [python, main.py]\nwrites:\n  - table: attrdata.ledger\n    fields: [id, amount]\n", attrAtomicScript)
}

// awaitCount polls a single-count query until it returns want or the deadline
// passes, failing loudly on timeout. Readiness is a recorded database condition,
// never elapsed time; the interval only keeps the poll from spinning.
func awaitCount(ctx context.Context, t *testing.T, conn *pgx.Conn, want int64, within time.Duration, sql string, args ...any) {
	t.Helper()
	dl := time.Now().Add(within)
	var got int64
	for time.Now().Before(dl) {
		if err := conn.QueryRow(ctx, sql, args...).Scan(&got); err != nil {
			t.Fatalf("poll %q: %v", sql, err)
		}
		if got == want {
			return
		}
		time.Sleep(150 * time.Millisecond)
	}
	t.Fatalf("condition %q = %d, want %d within %s", sql, got, want, within)
}

// scanID reads one bigint from a single-row query: the run-id and journal-id
// lookups the legs key their assertions on.
func scanID(ctx context.Context, t *testing.T, conn *pgx.Conn, sql string, args ...any) int64 {
	t.Helper()
	var id int64
	if err := conn.QueryRow(ctx, sql, args...).Scan(&id); err != nil {
		t.Fatalf("read id (%q): %v", sql, err)
	}
	return id
}
