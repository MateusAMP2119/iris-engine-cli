//go:build conformance

package conformance

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/MateusAMP2119/iris-lakehouse/internal/fixtures"
)

// This file is the golden-sample acceptance spine: the three scenario contracts that
// prove the whole engine end to end, unattended, against the shipped binary, a
// running daemon, and real Postgres. It reuses the two-daemon-on-one-meta pattern the
// bare failover legs established (standby_rejection_conformance_test.go's mutation
// rejection, failover_conformance_test.go's leader kill) but drives the GOLDEN
// workspace through the numbered acceptance steps and adds the properties those legs
// do not: reconciliation of the orphaned in-flight run to stopped, lanes resuming on
// the new leader, and the whole scenario running with zero human intervention.
//
// The member pipelines speak the turn protocol (#206): frame-speaking residents
// whose declared writes the ENGINE performs itself (no IRIS_DB_URL, no pipeline
// credentials). Producing turns mint their own run rows at commit; a quiet turn
// (done, no rows) mints nothing at all; a hung LOOP turn holds its lane with no run
// row, so the orphaned in-flight run the failover leg reconciles is a pre-minted
// MANUAL run hung mid-turn.
//
// All three need two candidates on ONE shared meta, so they run only in external
// mode (IRIS_PG_DSN set, the conformance/CI configuration); managed Postgres gives
// each daemon its own cluster, so there is no shared advisory lock to contend for
// and no standby. They skip when no external DSN is present.

// scenarioGoldenTargets is the golden ingest graph applied composer-first, then
// members in composer order -- the four single-file applies of acceptance step 2.
var scenarioGoldenTargets = []string{
	filepath.Join("pipelines", "ingest"),
	filepath.Join("pipelines", "ingest", "extract_orders"),
	filepath.Join("pipelines", "ingest", "reset_counters"),
	filepath.Join("pipelines", "ingest", "load_orders"),
}

// scenarioWriteScript overwrites a golden pipeline's main.py before the graph is
// applied, so the very first lane pass runs the intended behavior.
func scenarioWriteScript(t *testing.T, ws, pipe, body string) {
	t.Helper()
	p := filepath.Join(ws, "pipelines", "ingest", pipe, "main.py")
	if err := os.WriteFile(p, []byte(body), 0o644); err != nil { //nolint:gosec // G306: workspace script, world-readable by design for dev runs.
		t.Fatalf("write script for %s: %v", pipe, err)
	}
}

// scenarioQuietPy is the quiet turn-protocol resident: every turn answers a bare
// done, produces nothing, and so mints no run rows and writes nothing at all --
// the good-citizen no-op member under #206 (the lane chains past it on the other
// members' producing turns).
const scenarioQuietPy = PyTurnPrelude + `
def on_turn(turn, rows):
    done(turn)

turn_loop(on_turn)
`

// scenarioHoldPy is the takeover leg's reset_counters body, in two marker-gated
// phases. Without hold.marker every turn ends in a declared error frame: the fresh
// loop turn dead-letters (reason failed) and the loop's no-retry brake stops
// further fresh turns -- deliberate, because a HUNG loop turn would hold the lane
// with NO run row (#206), leaving reconciliation nothing to see. Once the test
// writes hold.marker, the next turn -- the enqueued manual run's, picked up at the
// lane boundary against its pre-minted row -- hangs mid-turn forever, a real
// running run for the failover reconciliation to dead-letter.
const scenarioHoldPy = "import os, time\n" + PyTurnPrelude + `
def on_turn(turn, rows):
    if os.path.exists("hold.marker"):
        while True:
            time.sleep(0.2)
    error(turn, "hold brake")

turn_loop(on_turn)
`

// scenarioWriterPy returns a turn-protocol resident that emits one fresh-uuid row
// into schema.table per turn as a declared-write row frame plus done. The ENGINE
// performs the insert on its own admin connection with the run's exact attribution
// (the capture trigger journals under the turn's run id), so every turn is a
// producing turn and mints its own succeeded run at commit -- the golden
// dev/disposable run of acceptance step 3. The pipeline holds no credentials.
func scenarioWriterPy(schema, table string) string {
	return "import uuid\n" + PyTurnPrelude + fmt.Sprintf(`
def on_turn(turn, rows):
    emit("%s.%s", {"id": str(uuid.uuid4()), "customer_id": str(uuid.uuid4()), "amount": 42})
    done(turn)

turn_loop(on_turn)
`, schema, table)
}

// scenarioAdaptGolden overwrites a golden workspace's three member bodies with
// turn-protocol residents before the graph is applied: producing writers for
// extract_orders (raw.orders_staging) and load_orders (analytics.orders), and the
// caller's chosen body for reset_counters (quiet, or the takeover leg's hold
// script). Both failover workspaces get the same bodies, so whichever daemon leads
// runs protocol-correct residents from its own folders.
func scenarioAdaptGolden(t *testing.T, ws, resetBody string) {
	t.Helper()
	scenarioWriteScript(t, ws, "extract_orders", scenarioWriterPy("raw", "orders_staging"))
	scenarioWriteScript(t, ws, "reset_counters", resetBody)
	scenarioWriteScript(t, ws, "load_orders", scenarioWriterPy("analytics", "orders"))
}

// scMetaConn opens a pgx connection to the shared meta database of a workspace's
// daemon, closed at test end.
func scMetaConn(t *testing.T, ws string) *pgx.Conn {
	t.Helper()
	conn, err := pgx.Connect(context.Background(), metaDSN(t, ws))
	if err != nil {
		t.Fatalf("connect meta: %v", err)
	}
	t.Cleanup(func() { _ = conn.Close(context.Background()) })
	return conn
}

// scMaxSucceeded returns the highest succeeded run id for a pipeline (0 if none).
func scMaxSucceeded(conn *pgx.Conn, pipeline string) int64 {
	var id int64
	_ = conn.QueryRow(context.Background(),
		"SELECT coalesce(max(id),0) FROM runs WHERE pipeline=$1 AND state='succeeded'", pipeline).Scan(&id)
	return id
}

// scWaitRunState waits until a pipeline has a run in state, returning the latest
// such run id. Readiness is the observed state, never elapsed time.
func scWaitRunState(t *testing.T, conn *pgx.Conn, pipeline, state string, deadline time.Duration) int64 {
	t.Helper()
	dl := time.Now().Add(deadline)
	for time.Now().Before(dl) {
		var id int64
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

// scWaitSucceededAfter waits until a pipeline has a succeeded run strictly newer
// than after, proving the lane chained another pass for it.
func scWaitSucceededAfter(t *testing.T, conn *pgx.Conn, pipeline string, after int64, deadline time.Duration) int64 {
	t.Helper()
	dl := time.Now().Add(deadline)
	for time.Now().Before(dl) {
		if id := scMaxSucceeded(conn, pipeline); id > after {
			return id
		}
		time.Sleep(150 * time.Millisecond)
	}
	t.Fatalf("no succeeded run for %s beyond id %d within %s", pipeline, after, deadline)
	return 0
}

// scStartLeader installs (external no-op) and starts a detached daemon on ws, waits
// for its socket and confirmed leadership, and registers a stop cleanup.
func scStartLeader(t *testing.T, bin *Binary, ws string) string {
	t.Helper()
	socket := filepath.Join(ws, ".iris", "iris.sock")
	bin.Run(t, RunOptions{Args: []string{"engine", "install"}, Dir: ws, Timeout: 5 * time.Minute}).RequireExit(t, 0)
	bin.Run(t, RunOptions{Args: []string{"engine", "start", "-d"}, Dir: ws, Timeout: 2 * time.Minute}).RequireExit(t, 0)
	t.Cleanup(func() {
		bin.Run(t, RunOptions{Args: []string{"engine", "stop"}, Dir: ws, Timeout: 30 * time.Second})
	})
	readyCtx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	if err := WaitForSocket(readyCtx, socket); err != nil {
		cancel()
		t.Fatalf("leader socket never became ready: %v", err)
	}
	cancel()
	if !waitForLeader(t, socket) {
		t.Fatalf("daemon never became leader (role=%q)", healthzRole(t, socket))
	}
	return socket
}

// scStartStandby starts a second detached daemon on ws against the shared meta,
// waits for its socket and the standby role, and registers a stop cleanup. It does
// not install: the shared meta already exists.
func scStartStandby(t *testing.T, bin *Binary, ws string) string {
	t.Helper()
	socket := filepath.Join(ws, ".iris", "iris.sock")
	bin.Run(t, RunOptions{Args: []string{"engine", "start", "-d"}, Dir: ws, Timeout: 2 * time.Minute}).RequireExit(t, 0)
	t.Cleanup(func() {
		bin.Run(t, RunOptions{Args: []string{"engine", "stop"}, Dir: ws, Timeout: 30 * time.Second})
	})
	readyCtx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	if err := WaitForSocket(readyCtx, socket); err != nil {
		cancel()
		t.Fatalf("standby socket never became ready: %v", err)
	}
	cancel()
	if !waitForStandby(t, socket) {
		t.Fatalf("second candidate never reported standby (role=%q); it must block on the held lock", healthzRole(t, socket))
	}
	return socket
}

// TestGoldenFailoverStandbyTakeover is the golden-sample failover leg of acceptance
// step 9. Two candidates share one meta over the golden workspace; a manual run of a
// golden pipeline hangs mid-turn on the leader, leaving a real in-flight run (a hung
// LOOP turn would leave no run row under #206, so the orphan is a pre-minted manual
// run). Killing the leader abruptly (host-loss simulation) drops its meta session
// and releases the advisory lock: the standby acquires it, runs startup
// reconciliation (the orphaned running run is dead-lettered stopped), reports itself
// leader, and resumes lanes (a fresh succeeded producing run lands on the new
// leader). This is the golden version -- richer than the bare takeover leg in
// failover_conformance_test.go by proving reconciliation AND lane resumption over
// the sample graph.
func TestGoldenFailoverStandbyTakeover(t *testing.T) {
	// Two candidates share one meta: the suite-owned embedded cluster (or an
	// ambient IRIS_PG_DSN).
	requireSharedCluster(t)
	ensurePython(t)
	freshDatabases(t)
	bin := Build(t)

	// Two distinct workspaces (distinct sockets, pidfiles, objects roots) share one
	// external meta -- two hosts, one meta. Both get the turn-protocol bodies:
	// extract_orders and load_orders produce a row every turn (each turn mints its
	// own succeeded run -- the resume signal), and reset_counters runs the hold
	// script (brake first, marker-gated hang on the manual turn).
	wsLeader := shortWorkspace(t)
	wsStandby := shortWorkspace(t)
	copyGoldenWorkspace(t, wsLeader)
	copyGoldenWorkspace(t, wsStandby)
	scenarioAdaptGolden(t, wsLeader, scenarioHoldPy)
	scenarioAdaptGolden(t, wsStandby, scenarioHoldPy)

	scStartLeader(t, bin, wsLeader)
	leaderPIDPath := filepath.Join(wsLeader, ".iris", "iris.pid")

	// Apply the golden ingest graph on the leader; the perpetual lane loop picks it up.
	for _, tgt := range scenarioGoldenTargets {
		bin.Run(t, RunOptions{Args: []string{"declare", "apply", tgt}, Dir: wsLeader, Timeout: time.Minute}).RequireExit(t, 0)
	}

	meta := scMetaConn(t, wsLeader)
	// extract_orders (composer-first, producing) succeeds; reset_counters' first
	// fresh turn errors by design (dead-lettered failed, and the no-retry brake
	// stops its fresh loop turns), so the lane keeps chaining past it.
	scWaitRunState(t, meta, "extract_orders", "succeeded", 90*time.Second)
	scWaitRunState(t, meta, "reset_counters", "dead_lettered", 60*time.Second)

	// Arm the hang and enqueue the manual run: the lane-member manual is minted
	// queued (exit 0) and picked up at reset_counters' next lane boundary against
	// its pre-minted row -- marked running, then hung mid-turn by the marker. That
	// running row is the orphan-to-be, and the hang holds the lane, so the extract
	// baseline read after it is the leader's final word.
	if err := os.WriteFile(filepath.Join(wsLeader, "pipelines", "ingest", "reset_counters", "hold.marker"), nil, 0o644); err != nil {
		t.Fatalf("arm hold marker: %v", err)
	}
	bin.Run(t, RunOptions{Args: []string{"pipeline", "run", "reset_counters"}, Dir: wsLeader, Timeout: time.Minute}).RequireExit(t, 0)
	orphanID := scWaitRunState(t, meta, "reset_counters", "running", 90*time.Second)
	extractBaseline := scMaxSucceeded(meta, "extract_orders")

	// Bring up the standby on the shared meta and confirm it blocks as standby.
	standbySock := scStartStandby(t, bin, wsStandby)

	// Kill the leader abruptly: SIGKILL the process (host loss), not a graceful stop.
	// Its Postgres session drops, releasing the session-level advisory lock.
	leaderPID := readDaemonPID(t, leaderPIDPath)
	if err := syscall.Kill(leaderPID, syscall.SIGKILL); err != nil {
		t.Fatalf("SIGKILL leader (pid %d): %v", leaderPID, err)
	}

	// Takeover: the standby's blocked pg_advisory_lock returns once the leader's session
	// is gone; it acquires the lock, reconciles, and becomes the leader.
	if !waitForLeader(t, standbySock) {
		t.Fatalf("standby did not take over after the leader was killed (role=%q); the freed advisory lock must promote it", healthzRole(t, standbySock))
	}

	// Reconciliation: the orphaned in-flight run is dead-lettered stopped. The new
	// leader must not dispatch on an unreconciled meta, so this settles promptly.
	var state, reason, detail string
	dl := time.Now().Add(30 * time.Second)
	for time.Now().Before(dl) {
		_ = meta.QueryRow(context.Background(), "SELECT state FROM runs WHERE id=$1", orphanID).Scan(&state)
		if state == "dead_lettered" {
			break
		}
		time.Sleep(150 * time.Millisecond)
	}
	if state != "dead_lettered" {
		t.Fatalf("orphaned in-flight run %d state=%q after takeover, want dead_lettered (startup reconciliation must terminal it)", orphanID, state)
	}
	_ = meta.QueryRow(context.Background(), "SELECT reason, coalesce(error,'') FROM dead_letters WHERE run_id=$1", orphanID).Scan(&reason, &detail)
	if reason != "stopped" {
		t.Errorf("orphaned run %d dead_letters reason=%q after takeover, want stopped", orphanID, reason)
	}
	// A crash-reconciliation stop carries the daemon-terminated detail, never the
	// operator-cancel detail -- the loop resumes past it rather than parking (#206).
	if !strings.Contains(detail, "daemon terminated") {
		t.Errorf("orphaned run %d dead_letters error=%q, want the daemon-terminated crash-stop detail", orphanID, detail)
	}

	// Lanes resume: the new leader loops the sample graph, so extract_orders gets a
	// producing succeeded run strictly newer than the held leader's final baseline.
	scWaitSucceededAfter(t, meta, "extract_orders", extractBaseline, 90*time.Second)
}

// TestGoldenStandbyMutationExit6 is the golden-sample standby-rejection leg of
// acceptance step 9. Two candidates share one meta over the golden workspace with the
// ingest graph applied; a golden mutation posted to the standby is rejected before it
// reaches a route, exit 6, with the not_leader envelope naming the leader for
// retargeting. This is the golden version (real registered golden pipelines, the
// sample's own mutations), distinct from the bare standby-rejection leg's rejection
// of an unregistered "any_pipeline" (standby_rejection_conformance_test.go).
func TestGoldenStandbyMutationExit6(t *testing.T) {
	requireSharedCluster(t)
	freshDatabases(t)
	bin := Build(t)

	wsLeader := shortWorkspace(t)
	wsStandby := shortWorkspace(t)
	copyGoldenWorkspace(t, wsLeader)
	copyGoldenWorkspace(t, wsStandby)

	scStartLeader(t, bin, wsLeader)
	// Register the golden graph on the leader so the standby's rejected mutations name
	// real sample pipelines, not a bare unknown one.
	for _, tgt := range scenarioGoldenTargets {
		bin.Run(t, RunOptions{Args: []string{"declare", "apply", tgt}, Dir: wsLeader, Timeout: time.Minute}).RequireExit(t, 0)
	}

	standbySock := scStartStandby(t, bin, wsStandby)
	// Reads work on the standby regardless of role, so the socket is genuinely up and
	// the rejections below are the mutation gate, not a dead listener.
	requireHealthzOK(t, standbySock)

	// Every golden control mutation posted to the standby is gated to the leader: the
	// mux rejects any non-safe method on a non-leader role before routing, and the CLI
	// maps not_leader to exit 6. Cover the sample's own mutation surface.
	goldenMutations := [][]string{
		{"declare", "apply", filepath.Join("pipelines", "ingest", "extract_orders")},
		{"pipeline", "promote", "extract_orders"},
		{"workload", "wipe", "extract_orders", "--yes"},
	}
	for _, mut := range goldenMutations {
		args := append([]string{"--socket", standbySock}, mut...)
		res := bin.Run(t, RunOptions{Args: args, Dir: wsStandby, Timeout: 30 * time.Second})
		res.RequireExit(t, 6)
		out := strings.ToLower(string(res.Stdout) + string(res.Stderr))
		if !strings.Contains(out, "leader") {
			t.Errorf("exit-6 rejection of %v did not point to the leader:\nstdout:\n%s\nstderr:\n%s", mut, res.Stdout, res.Stderr)
		}
	}

	// Under --json the single stdout document is the not_leader error envelope: its
	// machine code is not_leader, its message names the leader, and its leader hint is
	// present for retargeting.
	res := bin.Run(t, RunOptions{
		Args:    []string{"--socket", standbySock, "--json", "pipeline", "promote", "extract_orders"},
		Dir:     wsStandby,
		Timeout: 30 * time.Second,
	})
	res.RequireExit(t, 6)
	var env struct {
		Error struct {
			Code    string `json:"code"`
			Message string `json:"message"`
		} `json:"error"`
	}
	res.DecodeJSON(t, &env)
	if env.Error.Code != "not_leader" {
		t.Errorf("--json error code = %q, want not_leader", env.Error.Code)
	}
	if !strings.Contains(strings.ToLower(env.Error.Message), "leader") {
		t.Errorf("--json message did not name the leader for retargeting: %q", env.Error.Message)
	}
}

// TestGoldenScenarioPassesUnattended is the final gate of E13: the golden sample
// passes the acceptance scenario end to end with zero
// human intervention. One continuous run of the shipped binary walks the numbered
// steps in order -- install + start (one code path), the four single-file applies
// (and the bare invocation that exits 2), the perpetual dev lane landing journaled
// rows across passes, the operator's capture-lifecycle mutations (scoped and bare
// wipe, build + promote) all returning unattended, the declared read endpoint served
// to a data PAT, provenance answering for a landed row, and the HA leg (standby
// rejects a mutation with exit 6, the killed leader's lock frees, the standby takes
// over and resumes lanes). Every command is issued non-interactively and asserted;
// nothing waits on a human. The per-step invariants are proven by their own
// dedicated legs -- this is the integrative proof that the whole scenario runs
// unattended.
func TestGoldenScenarioPassesUnattended(t *testing.T) {
	requireSharedCluster(t)
	ensurePython(t)
	freshDatabases(t)
	bin := Build(t)

	ws := shortWorkspace(t)
	copyGoldenWorkspace(t, ws)
	// copyGoldenWorkspace copies only pipelines+schemas; the acceptance scenario also
	// publishes the golden read endpoint, so bring its declaration tree in too.
	if err := copyTree(filepath.Join(fixtures.WorkspaceGolden(), "endpoints"), filepath.Join(ws, "endpoints")); err != nil {
		t.Fatalf("copy golden endpoints tree: %v", err)
	}
	// The sample's dev turns land rows over the turn protocol: extract emits into
	// raw.orders_staging, load into analytics.orders (the endpoint's source) --
	// engine-performed writes, exact run attribution, one run row per producing
	// turn. reset_counters is the quiet resident: it answers every turn with a bare
	// done and therefore mints no run rows at all.
	scenarioAdaptGolden(t, ws, scenarioQuietPy)

	// Step 1: install + start -- one code path, managed locally / external in CI. The
	// daemon comes up and elects leader with no prompt.
	tcpAddr := freeTCPAddr(t)
	socket := filepath.Join(ws, ".iris", "iris.sock")
	bin.Run(t, RunOptions{Args: []string{"engine", "install"}, Dir: ws, Timeout: 5 * time.Minute}).RequireExit(t, 0)
	bin.Run(t, RunOptions{Args: []string{"engine", "start", "-d", "--tcp", tcpAddr}, Dir: ws, Timeout: 2 * time.Minute}).RequireExit(t, 0)
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
		t.Fatalf("daemon never became leader (role=%q)", healthzRole(t, socket))
	}

	// Step 2: the bare apply is a usage error (exit 2), then four single-file applies
	// register the graph, composer-first then members in composer order.
	bin.Run(t, RunOptions{Args: []string{"declare", "apply"}, Dir: ws, Timeout: 30 * time.Second}).RequireExit(t, 2)
	for _, tgt := range scenarioGoldenTargets {
		bin.Run(t, RunOptions{Args: []string{"declare", "apply", tgt}, Dir: ws, Timeout: time.Minute}).RequireExit(t, 0)
	}

	meta := scMetaConn(t, ws)
	// Step 3: the perpetual dev lane drives the producing members to succeeded, rows
	// land in the real tables, and the writes are journaled under each run's own id
	// -- all unattended, no manual run, no pipeline-held credentials.
	extractRun := scWaitRunState(t, meta, "extract_orders", "succeeded", 90*time.Second)
	loadRun := scWaitRunState(t, meta, "load_orders", "succeeded", 60*time.Second)

	data := connectData(t, dataDSN(t, ws))
	var rawRows, anaRows int
	_ = data.QueryRow(context.Background(), "SELECT count(*) FROM raw.orders_staging").Scan(&rawRows)
	_ = data.QueryRow(context.Background(), "SELECT count(*) FROM analytics.orders").Scan(&anaRows)
	if rawRows == 0 || anaRows == 0 {
		t.Fatalf("dev lane landed no rows (raw=%d analytics=%d); the sample run must write real tables", rawRows, anaRows)
	}
	var journaled int
	_ = data.QueryRow(context.Background(),
		"SELECT count(*) FROM public.data_journal WHERE run_id IN ($1,$2)", extractRun, loadRun).Scan(&journaled)
	if journaled == 0 {
		t.Fatalf("no journal stamps attributed to extract run %d or load run %d; writes must be captured", extractRun, loadRun)
	}

	// Step 3 continued (and step 10's idle-chaining shape): the lane keeps looping
	// unattended -- a later pass gives extract a strictly newer succeeded run.
	scWaitSucceededAfter(t, meta, "extract_orders", extractRun, 60*time.Second)

	// The quiet member's contract (#206): a second extract pass began, so the first
	// pass -- reset_counters' quiet turn included -- demonstrably completed, yet its
	// bare-done turns minted NOTHING: no run rows, no watermark bumps, nothing to
	// prune. Quiet loop turns leave no record; only producing turns do.
	var resetRuns int
	_ = meta.QueryRow(context.Background(), "SELECT count(*) FROM runs WHERE pipeline='reset_counters'").Scan(&resetRuns)
	if resetRuns != 0 {
		t.Fatalf("quiet member reset_counters minted %d run rows, want 0 (a quiet turn records nothing)", resetRuns)
	}

	// Step 4: the operator's wipe mutations run non-interactively and succeed against
	// the live sample -- scoped wipe of one pipeline, then the bare wipe of the rest.
	// (The exact revert invariants are proven by their own legs; here they must simply
	// pass unattended.) The lane loop keeps producing back to back, so a wipe can
	// race an in-flight producing turn's brief running row; --force is the
	// documented unattended override, cancelling whatever it finds in flight and
	// proceeding -- and a --force cancellation never parks the loop (its stop detail
	// is not the operator cancel's), so the lane resumes landing rows below. The
	// build+promote half of step 5 and its wipe-immunity invariant are proven over a
	// compile-in-CI pipeline -- the golden sample's Python pipelines build via
	// pyinstaller, which the conformance runner does not carry.
	for _, mut := range [][]string{
		{"workload", "wipe", "extract_orders", "--force"},
		{"workload", "wipe", "--force"},
	} {
		bin.Run(t, RunOptions{Args: mut, Dir: ws, Timeout: 2 * time.Minute}).RequireExit(t, 0)
	}

	// Step 7: publish the declared endpoint and read it as a data PAT over TCP. The lane
	// keeps landing analytics.orders rows, so wait until the promoted/looped table has
	// rows to serve, then read through the show-once token.
	bin.Run(t, RunOptions{Args: []string{"endpoint", "apply"}, Dir: ws, Timeout: time.Minute}).RequireExit(t, 0)
	pat := bin.Run(t, RunOptions{
		Args:    []string{"--json", "pat", "create", "--scope", "data", "--endpoint", "orders_by_customer"},
		Dir:     ws,
		Timeout: time.Minute,
	})
	pat.RequireExit(t, 0)
	var patEnv struct {
		Data struct {
			Token string `json:"token"`
		} `json:"data"`
	}
	pat.DecodeJSON(t, &patEnv)
	if patEnv.Data.Token == "" {
		t.Fatalf("pat create surfaced no show-once token: %s", pat.Stdout)
	}
	scWaitAnalyticsRows(t, data, 60*time.Second)
	code, body := scTCPGet(t, "http://"+tcpAddr+"/q/orders_by_customer", patEnv.Data.Token)
	if code != http.StatusOK {
		t.Fatalf("data PAT read of /q/orders_by_customer = %d, want 200 (body %s)", code, body)
	}

	// Step 8: provenance answers for a landed row -- pick a current analytics.orders pk
	// and assert the walk names an authoring run.
	pk := scAnyAnalyticsPK(t, data)
	prov := bin.Run(t, RunOptions{
		Args:    []string{"--json", "data", "provenance", "analytics.orders", pk},
		Dir:     ws,
		Timeout: time.Minute,
	})
	prov.RequireExit(t, 0)
	var provEnv struct {
		Data struct {
			Author struct {
				RunID int64 `json:"run_id"`
			} `json:"author"`
		} `json:"data"`
	}
	prov.DecodeJSON(t, &provEnv)
	if provEnv.Data.Author.RunID == 0 {
		t.Fatalf("provenance for analytics.orders %s named no authoring run: %s", pk, prov.Stdout)
	}

	// Step 9: HA. A second candidate joins on the same meta as a standby; a mutation
	// against it is rejected with leader guidance and exit 6. Then the leader is killed
	// abruptly; the standby acquires the freed lock, takes over, and resumes lanes --
	// all with no human in the loop. The standby gets the same turn-protocol bodies:
	// after takeover it runs its own folders' scripts, and the resume signal is a
	// producing extract turn minting a fresh succeeded run.
	wsStandby := shortWorkspace(t)
	copyGoldenWorkspace(t, wsStandby)
	scenarioAdaptGolden(t, wsStandby, scenarioQuietPy)
	standbySock := scStartStandby(t, bin, wsStandby)

	rej := bin.Run(t, RunOptions{
		Args:    []string{"--socket", standbySock, "pipeline", "promote", "load_orders"},
		Dir:     wsStandby,
		Timeout: 30 * time.Second,
	})
	rej.RequireExit(t, 6)
	if !strings.Contains(strings.ToLower(string(rej.Stdout)+string(rej.Stderr)), "leader") {
		t.Errorf("standby mutation exit-6 rejection did not name the leader:\nstdout:\n%s\nstderr:\n%s", rej.Stdout, rej.Stderr)
	}

	leaderPID := readDaemonPID(t, filepath.Join(ws, ".iris", "iris.pid"))
	if err := syscall.Kill(leaderPID, syscall.SIGKILL); err != nil {
		t.Fatalf("SIGKILL scenario leader (pid %d): %v", leaderPID, err)
	}
	if !waitForLeader(t, standbySock) {
		t.Fatalf("standby did not take over after the leader was killed (role=%q)", healthzRole(t, standbySock))
	}
	// Lanes resume on the new leader: the baseline is read AFTER takeover (the old
	// leader produced until the instant it died, so a pre-kill baseline could be
	// overtaken by its own last runs), and any succeeded extract run strictly newer
	// than it is necessarily the new leader's.
	takeoverBaseline := scMaxSucceeded(meta, "extract_orders")
	scWaitSucceededAfter(t, meta, "extract_orders", takeoverBaseline, 90*time.Second)
}

// scWaitAnalyticsRows waits until analytics.orders has at least one row, so the
// endpoint read has something to serve after the loop's writes and the wipe steps.
func scWaitAnalyticsRows(t *testing.T, data *pgx.Conn, deadline time.Duration) {
	t.Helper()
	dl := time.Now().Add(deadline)
	for time.Now().Before(dl) {
		var n int
		_ = data.QueryRow(context.Background(), "SELECT count(*) FROM analytics.orders").Scan(&n)
		if n > 0 {
			return
		}
		time.Sleep(200 * time.Millisecond)
	}
	t.Fatalf("analytics.orders never had a row to serve within %s", deadline)
}

// scAnyAnalyticsPK returns some current analytics.orders primary key, failing if the
// table is empty.
func scAnyAnalyticsPK(t *testing.T, data *pgx.Conn) string {
	t.Helper()
	var pk string
	if err := data.QueryRow(context.Background(),
		"SELECT id::text FROM analytics.orders ORDER BY created_at DESC LIMIT 1").Scan(&pk); err != nil {
		t.Fatalf("read an analytics.orders pk for provenance: %v", err)
	}
	return pk
}

// scTCPGet issues a Bearer-authenticated GET to the daemon's TCP listener and
// returns the status code and raw body.
func scTCPGet(t *testing.T, url, token string) (int, []byte) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		t.Fatalf("build GET %s: %v", url, err)
	}
	req.Header.Set("Authorization", "Bearer "+token)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("GET %s: %v", url, err)
	}
	defer resp.Body.Close() //nolint:errcheck // test read
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read %s body: %v", url, err)
	}
	return resp.StatusCode, body
}
