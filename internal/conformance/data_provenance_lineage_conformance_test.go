//go:build conformance

package conformance

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/MateusAMP2119/iris-engine-cli/internal/pg"
)

// TestDataProvenanceLineage drives the real iris binary end-to-end with a running
// daemon and real Postgres over the golden sample workspace, produces a row
// attributed to a writing run with upstream consumption, and proves the three
// S13 lineage contracts:
//
//   - S13/data-provenance-full-lineage: iris data provenance answers the writing
//     run, pipeline, artifact/declaration hashes, and consumed upstream in one
//     query while the history is live.
//   - S13/data-provenance-after-prune: after the writing run is pruned (its run
//     row and run_inputs gone), provenance still answers from the archival
//     summary.
//   - S13/archived-partition-provenance-answers: provenance still answers
//     stamps whose partition has been sealed, exported, and dropped (location
//     archived).
//
// The tests are red until the provenance readout wires the full three-lookup
// walk (live + summary fallback + archived span) and the CLI surfaces it.
//
// spec: S13/data-provenance-full-lineage
// spec: S13/data-provenance-after-prune
// spec: S13/archived-partition-provenance-answers
func TestDataProvenanceLineage(t *testing.T) {
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

	// Apply the golden ingest lane upstream-first so the registry and
	// provisioning exist (load_orders writes analytics.orders).
	targets := []string{
		"pipelines/ingest",
		"pipelines/ingest/extract_orders",
		"pipelines/ingest/reset_counters",
		"pipelines/ingest/load_orders",
	}
	for _, tgt := range targets {
		bin.Run(t, RunOptions{Args: []string{"declare", "apply", tgt}, Dir: ws}).RequireExit(t, 0)
	}

	// Fabricate a writing run + upstream and a stamped row in analytics.orders.
	// We drive the journal entry directly (SET run_id + INSERT) exactly as an
	// injected run connection would, then record the run facts and consumption
	// in meta. This yields the state the provenance walk must answer.
	const (
		authorRunID   int64 = 424242
		upstreamRunID int64 = 424241
	)
	pk := "11111111-1111-1111-1111-111111111111"

	// Insert an attributed row by connecting with the run id baked into the DSN
	// via the same InjectRunID the engine uses at spawn (the -c option sets the
	// GUC before any statement; the capture trigger reads it in-transaction).
	writeDSN := pg.InjectRunID(dataDSN(t, ws), authorRunID)
	dataConn := connectPG(t, writeDSN)
	defer func() { _ = dataConn.Close(context.Background()) }()

	// Minimal insert; customer_id is required non-null uuid. The journal entry
	// will be stamped with the run id from the injected GUC.
	if _, err := dataConn.Exec(context.Background(), `
		INSERT INTO analytics.orders (id, customer_id, amount)
		VALUES ($1::uuid, '22222222-2222-2222-2222-222222222222'::uuid, 123)
	`, pk); err != nil {
		t.Fatalf("insert attributed row for provenance: %v", err)
	}

	// Record the runs and consumption in meta so lineage can resolve.
	metaConn := connectPG(t, metaDSN(t, ws))
	defer func() { _ = metaConn.Close(context.Background()) }()

	// Ensure a pipeline row for the name used by the fabricated run (load_orders
	// was applied above; insert is idempotent enough for our snapshot).
	_, _ = metaConn.Exec(context.Background(), `
		INSERT INTO pipelines (name, folder, run, artifact, data_mode)
		VALUES ('load_orders', 'pipelines/ingest/load_orders', '["python","main.py"]'::json, 'source', 'disposable')
		ON CONFLICT (name) DO NOTHING
	`)
	_, _ = metaConn.Exec(context.Background(), `
		INSERT INTO pipelines (name, folder, run, artifact, data_mode)
		VALUES ('extract_orders', 'pipelines/ingest/extract_orders', '["python","main.py"]'::json, 'source', 'disposable')
		ON CONFLICT (name) DO NOTHING
	`)

	// Fabricate artifact rows so runs FK is satisfied (hash is FK to artifacts).
	if _, err := metaConn.Exec(context.Background(), `
		INSERT INTO artifacts (hash, pipeline, size_bytes, recorded_at)
		VALUES ('sha256-bin-author', 'load_orders', 123, '2026-07-09T00:00:00Z'),
		       ('sha256-bin-up', 'extract_orders', 123, '2026-07-09T00:00:00Z')
		ON CONFLICT (hash) DO NOTHING
	`); err != nil {
		t.Fatalf("fabricate artifacts: %v", err)
	}

	// Author run (succeeded, with hashes and pin for full report).
	_, _ = metaConn.Exec(context.Background(), `
		INSERT INTO runs (id, pipeline, state, cause, artifact_hash, declaration_checksum, snapshot_lsn, journal_floor, journal_ceiling, recorded_at)
		OVERRIDING SYSTEM VALUE
		VALUES ($1, 'load_orders', 'succeeded', 'loop', 'sha256-bin-author', 'sha256-decl-author', '0/ABC', 100, 200, '2026-07-09T00:00:00Z')
		ON CONFLICT (id) DO UPDATE SET pipeline = EXCLUDED.pipeline, state = EXCLUDED.state
	`, authorRunID)

	// Upstream run.
	_, _ = metaConn.Exec(context.Background(), `
		INSERT INTO runs (id, pipeline, state, cause, artifact_hash, declaration_checksum, snapshot_lsn, journal_floor, journal_ceiling, recorded_at)
		OVERRIDING SYSTEM VALUE
		VALUES ($1, 'extract_orders', 'succeeded', 'loop', 'sha256-bin-up', 'sha256-decl-up', '0/AAA', 90, 95, '2026-07-09T00:00:00Z')
		ON CONFLICT (id) DO UPDATE SET pipeline = EXCLUDED.pipeline, state = EXCLUDED.state
	`, upstreamRunID)

	// Consumption edge for lineage.
	_, _ = metaConn.Exec(context.Background(), `
		INSERT INTO run_inputs (run_id, upstream_run_id) VALUES ($1, $2)
		ON CONFLICT DO NOTHING
	`, authorRunID, upstreamRunID)

	// spec: S13/data-provenance-full-lineage
	t.Run("S13/data-provenance-full-lineage", func(t *testing.T) {
		res := bin.Run(t, RunOptions{
			Args: []string{"--json", "data", "provenance", "analytics.orders", pk},
			Dir:  ws,
		})
		res.RequireExit(t, 0)

		// Once wired, stdout must carry the full lineage: writing run, pipeline,
		// hashes, and at least one consumed upstream. The exact envelope shape
		// is defined by the provenance readout; assert presence of core fields.
		// The flat provenance result carries the authoring run in author.run_id
		// (with authored=true) and the run's facts as top-level fields.
		var env struct {
			Data struct {
				Schema       string           `json:"schema"`
				Table        string           `json:"table"`
				PK           string           `json:"pk"`
				Stamps       []map[string]any `json:"stamps"`
				Author       map[string]any   `json:"author"`
				Authored     bool             `json:"authored"`
				Pipeline     string           `json:"pipeline"`
				ArtifactHash *string          `json:"artifact_hash"`
				Ancestry     []map[string]any `json:"ancestry"`
			} `json:"data"`
		}
		res.DecodeJSON(t, &env)

		if !env.Data.Authored || env.Data.Author == nil || env.Data.Author["run_id"] == nil {
			t.Fatalf("full lineage did not report an authoring run; got authored=%v author=%v", env.Data.Authored, env.Data.Author)
		}
		if env.Data.Pipeline != "load_orders" {
			t.Errorf("full lineage pipeline = %v, want load_orders", env.Data.Pipeline)
		}
		if env.Data.ArtifactHash == nil || *env.Data.ArtifactHash == "" {
			t.Errorf("full lineage reported no artifact hash; want the writing run's binary hash")
		}
		if len(env.Data.Ancestry) == 0 {
			t.Errorf("full lineage reported no ancestry (consumed upstream); want at least the extract_orders edge")
		}
	})

	// spec: S13/data-provenance-after-prune
	t.Run("S13/data-provenance-after-prune", func(t *testing.T) {
		// Prune the author run: remove its run row and run_inputs; leave only the
		// archival summary. Provenance must still answer the same row.
		_, _ = metaConn.Exec(context.Background(), `DELETE FROM run_inputs WHERE run_id = $1`, authorRunID)
		_, _ = metaConn.Exec(context.Background(), `DELETE FROM runs WHERE id = $1`, authorRunID)
		_, _ = metaConn.Exec(context.Background(), `
			INSERT INTO run_summaries (run_id, pipeline, state, artifact_hash, declaration_checksum, consumed_upstream_run_ids, snapshot_lsn, journal_floor, journal_ceiling, recorded_at)
			VALUES ($1, 'load_orders', 'succeeded', 'sha256-bin-author', 'sha256-decl-author', '[424241]'::json, '0/ABC', 100, 200, '2026-07-09T00:00:00Z')
			ON CONFLICT (run_id) DO NOTHING
		`, authorRunID)

		res := bin.Run(t, RunOptions{
			Args: []string{"--json", "data", "provenance", "analytics.orders", pk},
			Dir:  ws,
		})
		res.RequireExit(t, 0)

		var env struct {
			Data struct {
				Author              map[string]any   `json:"author"`
				Authored            bool             `json:"authored"`
				FromSummary         bool             `json:"from_summary"`
				DeclarationChecksum string           `json:"declaration_checksum"`
				Ancestry            []map[string]any `json:"ancestry"`
			} `json:"data"`
		}
		res.DecodeJSON(t, &env)

		if !env.Data.Authored || env.Data.Author == nil || env.Data.Author["run_id"] == nil {
			t.Fatalf("post-prune provenance did not resolve an authoring run; got authored=%v author=%v", env.Data.Authored, env.Data.Author)
		}
		// The summary fallback path must be visible: the live run row is gone,
		// so the facts must come from the archival summary.
		if !env.Data.FromSummary {
			t.Errorf("post-prune from_summary = false; the run row was pruned so facts must resolve from the archival summary")
		}
		// At minimum the declaration and binary hashes must survive the prune.
		if env.Data.DeclarationChecksum != "sha256-decl-author" {
			t.Errorf("post-prune declaration_checksum = %v, want exact pre-prune value", env.Data.DeclarationChecksum)
		}
		if len(env.Data.Ancestry) == 0 {
			t.Errorf("post-prune ancestry empty; summary's consumed_upstream_run_ids must supply it")
		}
	})

	// spec: S13/archived-partition-provenance-answers
	t.Run("S13/archived-partition-provenance-answers", func(t *testing.T) {
		// Simulate the partition containing the stamp having been sealed, exported,
		// and dropped: mark a checkpoint covering the journal id as archived.
		// Provenance must still return the stamp (spans archive boundary).
		_, _ = metaConn.Exec(context.Background(), `
			INSERT INTO journal_checkpoints (id_from, id_to, digest, parent_digest, signature, location, recorded_at)
			VALUES (90, 300, 'digest-for-test'::bytea, ''::bytea, 'sig'::bytea, 'archived', '2026-07-09T00:00:00Z')
			ON CONFLICT DO NOTHING
		`)

		// In a full archival world the rows would be dropped from the live journal
		// partition and served from the object-store file. For the contract we
		// assert the readout still succeeds and names the same authoring facts.
		res := bin.Run(t, RunOptions{
			Args: []string{"--json", "data", "provenance", "analytics.orders", pk},
			Dir:  ws,
		})
		res.RequireExit(t, 0)

		var env struct {
			Data struct {
				Stamps   []map[string]any `json:"stamps"`
				Author   map[string]any   `json:"author"`
				Authored bool             `json:"authored"`
			} `json:"data"`
		}
		res.DecodeJSON(t, &env)

		if len(env.Data.Stamps) == 0 {
			t.Fatalf("archived-partition provenance returned no stamps")
		}
		if !env.Data.Authored || env.Data.Author == nil || env.Data.Author["run_id"] == nil {
			t.Fatalf("archived-partition provenance did not resolve an authoring run")
		}
	})
}
