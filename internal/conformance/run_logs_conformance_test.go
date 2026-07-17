//go:build conformance

package conformance

import (
	"context"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
)

// This file is the per-run output capture leg: a real pipeline run's stderr is
// captured into the run-id-keyed log (stdout is protocol-only under the turn
// protocol, #206), its path is recorded in runs.log_ref, and `iris run logs
// <run>` streams it back. A run id with no captured output answers honestly
// (operation failed), never an empty success.

func TestRunOutputCaptured(t *testing.T) {
	freshDatabases(t)
	bin := Build(t)

	ws := shortWorkspace(t)
	socket := filepath.Join(ws, ".iris", "iris.sock")
	setupWriterPipeline(t, ws, "wlog", "logs", 9301, 9302)

	bin.Run(t, RunOptions{Args: []string{"engine", "install"}, Dir: ws, Timeout: 5 * time.Minute}).RequireExit(t, 0)
	bin.Run(t, RunOptions{Args: []string{"engine", "start", "-d"}, Dir: ws, Timeout: 2 * time.Minute}).RequireExit(t, 0)
	t.Cleanup(func() {
		bin.Run(t, RunOptions{Args: []string{"engine", "stop"}, Dir: ws, Timeout: 30 * time.Second})
	})

	readyCtx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	if err := WaitForSocket(readyCtx, socket); err != nil {
		cancel()
		t.Fatalf("socket not ready: %v", err)
	}
	cancel()
	if !waitForLeader(t, socket) {
		t.Fatal("never leader")
	}

	bin.Run(t, RunOptions{Args: []string{"declare", "apply", "pipelines/wlog"}, Dir: ws, Timeout: time.Minute}).RequireExit(t, 0)
	bin.Run(t, RunOptions{Args: []string{"pipeline", "run", "wlog"}, Dir: ws, Timeout: time.Minute}).RequireExit(t, 0)

	// Target the MANUAL run specifically: the lane loop keeps minting cause=loop
	// runs, so the pipeline's latest run may still be mid-flight with an empty
	// log; the manual run is terminal (the CLI blocked on it).
	runID := manualRunForPipeline(t, ws, "wlog")

	t.Run("run-logs-streams-captured-stderr", func(t *testing.T) {
		// The pipeline's script logs a fixed line to stderr; the captured log must carry it.
		res := bin.Run(t, RunOptions{Args: []string{"run", "logs", strconv.FormatInt(runID, 10)}, Dir: ws, Timeout: 30 * time.Second})
		if res.ExitCode != 0 {
			t.Fatalf("run logs exited %d\nstdout:\n%s\nstderr:\n%s", res.ExitCode, res.Stdout, res.Stderr)
		}
		if !strings.Contains(string(res.Stdout), "noop for test attribution") {
			t.Errorf("captured log does not carry the run's stderr line:\n%s", res.Stdout)
		}
	})

	t.Run("log-ref-recorded-in-meta", func(t *testing.T) {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		meta, err := pgx.Connect(ctx, metaDSN(t, ws))
		if err != nil {
			t.Fatalf("connect meta: %v", err)
		}
		defer func() { _ = meta.Close(ctx) }()
		var ref *string
		if err := meta.QueryRow(ctx, `SELECT log_ref FROM runs WHERE id = $1`, runID).Scan(&ref); err != nil {
			t.Fatalf("read runs.log_ref: %v", err)
		}
		if ref == nil || *ref == "" {
			t.Fatal("runs.log_ref is NULL for a captured run; the run start must record the log reference")
		}
		if !strings.Contains(*ref, "logs") {
			t.Errorf("log_ref %q does not point under the logs directory", *ref)
		}
	})

	t.Run("missing-log-answers-honestly", func(t *testing.T) {
		res := bin.Run(t, RunOptions{Args: []string{"run", "logs", "999999"}, Dir: ws, Timeout: 30 * time.Second})
		if res.ExitCode != 4 {
			t.Fatalf("run logs for an absent run exited %d, want 4\nstdout:\n%s\nstderr:\n%s", res.ExitCode, res.Stdout, res.Stderr)
		}
		combined := string(res.Stdout) + string(res.Stderr)
		if !strings.Contains(combined, "no captured output") {
			t.Errorf("absent-run refusal should say no captured output: %s", combined)
		}
	})
}

// manualRunForPipeline returns the id of the pipeline's most recent cause=manual
// run by querying meta directly (conformance reads are allowed).
func manualRunForPipeline(t *testing.T, ws, pipeline string) int64 {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	conn, err := pgx.Connect(ctx, metaDSN(t, ws))
	if err != nil {
		t.Fatalf("connect meta for manual run id: %v", err)
	}
	defer func() { _ = conn.Close(ctx) }()
	var id int64
	if err := conn.QueryRow(ctx,
		`SELECT id FROM runs WHERE pipeline = $1 AND cause = 'manual' ORDER BY id DESC LIMIT 1`, pipeline).Scan(&id); err != nil {
		t.Fatalf("query manual run for %s: %v", pipeline, err)
	}
	return id
}
