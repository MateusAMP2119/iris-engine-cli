//go:build conformance

package conformance

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
)

// This file is the declare-destroy teardown leg: a real daemon, a real pipeline
// with landed disposable data, one `iris declare destroy --force`. It proves the
// destroyer's production seams end to end: the un-promoted disposable data is
// reverted (the journal-driven reverse-replay, same as a scoped wipe), each
// remaining run leaves its archival run_summaries row, and every meta row of the
// unit is retired -- while the journal history itself is retained.

// TestDeclareDestroyTeardown drives apply -> run -> land disposable rows ->
// destroy --force against a live engine and asserts the teardown's three
// observable outcomes on the data and meta databases.
func TestDeclareDestroyTeardown(t *testing.T) {
	freshDatabases(t)
	bin := Build(t)

	ws := shortWorkspace(t)
	socket := filepath.Join(ws, ".iris", "iris.sock")
	setupWriterPipeline(t, ws, "w9", "teardown", 9101, 9102)

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

	bin.Run(t, RunOptions{Args: []string{"declare", "apply", "pipelines/w9"}, Dir: ws, Timeout: time.Minute}).RequireExit(t, 0)
	bin.Run(t, RunOptions{Args: []string{"pipeline", "run", "w9"}, Dir: ws, Timeout: time.Minute}).RequireExit(t, 0)

	// Land two disposable (wipe-eligible) rows attributed to the real run, so the
	// destroy has un-promoted data to revert and a run to archive.
	runID := latestRunForPipeline(t, ws, "w9")
	conn := connectData(t, dataDSN(t, ws))
	defer conn.Close(context.Background())
	landAttributed(t, conn, runID, true, 9101, "from-w9", 9102, "from-w9")
	assertCount(ctxFor(t), t, conn, 2, "SELECT count(*) FROM testdata.items")
	assertCount(ctxFor(t), t, conn, 2, "SELECT count(*) FROM public.data_journal WHERE undo='open'")

	// Destroy the pipeline. The lane loop keeps it in flight, so --force is the
	// documented path (cancels the in-flight loop run, then proceeds); the hard
	// blockers stay open (no dependent, consumer, or worklist reference).
	bin.Run(t, RunOptions{Args: []string{"declare", "destroy", "pipelines/w9", "--force"}, Dir: ws, Timeout: 2 * time.Minute}).RequireExit(t, 0)

	t.Run("destroy-reverts-unpromoted-data", func(t *testing.T) {
		// The reverter ran the journal-driven reverse-replay before the meta
		// retirement: the disposable rows are gone and no open entry remains, while
		// the journal history itself is retained (provenance memory, never deleted).
		assertCount(ctxFor(t), t, conn, 0, "SELECT count(*) FROM testdata.items")
		assertCount(ctxFor(t), t, conn, 0, "SELECT count(*) FROM public.data_journal WHERE undo='open'")
		assertCount(ctxFor(t), t, conn, 2, "SELECT count(*) FROM public.data_journal")
	})

	meta, err := pgx.Connect(context.Background(), metaDSN(t, ws))
	if err != nil {
		t.Fatalf("connect meta: %v", err)
	}
	defer meta.Close(context.Background())

	t.Run("destroy-archives-run-summaries", func(t *testing.T) {
		// Every remaining run left its archival summary (the manual run at least;
		// loop passes may have added more), written in the retirement transaction.
		var sums int64
		if err := meta.QueryRow(ctxFor(t), `SELECT count(*) FROM run_summaries WHERE pipeline = 'w9'`).Scan(&sums); err != nil {
			t.Fatalf("count run_summaries: %v", err)
		}
		if sums < 1 {
			t.Errorf("run_summaries holds %d rows for the destroyed pipeline, want >= 1 (stamps must resolve after teardown)", sums)
		}
		var archived int64
		if err := meta.QueryRow(ctxFor(t), `SELECT count(*) FROM run_summaries WHERE run_id = $1`, runID).Scan(&archived); err != nil {
			t.Fatalf("count archived manual run: %v", err)
		}
		if archived != 1 {
			t.Errorf("the manual run %d left %d summaries, want exactly 1", runID, archived)
		}
	})

	t.Run("destroy-retires-every-meta-row", func(t *testing.T) {
		for _, q := range []string{
			`SELECT count(*) FROM pipelines WHERE name = 'w9'`,
			`SELECT count(*) FROM runs WHERE pipeline = 'w9'`,
			`SELECT count(*) FROM artifacts WHERE pipeline = 'w9'`,
			`SELECT count(*) FROM lanes WHERE pipeline = 'w9'`,
			`SELECT count(*) FROM dead_letters dl WHERE dl.run_id IN (SELECT run_id FROM run_summaries WHERE pipeline = 'w9')`,
		} {
			var n int64
			if err := meta.QueryRow(ctxFor(t), q).Scan(&n); err != nil {
				t.Fatalf("query %q: %v", q, err)
			}
			if n != 0 {
				t.Errorf("after destroy, %q = %d, want 0", q, n)
			}
		}
	})
}
