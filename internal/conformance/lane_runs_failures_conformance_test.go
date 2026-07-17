//go:build conformance

package conformance

import (
	"context"
	"fmt"
	"os"
	osexec "os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
)

// ensurePython guarantees a bare `python` resolves on PATH for the daemon's run
// subprocesses (the golden pipelines declare run: [python, main.py]). Where only
// python3 exists (a common dev/CI mismatch) it shims a python->python3 symlink onto
// PATH; the detached daemon inherits this PATH (conformance runs inherit os.Environ),
// so its run subprocesses resolve the interpreter.
func ensurePython(t *testing.T) {
	t.Helper()
	if _, err := osexec.LookPath("python"); err == nil {
		return
	}
	py3, err := osexec.LookPath("python3")
	if err != nil {
		t.Skip("neither python nor python3 on PATH; golden pipeline runs need a Python interpreter")
	}
	dir := t.TempDir()
	if err := os.Symlink(py3, filepath.Join(dir, "python")); err != nil {
		t.Fatalf("shim python->python3: %v", err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
}

// TestGoldenLaneRunsAndFailures drives the golden sample ingest lane (the three
// pipelines under the ingest composer) through its run-lifecycle scenarios via the
// PERPETUAL LANE LOOP under the turn protocol (#206): producing turns whose rows the
// ENGINE writes and journals with exact run attribution (the pipeline holds no
// database credentials), per-pipeline watermarks advancing for producers while a
// quiet member records nothing, an all-quiet lane recording nothing at all, worker
// death dead-lettering with depends_on propagation while the quiet composer-only
// member still takes its turn, pipeline stop freeing a lane held by a hung turn
// (a hung loop turn has no run row for `iris run cancel` to target), and an
// enqueued lane-member manual run executing as one recorded turn. All assertions
// are at conformance tier against the real binary, a live daemon (with the wired
// lane loop), and real Postgres.
//
// Each subtest freshens the shared cluster first so one scenario's failures never
// poison the next.
func TestGoldenLaneRunsAndFailures(t *testing.T) {
	// setupLane freshens the databases, brings up a leader on a fresh golden workspace,
	// and returns the workspace plus a cleanup that stops the daemon. The caller writes
	// scripts, then applies the ingest graph; the perpetual lane loop dispatches it.
	setupLane := func(t *testing.T) (bin *Binary, ws string, cleanup func()) {
		t.Helper()
		ensurePython(t)
		freshDatabases(t)
		bin = Build(t)
		ws = shortWorkspace(t)
		copyGoldenWorkspace(t, ws)
		socket := filepath.Join(ws, ".iris", "iris.sock")

		bin.Run(t, RunOptions{Args: []string{"engine", "install"}, Dir: ws, Timeout: 5 * time.Minute}).RequireExit(t, 0)
		bin.Run(t, RunOptions{Args: []string{"engine", "start", "-d"}, Dir: ws, Timeout: 2 * time.Minute}).RequireExit(t, 0)
		cleanup = func() {
			bin.Run(t, RunOptions{Args: []string{"engine", "stop"}, Dir: ws, Timeout: 30 * time.Second})
		}

		readyCtx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
		if err := WaitForSocket(readyCtx, socket); err != nil {
			cancel()
			cleanup()
			t.Fatalf("daemon socket never became ready: %v", err)
		}
		cancel()
		if !waitForLeader(t, socket) {
			cleanup()
			t.Fatal("daemon never became leader")
		}
		return bin, ws, cleanup
	}

	// applyIngest applies the ingest composer and its three members upstream-first, so
	// the graph is registered and the lane loop picks it up on its next pass.
	applyIngest := func(t *testing.T, bin *Binary, ws string) {
		t.Helper()
		for _, tgt := range []string{
			"pipelines/ingest",
			"pipelines/ingest/extract_orders",
			"pipelines/ingest/reset_counters",
			"pipelines/ingest/load_orders",
		} {
			bin.Run(t, RunOptions{Args: []string{"declare", "apply", tgt}, Dir: ws, Timeout: time.Minute}).RequireExit(t, 0)
		}
	}

	// writeScript overwrites a pipeline's main.py under the copied golden tree BEFORE the
	// graph is applied, so the very first lane pass runs the intended behavior.
	writeScript := func(t *testing.T, ws, pipe, body string) {
		t.Helper()
		p := filepath.Join(ws, "pipelines", "ingest", pipe, "main.py")
		if err := os.WriteFile(p, []byte(body), 0o644); err != nil { //nolint:gosec // G306: workspace script, world-readable by design for dev runs.
			t.Fatalf("write script for %s: %v", pipe, err)
		}
	}

	openMeta := func(t *testing.T, ws string) *pgx.Conn {
		t.Helper()
		conn, err := pgx.Connect(context.Background(), metaDSN(t, ws))
		if err != nil {
			t.Fatalf("connect meta: %v", err)
		}
		t.Cleanup(func() { _ = conn.Close(context.Background()) })
		return conn
	}

	// latestSucceeded returns the highest succeeded run id for a pipeline (0 if none).
	latestSucceeded := func(conn *pgx.Conn, pipeline string) int64 {
		var id int64
		_ = conn.QueryRow(context.Background(),
			"SELECT coalesce(max(id),0) FROM runs WHERE pipeline=$1 AND state='succeeded'", pipeline).Scan(&id)
		return id
	}

	// waitSucceededAfter waits until a pipeline has a succeeded run with id strictly
	// greater than after, returning it. Proves the loop chained another pass for that
	// pipeline.
	waitSucceededAfter := func(t *testing.T, conn *pgx.Conn, pipeline string, after int64, deadline time.Duration) int64 {
		t.Helper()
		dl := time.Now().Add(deadline)
		for time.Now().Before(dl) {
			if id := latestSucceeded(conn, pipeline); id > after {
				return id
			}
			time.Sleep(150 * time.Millisecond)
		}
		t.Fatalf("no succeeded run for %s beyond id %d within %s", pipeline, after, deadline)
		return 0
	}

	// waitState waits until a pipeline has a run in the given state, returning the latest
	// such run id.
	waitState := func(t *testing.T, conn *pgx.Conn, pipeline, state string, deadline time.Duration) int64 {
		t.Helper()
		dl := time.Now().Add(deadline)
		var id int64
		for time.Now().Before(dl) {
			_ = conn.QueryRow(context.Background(),
				"SELECT coalesce(max(id),0) FROM runs WHERE pipeline=$1 AND state=$2", pipeline, state).Scan(&id)
			if id != 0 {
				return id
			}
			time.Sleep(150 * time.Millisecond)
		}
		t.Fatalf("no %s run for %s within %s", state, pipeline, deadline)
		return 0
	}

	// waitFile waits until path exists; the frame-protocol scripts use files as their
	// observable because a quiet or hung turn records nothing in meta (#206).
	waitFile := func(t *testing.T, path string, deadline time.Duration) {
		t.Helper()
		dl := time.Now().Add(deadline)
		for time.Now().Before(dl) {
			if _, err := os.Stat(path); err == nil {
				return
			}
			time.Sleep(150 * time.Millisecond)
		}
		t.Fatalf("file %s never appeared within %s", path, deadline)
	}

	// quietScript is the turn-protocol no-op: a resident answering every turn with a
	// bare done. A quiet turn records NOTHING (#206) -- no run row, no watermark bump --
	// so a quiet member never appears in runs; absence is the record.
	quietScript := PyTurnPrelude + `
def on_turn(turn, rows):
    done(turn)

turn_loop(on_turn)
`

	// markerQuietScript is quietScript plus an observable: it appends each turn number
	// to turns.marker in the pipeline folder before answering done, so a leg can prove
	// the loop drove turns that recorded nothing.
	markerQuietScript := PyTurnPrelude + `
def on_turn(turn, rows):
    with open("turns.marker", "a") as f:
        f.write(str(turn) + "\n")
    done(turn)

turn_loop(on_turn)
`

	// writerScript is a frame-speaking producer: per turn it answers one declared-write
	// row keyed by a fresh uuid and echoes done. It opens no database connection and
	// reads no IRIS_DB_URL (gone in #206) -- the ENGINE upserts the row on its own
	// admin connection inside the turn's data transaction, attributed to the run.
	writerScript := func(table string) string {
		return PyTurnPrelude + fmt.Sprintf(`
import uuid

def on_turn(turn, rows):
    emit(%q, {"id": str(uuid.uuid4()), "customer_id": str(uuid.uuid4()), "amount": 42})
    done(turn)

turn_loop(on_turn)
`, table)
	}

	t.Run("dev-run-rows-journaled", func(t *testing.T) {
		bin, ws, cleanup := setupLane(t)
		defer cleanup()
		writeScript(t, ws, "extract_orders", writerScript("raw.orders_staging"))
		writeScript(t, ws, "reset_counters", quietScript)
		writeScript(t, ws, "load_orders", writerScript("analytics.orders"))
		applyIngest(t, bin, ws)

		meta := openMeta(t, ws)
		// The lane loop drives the pass; the two producers mint run rows at commit.
		// reset_counters is quiet, so it records nothing to wait on (#206).
		extractID := waitState(t, meta, "extract_orders", "succeeded", 90*time.Second)
		loadID := waitState(t, meta, "load_orders", "succeeded", 60*time.Second)

		dconn, err := pgx.Connect(context.Background(), dataDSN(t, ws))
		if err != nil {
			t.Fatalf("data connect: %v", err)
		}
		defer dconn.Close(context.Background())

		var rawC, anaC int
		_ = dconn.QueryRow(context.Background(), "SELECT count(*) FROM raw.orders_staging").Scan(&rawC)
		_ = dconn.QueryRow(context.Background(), "SELECT count(*) FROM analytics.orders").Scan(&anaC)
		if rawC == 0 {
			t.Errorf("raw.orders_staging rows after dev lane run = 0; want >0 (engine-mediated row frames land)")
		}
		if anaC == 0 {
			t.Errorf("analytics.orders rows after dev lane run = 0; want >0 (engine-mediated row frames land)")
		}
		// Exact attribution (#206): the engine commits each turn's rows with SET LOCAL
		// iris.run_id, so each producing run journals exactly its own fresh-uuid insert
		// keyed by its own run id.
		var jExtract, jLoad int
		_ = dconn.QueryRow(context.Background(), "SELECT count(*) FROM public.data_journal WHERE run_id=$1", extractID).Scan(&jExtract)
		_ = dconn.QueryRow(context.Background(), "SELECT count(*) FROM public.data_journal WHERE run_id=$1", loadID).Scan(&jLoad)
		if jExtract != 1 {
			t.Errorf("data_journal rows attributed to extract run %d = %d; want exactly its own 1", extractID, jExtract)
		}
		if jLoad != 1 {
			t.Errorf("data_journal rows attributed to load run %d = %d; want exactly its own 1", loadID, jLoad)
		}
	})

	t.Run("per-pipeline-watermark", func(t *testing.T) {
		bin, ws, cleanup := setupLane(t)
		defer cleanup()
		writeScript(t, ws, "extract_orders", writerScript("raw.orders_staging"))
		writeScript(t, ws, "reset_counters", quietScript)
		writeScript(t, ws, "load_orders", writerScript("analytics.orders"))
		applyIngest(t, bin, ws)

		meta := openMeta(t, ws)
		// Each PRODUCING pipeline must advance its OWN watermark: a strictly newer
		// succeeded run, resolved independently per pipeline (not one shared counter).
		producers := []string{"extract_orders", "load_orders"}
		base := map[string]int64{}
		for _, p := range producers {
			base[p] = waitState(t, meta, p, "succeeded", 90*time.Second)
		}
		for _, p := range producers {
			waitSucceededAfter(t, meta, p, base[p], 60*time.Second)
		}
		// The quiet member's watermark never moves because nothing is ever minted for
		// it (#206): its turns end done with no rows, and a quiet turn records NOTHING
		// while its producing siblings keep chaining.
		var quietRuns int
		_ = meta.QueryRow(context.Background(),
			"SELECT count(*) FROM runs WHERE pipeline='reset_counters'").Scan(&quietRuns)
		if quietRuns != 0 {
			t.Errorf("quiet member reset_counters has %d run rows; want 0 (a quiet turn mints nothing)", quietRuns)
		}
	})

	t.Run("quiet-lane-records-nothing", func(t *testing.T) {
		bin, ws, cleanup := setupLane(t)
		defer cleanup()
		writeScript(t, ws, "extract_orders", markerQuietScript)
		writeScript(t, ws, "reset_counters", markerQuietScript)
		writeScript(t, ws, "load_orders", markerQuietScript)
		applyIngest(t, bin, ws)

		meta := openMeta(t, ws)
		pipes := []string{"extract_orders", "reset_counters", "load_orders"}
		// The successor of the old idle no-op chain (#206): the loop still drives the
		// ungated members' turns (the markers are the proof, since a quiet turn
		// leaves no run row), but a quiet turn writes NOTHING -- no run row, no
		// watermark bump -- so an all-quiet lane parks instead of chaining passes
		// forever. load_orders is edge-gated on extract_orders, whose quiet turns
		// mint no runs, so its gate never opens and it never takes a turn at all --
		// its zero run rows below are that gate's own proof.
		for _, p := range []string{"extract_orders", "reset_counters"} {
			waitFile(t, filepath.Join(ws, "pipelines", "ingest", p, "turns.marker"), 90*time.Second)
		}
		// A short settle so a contract break (a quiet turn minting a row) has passes in
		// which to surface before the negative assertion reads.
		time.Sleep(2 * time.Second)
		var runs int
		_ = meta.QueryRow(context.Background(),
			"SELECT count(*) FROM runs WHERE pipeline = ANY($1)", pipes).Scan(&runs)
		if runs != 0 {
			t.Errorf("all-quiet lane minted %d run rows; want 0 (quiet turns record nothing, dead-letter nothing)", runs)
		}
	})

	t.Run("failure-propagates-composer-runs", func(t *testing.T) {
		bin, ws, cleanup := setupLane(t)
		defer cleanup()
		// extract DIES: a bare exit with no terminal frame is a worker death under the
		// turn protocol, dead-lettering the turn (a crashing script, not a declared
		// {"event":"error"} failure). reset is composer-only (no dependency) and quiet;
		// load depends_on extract and never gets to run.
		writeScript(t, ws, "extract_orders", "import sys\nsys.exit(7)\n")
		writeScript(t, ws, "reset_counters", markerQuietScript)
		writeScript(t, ws, "load_orders", quietScript)
		applyIngest(t, bin, ws)

		meta := openMeta(t, ws)
		// extract dead-letters as a root cause: the failed turn mints its run directly
		// dead_lettered (reason failed) with the death disposition in the detail.
		extractID := waitState(t, meta, "extract_orders", "dead_lettered", 90*time.Second)
		var rootReason, rootErr string
		if err := meta.QueryRow(context.Background(),
			"SELECT reason, coalesce(error,'') FROM dead_letters WHERE run_id=$1", extractID).Scan(&rootReason, &rootErr); err != nil {
			t.Fatalf("read extract dead_letters entry: %v", err)
		}
		if rootReason != "failed" {
			t.Errorf("extract dead_letters reason = %q; want failed (root cause)", rootReason)
		}
		if !strings.Contains(rootErr, "worker died mid-turn") || !strings.Contains(rootErr, "exit code 7") {
			t.Errorf("extract dead_letters error = %q; want the death detail (worker died mid-turn ... exit code 7)", rootErr)
		}
		// load dead-letters by PROPAGATION: a never-executed run, reason
		// upstream_dead_lettered, failed_upstream = extract_orders.
		loadID := waitState(t, meta, "load_orders", "dead_lettered", 60*time.Second)
		var reason, failedUpstream string
		if err := meta.QueryRow(context.Background(),
			"SELECT reason, coalesce(failed_upstream,'') FROM dead_letters WHERE run_id=$1", loadID).Scan(&reason, &failedUpstream); err != nil {
			t.Fatalf("read load dead_letters entry: %v", err)
		}
		if reason != "upstream_dead_lettered" {
			t.Errorf("load dead_letters reason = %q; want upstream_dead_lettered (propagation)", reason)
		}
		if failedUpstream != "extract_orders" {
			t.Errorf("load dead_letters failed_upstream = %q; want extract_orders", failedUpstream)
		}
		// reset_counters (composer-only ordering, NOT a dependency) still takes its
		// turn despite extract's death -- the marker is the proof, since its quiet
		// turn records nothing -- and nothing is minted for it: no propagation row,
		// no dead letter.
		waitFile(t, filepath.Join(ws, "pipelines", "ingest", "reset_counters", "turns.marker"), 30*time.Second)
		var resetRuns int
		_ = meta.QueryRow(context.Background(),
			"SELECT count(*) FROM runs WHERE pipeline='reset_counters'").Scan(&resetRuns)
		if resetRuns != 0 {
			t.Errorf("reset_counters has %d run rows; composer order is not a dependency, its quiet turn must record nothing", resetRuns)
		}
	})

	t.Run("pipeline-stop-frees-hung-lane", func(t *testing.T) {
		bin, ws, cleanup := setupLane(t)
		defer cleanup()
		// reset_counters (middle of composer order) hangs mid-turn -- it answers
		// rows+run with no terminal frame, forever -- holding its lane; load waits
		// behind it. extract produces ahead of it so load's depends_on gate has a
		// success to consume once the lane is freed. A hung LOOP turn has NO run row
		// (#206), so `iris run cancel <id>` has nothing to target; the operator
		// surface is `iris pipeline stop <name>`, which parks the pipeline (the loop
		// does not resurrect it) and kills the resident worker.
		writeScript(t, ws, "extract_orders", writerScript("raw.orders_staging"))
		writeScript(t, ws, "reset_counters", PyTurnPrelude+`
import time

def on_turn(turn, rows):
    open("hang.marker", "w").close()
    while True:
        time.sleep(0.2)

turn_loop(on_turn)
`)
		writeScript(t, ws, "load_orders", writerScript("analytics.orders"))
		applyIngest(t, bin, ws)

		meta := openMeta(t, ws)
		// The hung turn leaves no run row; the marker file is the observable proof the
		// worker took its turn and is now holding the lane.
		waitFile(t, filepath.Join(ws, "pipelines", "ingest", "reset_counters", "hang.marker"), 90*time.Second)
		var hungRuns int
		_ = meta.QueryRow(context.Background(),
			"SELECT count(*) FROM runs WHERE pipeline='reset_counters'").Scan(&hungRuns)
		if hungRuns != 0 {
			t.Fatalf("hung loop turn minted %d run rows; want 0 (nothing for iris run cancel to target)", hungRuns)
		}

		// The pipeline-level stop parks: reset has no run at all, so the stop MINTS
		// the park row (a never-executed dead_lettered stopped run) and kills the hung
		// worker; the kill itself mints nothing over the park.
		bin.Run(t, RunOptions{Args: []string{"pipeline", "stop", "reset_counters"}, Dir: ws, Timeout: 20 * time.Second}).RequireExit(t, 0)

		var state, reason string
		dl := time.Now().Add(20 * time.Second)
		for time.Now().Before(dl) {
			_ = meta.QueryRow(context.Background(),
				"SELECT r.state, coalesce(d.reason,'') FROM runs r LEFT JOIN dead_letters d ON d.run_id=r.id WHERE r.pipeline='reset_counters' ORDER BY r.id DESC LIMIT 1").Scan(&state, &reason)
			if state == "dead_lettered" && reason == "stopped" {
				break
			}
			time.Sleep(150 * time.Millisecond)
		}
		if state != "dead_lettered" || reason != "stopped" {
			t.Fatalf("pipeline stop park row = (state %q, reason %q), want a dead_lettered stopped run", state, reason)
		}
		// The lane proceeds past the stopped member: load_orders (composer-after) gets
		// a succeeded producing run in the pass the stop freed. The ceiling is generous
		// (event-driven poll of meta for the run row): under -race on a shared cluster
		// a lane pass can take well over 45s, so wait up to 120s for the condition
		// rather than flaking on elapsed time.
		waitState(t, meta, "load_orders", "succeeded", 120*time.Second)
	})

	t.Run("lane-member-manual-run-executes-in-turn", func(t *testing.T) {
		bin, ws, cleanup := setupLane(t)
		defer cleanup()
		writeScript(t, ws, "extract_orders", quietScript)
		writeScript(t, ws, "reset_counters", quietScript)
		writeScript(t, ws, "load_orders", quietScript)
		applyIngest(t, bin, ws)

		meta := openMeta(t, ws)
		// All members are quiet, so the lane's loop passes record nothing and the lane
		// parks -- there is no succeeded loop run to wait on before enqueueing (#206).
		// A lane member's manual run is ENQUEUED (cause=manual, exit 0) for the lane
		// runner to start at the member's turn; the enqueued queued row is itself the
		// meta cause that wakes the parked lane. It must actually execute as one turn,
		// and unlike a quiet LOOP turn a quiet MANUAL turn still records: manual runs
		// pre-mint queued->running->terminal, so the pre-minted row ends succeeded.
		bin.Run(t, RunOptions{Args: []string{"pipeline", "run", "extract_orders"}, Dir: ws, Timeout: time.Minute}).RequireExit(t, 0)
		dl := time.Now().Add(90 * time.Second)
		var manualID int64
		for time.Now().Before(dl) {
			_ = meta.QueryRow(context.Background(),
				"SELECT coalesce(max(id),0) FROM runs WHERE pipeline='extract_orders' AND cause='manual' AND state='succeeded'").Scan(&manualID)
			if manualID != 0 {
				break
			}
			time.Sleep(150 * time.Millisecond)
		}
		if manualID == 0 {
			t.Fatalf("enqueued lane-member manual run never executed: no succeeded cause=manual run for extract_orders within 90s")
		}
	})
}
