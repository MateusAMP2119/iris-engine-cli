//go:build conformance

package conformance

import (
	"context"
	"fmt"
	"os"
	osexec "os/exec"
	"path/filepath"
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
// PERPETUAL LANE LOOP: the dev run that lands journaled rows, per-pipeline watermarks
// advancing independently across passes, an idle lane chaining cheap no-op passes,
// forced failure with dead-lettering and depends_on propagation while the composer-only
// member still runs, and run cancel that ends a hung pipeline and lets the lane proceed.
// All assertions are at conformance tier against the real binary, a live daemon (with
// the wired lane loop), and real Postgres.
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

	// noopScript is a cheap successful script.
	noopScript := "import sys\nprint(\"noop\")\nsys.exit(0)\n"

	// writerScript writes one row into schema.table via psql on the injected IRIS_DB_URL,
	// so the capture trigger attributes the write to the run.
	writerScript := func(schema, table string) string {
		return fmt.Sprintf(`import os, subprocess, sys, uuid
def main():
    url = os.environ.get("IRIS_DB_URL", "")
    if not url:
        print("missing IRIS_DB_URL", file=sys.stderr); sys.exit(2)
    rid = str(uuid.uuid4()); cid = str(uuid.uuid4())
    sql = "INSERT INTO %s.%s (id, customer_id, amount) VALUES ('%%s','%%s', 42);" %% (rid, cid)
    subprocess.check_call(["psql", url, "-v", "ON_ERROR_STOP=1", "-c", sql])
if __name__ == "__main__": main()
`, schema, table)
	}

	t.Run("dev-run-rows-journaled", func(t *testing.T) {
		bin, ws, cleanup := setupLane(t)
		defer cleanup()
		writeScript(t, ws, "extract_orders", writerScript("raw", "orders_staging"))
		writeScript(t, ws, "reset_counters", noopScript)
		writeScript(t, ws, "load_orders", writerScript("analytics", "orders"))
		applyIngest(t, bin, ws)

		meta := openMeta(t, ws)
		// The lane loop drives all three; wait for a succeeded run of each.
		extractID := waitState(t, meta, "extract_orders", "succeeded", 90*time.Second)
		waitState(t, meta, "reset_counters", "succeeded", 60*time.Second)
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
			t.Errorf("raw.orders_staging rows after dev lane run = 0; want >0")
		}
		if anaC == 0 {
			t.Errorf("analytics.orders rows after dev lane run = 0; want >0")
		}
		// The rows must be recorded in the data journal, attributed to their run.
		var jExtract, jLoad int
		_ = dconn.QueryRow(context.Background(), "SELECT count(*) FROM public.data_journal WHERE run_id=$1", extractID).Scan(&jExtract)
		_ = dconn.QueryRow(context.Background(), "SELECT count(*) FROM public.data_journal WHERE run_id=$1", loadID).Scan(&jLoad)
		if jExtract == 0 && jLoad == 0 {
			t.Errorf("data_journal has no rows attributed to extract run %d or load run %d; writes must be journaled", extractID, loadID)
		}
	})

	t.Run("per-pipeline-watermark", func(t *testing.T) {
		bin, ws, cleanup := setupLane(t)
		defer cleanup()
		writeScript(t, ws, "extract_orders", noopScript)
		writeScript(t, ws, "reset_counters", noopScript)
		writeScript(t, ws, "load_orders", noopScript)
		applyIngest(t, bin, ws)

		meta := openMeta(t, ws)
		pipes := []string{"extract_orders", "reset_counters", "load_orders"}
		base := map[string]int64{}
		for _, p := range pipes {
			base[p] = waitState(t, meta, p, "succeeded", 90*time.Second)
		}
		// Each pipeline must advance its OWN watermark: a strictly newer succeeded run,
		// resolved independently per pipeline (not one shared counter).
		for _, p := range pipes {
			waitSucceededAfter(t, meta, p, base[p], 60*time.Second)
		}
	})

	t.Run("idle-lane-chains-noop-passes", func(t *testing.T) {
		bin, ws, cleanup := setupLane(t)
		defer cleanup()
		writeScript(t, ws, "extract_orders", noopScript)
		writeScript(t, ws, "reset_counters", noopScript)
		writeScript(t, ws, "load_orders", noopScript)
		applyIngest(t, bin, ws)

		meta := openMeta(t, ws)
		// An idle lane keeps chaining passes: the succeeded-run count for a member climbs
		// pass over pass, and every run exits 0 (cheap no-op). Observe several successive
		// advances within a tight bound.
		prev := waitState(t, meta, "reset_counters", "succeeded", 90*time.Second)
		for i := 0; i < 3; i++ {
			prev = waitSucceededAfter(t, meta, "reset_counters", prev, 30*time.Second)
		}
		// No no-op run dead-lettered: every idle pass run exited 0.
		var dl int
		_ = meta.QueryRow(context.Background(),
			"SELECT count(*) FROM runs WHERE pipeline='reset_counters' AND state='dead_lettered'").Scan(&dl)
		if dl != 0 {
			t.Errorf("idle no-op runs dead-lettered %d times; want 0 (every no-op run exits 0 cheaply)", dl)
		}
	})

	t.Run("failure-propagates-composer-runs", func(t *testing.T) {
		bin, ws, cleanup := setupLane(t)
		defer cleanup()
		// extract fails; reset is composer-only (no dependency); load depends_on extract.
		writeScript(t, ws, "extract_orders", "import sys\nsys.exit(7)\n")
		writeScript(t, ws, "reset_counters", noopScript)
		writeScript(t, ws, "load_orders", noopScript)
		applyIngest(t, bin, ws)

		meta := openMeta(t, ws)
		// extract dead-letters as a root cause (failed).
		waitState(t, meta, "extract_orders", "dead_lettered", 90*time.Second)
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
		// reset_counters (composer-only ordering, NOT a dependency) still runs and
		// succeeds despite extract's failure.
		waitState(t, meta, "reset_counters", "succeeded", 30*time.Second)
		var resetDL int
		_ = meta.QueryRow(context.Background(),
			"SELECT count(*) FROM runs WHERE pipeline='reset_counters' AND state='dead_lettered'").Scan(&resetDL)
		if resetDL != 0 {
			t.Errorf("reset_counters dead-lettered %d times; composer order is not a dependency, it must still run", resetDL)
		}
	})

	t.Run("run-cancel-lane-proceeds", func(t *testing.T) {
		bin, ws, cleanup := setupLane(t)
		defer cleanup()
		// reset_counters (middle of composer order) hangs, holding its lane; load waits
		// behind it. extract runs+succeeds ahead of it.
		writeScript(t, ws, "extract_orders", noopScript)
		writeScript(t, ws, "reset_counters", "import time\nwhile True:\n    time.sleep(0.2)\n")
		writeScript(t, ws, "load_orders", noopScript)
		applyIngest(t, bin, ws)

		meta := openMeta(t, ws)
		hungID := waitState(t, meta, "reset_counters", "running", 90*time.Second)

		// Cancel the hung run: it must exit 0 and dead-letter the run as stopped.
		bin.Run(t, RunOptions{Args: []string{"run", "cancel", fmt.Sprint(hungID)}, Dir: ws, Timeout: 20 * time.Second}).RequireExit(t, 0)

		var state, reason string
		dl := time.Now().Add(20 * time.Second)
		for time.Now().Before(dl) {
			_ = meta.QueryRow(context.Background(), "SELECT state FROM runs WHERE id=$1", hungID).Scan(&state)
			if state == "dead_lettered" {
				break
			}
			time.Sleep(150 * time.Millisecond)
		}
		if state != "dead_lettered" {
			t.Fatalf("cancelled run %d state=%q, want dead_lettered", hungID, state)
		}
		_ = meta.QueryRow(context.Background(), "SELECT reason FROM dead_letters WHERE run_id=$1", hungID).Scan(&reason)
		if reason != "stopped" {
			t.Errorf("cancelled run %d dead_letters reason=%q, want stopped", hungID, reason)
		}
		// The lane proceeds past the cancelled member: load_orders (composer-after) gets a
		// succeeded run in the pass the cancel freed. The ceiling is generous (event-driven
		// poll of meta for the run row): under -race on a shared cluster a lane pass can take
		// well over the old 45s, so wait up to 120s for the condition rather than flaking on
		// elapsed time.
		waitState(t, meta, "load_orders", "succeeded", 120*time.Second)
	})
}
