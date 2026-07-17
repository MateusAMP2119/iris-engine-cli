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

// writePipelineDecl writes a minimal pipeline declaration under
// <ws>/pipelines/<name>/iris-declare.yaml, creating the folder. The folder basename is
// the pipeline name (the declare name-must-match-folder rule), so the daemon can register
// and resolve it.
func writePipelineDecl(t *testing.T, ws, name, decl string) {
	t.Helper()
	dir := filepath.Join(ws, "pipelines", name)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("create pipeline folder %s: %v", dir, err)
	}
	if err := os.WriteFile(filepath.Join(dir, "iris-declare.yaml"), []byte(decl), 0o644); err != nil { //nolint:gosec // G306: workspace declaration file, world-readable by design.
		t.Fatalf("write declaration for %s: %v", name, err)
	}
}

// TestManualPipelineRun drives the real iris binary end to end against a live daemon and
// managed Postgres to prove the two manual-run failure contracts under the turn protocol
// (#206): a manual run whose depends_on gate is not satisfied exits 4 with a reason, and
// a manual run whose worker DIES -- a bare exit with no terminal frame is a death, not a
// completed turn -- dead-letters and exits 5 with the run recording cause=manual and the
// death detail in dead_letters.error. One daemon and workspace serve both legs; three
// pipelines are registered upstream-first (a gated pair and a loose failing pipeline,
// whose manual run routes immediate since no lane row places it).
func TestManualPipelineRun(t *testing.T) {
	// Shared-cluster isolation: on the CI lane every conformance test shares one external
	// Postgres with fixed-name meta/data databases. A prior test (or a prior back-to-back
	// run of this one) leaves runs, dead_letters, and lane state behind, so this test's
	// `SELECT ... ORDER BY id DESC` reads and its depends_on-gate expectations would race
	// foreign rows -- gate_up's leftover success would make gate_down eligible, and boom's
	// leftover run would shadow the manual one. Start from a clean slate; the daemon
	// recreates the databases on start.
	freshDatabases(t)
	bin := Build(t)
	ws := shortWorkspace(t)
	socket := filepath.Join(ws, ".iris", "iris.sock")

	// gate_up is an upstream that never SUCCEEDS: its worker hangs mid-turn forever,
	// answering no terminal frame, so under the turn protocol its hung loop turn
	// records NOTHING -- no run row at all, no engine timeout (clock doctrine) -- and
	// gate_down's depends_on gate stays pending. The premise holds structurally, not
	// by timing: gate_up can never record a success. The hang holds the shared queue
	// lane (loose pipelines walk one serial queue lane), which never blocks the
	// manual path -- loose manual runs execute immediately, out of band. boom is a
	// loose pipeline whose script exits without ever speaking a frame: a bare exit is
	// a worker DEATH, so a manual run of it dead-letters.
	writePipelineDecl(t, ws, "gate_up", "name: gate_up\nrun: [\"sh\", \"-c\", \"sleep 100000\"]\n")
	writePipelineDecl(t, ws, "gate_down", "name: gate_down\nrun: [\"true\"]\ndepends_on: [gate_up]\n")
	writePipelineDecl(t, ws, "boom", "name: boom\nrun: [\"sh\", \"-c\", \"exit 7\"]\n")

	// Install (managed: cached download) and start the daemon detached against a real
	// Postgres, then wait for it to become the confirmed leader (mutations need a
	// leader).
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
		t.Fatal("daemon never became leader; manual runs need a leader")
	}

	// Register the pipelines upstream-first: gate_down's depends_on names gate_up, which
	// must be pre-registered.
	for _, name := range []string{"gate_up", "gate_down", "boom"} {
		bin.Run(t, RunOptions{Args: []string{"declare", "apply", filepath.Join("pipelines", name)}, Dir: ws, Timeout: time.Minute}).RequireExit(t, 0)
	}

	t.Run("manual-run-ineligible-exit4", func(t *testing.T) {
		// gate_up has recorded no success (its hung turn records nothing at all), so
		// gate_down's depends_on gate is not satisfied: a manual run of it is
		// ineligible and exits 4 with a reason.
		res := bin.Run(t, RunOptions{Args: []string{"pipeline", "run", "gate_down"}, Dir: ws, Timeout: time.Minute})
		res.RequireExit(t, 4)
		if len(res.Stderr) == 0 {
			t.Errorf("ineligible manual run exited 4 but wrote no reason to stderr\nstdout:\n%s", res.Stdout)
		}
	})

	t.Run("manual-run-deadletter-exit5-cause-manual", func(t *testing.T) {
		// boom's worker exits 7 without a terminal frame -- a death, since a manual
		// run executes as one turn -- so the manual run dead-letters and exits 5.
		res := bin.Run(t, RunOptions{Args: []string{"pipeline", "run", "boom"}, Dir: ws, Timeout: time.Minute})
		res.RequireExit(t, 5)

		// The dead-lettered run records cause=manual and sits in the dead-letter
		// worklist -- read directly from meta with an independent client. The loop may
		// have dead-lettered boom once already (cause=loop, before gate_up's hang held
		// the queue lane), but the manual run is minted later and the outstanding
		// failure parks loop reruns, so the latest run is the manual one.
		dsn := metaDSN(t, ws)
		qCtx, cancelQ := context.WithTimeout(context.Background(), 15*time.Second)
		defer cancelQ()
		conn, err := pgx.Connect(qCtx, dsn)
		if err != nil {
			t.Fatalf("connect to meta: %v", err)
		}
		defer func() { _ = conn.Close(context.Background()) }()

		var cause, state string
		var runID int64
		if err := conn.QueryRow(qCtx,
			"SELECT id, cause, state FROM runs WHERE pipeline = $1 ORDER BY id DESC LIMIT 1", "boom",
		).Scan(&runID, &cause, &state); err != nil {
			t.Fatalf("read boom's latest run: %v", err)
		}
		if cause != "manual" {
			t.Errorf("dead-lettered run cause = %q, want manual", cause)
		}
		if state != "dead_lettered" {
			t.Errorf("run state = %q, want dead_lettered", state)
		}

		var dead int
		if err := conn.QueryRow(qCtx,
			"SELECT count(*) FROM dead_letters WHERE run_id = $1", runID,
		).Scan(&dead); err != nil {
			t.Fatalf("read dead_letters for run %d: %v", runID, err)
		}
		if dead != 1 {
			t.Errorf("dead_letters rows for the manual run = %d, want 1", dead)
		}
		// The death detail (#206): reason failed, error carrying the manual prefix and
		// the worker's exit disposition (there is no exit_code column value to lean on;
		// failed turns record it NULL).
		var reason, detail string
		if err := conn.QueryRow(qCtx,
			"SELECT reason, coalesce(error,'') FROM dead_letters WHERE run_id = $1", runID,
		).Scan(&reason, &detail); err != nil {
			t.Fatalf("read dead_letters detail for run %d: %v", runID, err)
		}
		if reason != "failed" {
			t.Errorf("dead_letters reason = %q, want failed", reason)
		}
		if !strings.Contains(detail, "manual run dead-lettered:") || !strings.Contains(detail, "exit code 7") {
			t.Errorf("dead_letters error = %q; want the manual death detail (manual run dead-lettered: ... exit code 7)", detail)
		}
	})
}
