//go:build conformance

package conformance

import (
	"context"
	"errors"
	"path/filepath"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

// TestRetentionPruneUpstreamSurvivorNoViolation drives the real iris binary end-to-end
// with a running daemon and real Postgres and proves the run_inputs FK doctrine
// (specification sections 4 and 6.2): count-based retention prunes an upstream run
// while a cross-pipeline downstream still holds a run_inputs row naming it, and the
// prune must not raise a foreign-key violation. run_inputs.upstream_run_id is FK-free
// (the precedent is data_journal.run_id), so the pruned upstream's run row is deleted
// cleanly and the surviving downstream's consumption ledger row stays, now resolving to
// the upstream's archival summary rather than a live run.
//
// The prune is reproduced exactly as store.PruneRun issues it -- one meta transaction
// that writes the upstream's archival summary, deletes the upstream's OWN run_inputs
// rows (none here), then deletes the upstream run row -- against the real meta schema
// the engine created at install. On the old schema (a hard upstream_run_id FK) the
// final DELETE would raise SQLSTATE 23503; on the FK-free schema it commits.
//
// spec: S06.2/prune-upstream-survivor-no-violation
func TestRetentionPruneUpstreamSurvivorNoViolation(t *testing.T) {
	// Freshen the shared external cluster first: FORCE-dropping the meta/data databases
	// evicts a prior test's lingering daemon sessions -- including a still-held leader
	// advisory lock -- so this daemon elects promptly instead of timing out behind a
	// stale leader (see deadletter_plane/journal_capture_wipe).
	freshDatabases(t)
	bin := Build(t)
	ws := shortWorkspace(t)
	copyGoldenWorkspace(t, ws)
	socket := filepath.Join(ws, ".iris", "iris.sock")

	bin.Run(t, RunOptions{Args: []string{"engine", "install"}, Dir: ws, Timeout: 5 * time.Minute}).RequireExit(t, 0)
	bin.Run(t, RunOptions{Args: []string{"engine", "start", "-d"}, Dir: ws, Timeout: 2 * time.Minute}).RequireExit(t, 0)
	t.Cleanup(func() {
		bin.Run(t, RunOptions{Args: []string{"engine", "stop"}, Dir: ws, Timeout: 30 * time.Second})
	})

	readyCtx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	if err := WaitForSocket(readyCtx, socket); err != nil {
		cancel()
		t.Fatalf("daemon socket never became ready: %v", err)
	}
	cancel()
	if !waitForLeader(t, socket) {
		t.Fatal("daemon never became leader")
	}

	ctx := context.Background()
	metaConn := connectPG(t, metaDSN(t, ws))
	defer func() { _ = metaConn.Close(ctx) }()

	const (
		upstreamRunID   int64 = 707071 // pipeline A (extract_orders): the pruned upstream
		downstreamRunID int64 = 707072 // pipeline B (load_orders): the surviving consumer
	)

	// Two pipelines in a cross-pipeline lineage: load_orders consumed extract_orders.
	for _, ddl := range []string{
		`INSERT INTO pipelines (name, folder, run, artifact, data_mode)
		 VALUES ('extract_orders', 'pipelines/ingest/extract_orders', '["python","main.py"]'::json, 'source', 'disposable')
		 ON CONFLICT (name) DO NOTHING`,
		`INSERT INTO pipelines (name, folder, run, artifact, data_mode)
		 VALUES ('load_orders', 'pipelines/ingest/load_orders', '["python","main.py"]'::json, 'source', 'disposable')
		 ON CONFLICT (name) DO NOTHING`,
	} {
		if _, err := metaConn.Exec(ctx, ddl); err != nil {
			t.Fatalf("insert pipeline row: %v", err)
		}
	}

	// The two runs: an upstream success in extract_orders, a downstream success in
	// load_orders. No artifact_hash so no artifacts FK is needed (dev runs).
	if _, err := metaConn.Exec(ctx, `
		INSERT INTO runs (id, pipeline, state, cause, declaration_checksum, recorded_at)
		OVERRIDING SYSTEM VALUE
		VALUES ($1, 'extract_orders', 'succeeded', 'loop', 'sha256-decl-up', '2026-07-09T00:00:00Z')
		ON CONFLICT (id) DO NOTHING
	`, upstreamRunID); err != nil {
		t.Fatalf("insert upstream run: %v", err)
	}
	if _, err := metaConn.Exec(ctx, `
		INSERT INTO runs (id, pipeline, state, cause, declaration_checksum, recorded_at)
		OVERRIDING SYSTEM VALUE
		VALUES ($1, 'load_orders', 'succeeded', 'loop', 'sha256-decl-down', '2026-07-09T00:00:01Z')
		ON CONFLICT (id) DO NOTHING
	`, downstreamRunID); err != nil {
		t.Fatalf("insert downstream run: %v", err)
	}

	// The consumption edge: the surviving downstream names the soon-to-be-pruned
	// upstream. This is the row a hard upstream_run_id FK would strand at prune time.
	if _, err := metaConn.Exec(ctx, `
		INSERT INTO run_inputs (run_id, upstream_run_id) VALUES ($1, $2)
		ON CONFLICT DO NOTHING
	`, downstreamRunID, upstreamRunID); err != nil {
		t.Fatalf("insert consumption edge: %v", err)
	}

	// Prune the UPSTREAM run exactly as store.PruneRun does: archival summary first,
	// then the run's own run_inputs rows (the upstream consumed nothing here), then the
	// run row -- all in one meta transaction. A hard upstream_run_id FK makes the final
	// DELETE raise foreign_key_violation (23503) because the downstream still references
	// this run; FK-free, it commits.
	if err := pruneUpstream(ctx, metaConn, upstreamRunID); err != nil {
		var pgErr *pgconn.PgError
		if errors.As(err, &pgErr) && pgErr.Code == "23503" {
			t.Fatalf("pruning a referenced upstream raised a foreign-key violation (%s): run_inputs.upstream_run_id must be FK-free so retention can prune an upstream a cross-pipeline downstream still references: %v", pgErr.Code, err)
		}
		t.Fatalf("prune upstream transaction failed: %v", err)
	}

	// The surviving downstream's ledger row is intact: its lineage and gate check are
	// preserved even though the upstream run row is gone.
	var edges int
	if err := metaConn.QueryRow(ctx,
		`SELECT count(*) FROM run_inputs WHERE run_id = $1 AND upstream_run_id = $2`,
		downstreamRunID, upstreamRunID).Scan(&edges); err != nil {
		t.Fatalf("count surviving run_inputs edge: %v", err)
	}
	if edges != 1 {
		t.Errorf("surviving downstream's run_inputs edge count = %d, want 1 (the FK-free upstream reference must survive the prune)", edges)
	}

	// The upstream run row is gone, but it now resolves to its archival summary.
	var runRows int
	if err := metaConn.QueryRow(ctx, `SELECT count(*) FROM runs WHERE id = $1`, upstreamRunID).Scan(&runRows); err != nil {
		t.Fatalf("count upstream run row: %v", err)
	}
	if runRows != 0 {
		t.Errorf("pruned upstream run row count = %d, want 0", runRows)
	}
	var summaries int
	if err := metaConn.QueryRow(ctx, `SELECT count(*) FROM run_summaries WHERE run_id = $1`, upstreamRunID).Scan(&summaries); err != nil {
		t.Fatalf("count upstream run summary: %v", err)
	}
	if summaries != 1 {
		t.Errorf("pruned upstream run summary count = %d, want 1 (the reference must resolve to a summary)", summaries)
	}
}

// pruneUpstream reproduces store.PruneRun's atomic prune batch for one run in a single
// meta transaction: write the archival summary, delete the run's own run_inputs rows,
// then delete the run row. It is the exact statement sequence store.pruneStatements
// builds; running it against the real meta schema is what surfaces (or clears) the
// upstream_run_id FK violation.
func pruneUpstream(ctx context.Context, conn *pgx.Conn, runID int64) error {
	tx, err := conn.Begin(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback(context.Background()) }()

	if _, err := tx.Exec(ctx, `INSERT INTO run_summaries
		(run_id, pipeline, state, artifact_hash, declaration_checksum, consumed_upstream_run_ids, snapshot_lsn, journal_floor, journal_ceiling, recorded_at)
		VALUES ($1, 'extract_orders', 'succeeded', NULL, 'sha256-decl-up', '[]'::json, NULL, NULL, NULL, now()::text)`, runID); err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, `DELETE FROM run_inputs WHERE run_id = $1`, runID); err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, `DELETE FROM runs WHERE id = $1`, runID); err != nil {
		return err
	}
	return tx.Commit(ctx)
}
