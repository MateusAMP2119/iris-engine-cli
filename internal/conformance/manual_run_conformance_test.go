//go:build conformance

package conformance

import (
	"context"
	"os"
	"path/filepath"
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
	if err := os.WriteFile(filepath.Join(dir, "iris-declare.yaml"), []byte(decl), 0o644); err != nil {
		t.Fatalf("write declaration for %s: %v", name, err)
	}
}

// TestManualPipelineRun drives the real iris binary end to end against a live daemon and
// managed Postgres to prove the two manual-run failure contracts (specification section
// 8): a manual run whose depends_on gate is not satisfied exits 4 with a reason, and a
// manual run that dead-letters exits 5 with the dead-lettered run recording cause=manual.
// One daemon and workspace serve both legs; three pipelines are registered upstream-first
// (a gated pair and an own-lane failing pipeline).
//
// spec: S08/manual-run-ineligible-exit4
// spec: S08/manual-run-deadletter-exit5-cause-manual
func TestManualPipelineRun(t *testing.T) {
	bin := Build(t)
	ws := shortWorkspace(t)
	socket := filepath.Join(ws, ".iris", "iris.sock")

	// gate_up is an upstream that never runs; gate_down gates on it, so gate_down is
	// never eligible for a manual run. boom is an own-lane pipeline whose script fails,
	// so a manual run of it dead-letters.
	writePipelineDecl(t, ws, "gate_up", "name: gate_up\nrun: [\"true\"]\n")
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

	t.Run("S08/manual-run-ineligible-exit4", func(t *testing.T) {
		// gate_up has produced no success, so gate_down's depends_on gate is not
		// satisfied: a manual run of it is ineligible and exits 4 with a reason.
		res := bin.Run(t, RunOptions{Args: []string{"pipeline", "run", "gate_down"}, Dir: ws, Timeout: time.Minute})
		res.RequireExit(t, 4)
		if len(res.Stderr) == 0 {
			t.Errorf("ineligible manual run exited 4 but wrote no reason to stderr\nstdout:\n%s", res.Stdout)
		}
	})

	t.Run("S08/manual-run-deadletter-exit5-cause-manual", func(t *testing.T) {
		// boom's script exits non-zero, so a manual run of it dead-letters and exits 5.
		res := bin.Run(t, RunOptions{Args: []string{"pipeline", "run", "boom"}, Dir: ws, Timeout: time.Minute})
		res.RequireExit(t, 5)

		// The dead-lettered run records cause=manual and sits in the dead-letter
		// worklist -- read directly from meta with an independent client.
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
	})
}
