//go:build conformance

package conformance

import (
	"context"
	"path/filepath"
	"strconv"
	"testing"
	"time"

	"github.com/MateusAMP2119/iris-engine-cli/internal/api"
)

// itoa renders a run id as its decimal string, the form the CLI and the wire use.
func itoa(id int64) string { return strconv.FormatInt(id, 10) }

// TestDeadletterBlastAndReplay drives the real iris binary end to end against a running
// daemon and real Postgres to prove the two dead-letter dispositions of the golden
// sample (specification section 6.2, and the golden sample step 6): the blast-radius
// readout `iris deadletter show` renders, and the root-walking replay that clears the
// worklist and discards the propagated entry as superseded.
//
// The propagation state itself -- a forced failure in extract_orders that propagates to
// load_orders while reset_counters (composer-only) is untouched -- is the lane loop's to
// PRODUCE (E13.3). Here it is seeded directly in meta so the readout and the replay
// disposition, which are this task's contracts, can be proven through the real binary +
// daemon + Postgres over registered golden pipelines. `iris deadletter show` reads the
// blast radius on any node; `iris deadletter replay` mints the root's replacement on the
// leader, clears the root entry, and discards the propagated entry as superseded.
func TestDeadletterBlastAndReplay(t *testing.T) {
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

	readyCtx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	if err := WaitForSocket(readyCtx, socket); err != nil {
		cancel()
		t.Fatalf("daemon socket never became ready: %v", err)
	}
	cancel()
	if !waitForLeader(t, socket) {
		t.Fatal("daemon never became leader")
	}

	// Register the golden graph upstream-first: the ingest composer, extract_orders
	// (root), reset_counters (composer-only), load_orders (depends_on extract_orders).
	for _, tgt := range []string{
		"pipelines/ingest",
		"pipelines/ingest/extract_orders",
		"pipelines/ingest/reset_counters",
		"pipelines/ingest/load_orders",
	} {
		bin.Run(t, RunOptions{Args: []string{"declare", "apply", tgt}, Dir: ws}).RequireExit(t, 0)
	}

	ctx := context.Background()
	conn := connectPG(t, metaDSN(t, ws))

	// Seed the propagation state: extract_orders failed on its own (root cause), and
	// load_orders propagated from it (a never-executed dead-lettered run recording the
	// poisoned upstream in run_inputs). reset_counters has no entry: composer order is
	// not a dependency, so it is untouched.
	var extractRun, loadRun int64
	if err := conn.QueryRow(ctx,
		`INSERT INTO runs (pipeline, state, cause, declaration_checksum, recorded_at)
		 VALUES ('extract_orders', 'dead_lettered', 'loop', 'seed', now()::text) RETURNING id`).Scan(&extractRun); err != nil {
		t.Fatalf("seed extract_orders run: %v", err)
	}
	if _, err := conn.Exec(ctx,
		`INSERT INTO dead_letters (run_id, reason) VALUES ($1, 'failed')`, extractRun); err != nil {
		t.Fatalf("seed extract_orders dead letter: %v", err)
	}
	if err := conn.QueryRow(ctx,
		`INSERT INTO runs (pipeline, state, cause, declaration_checksum, recorded_at)
		 VALUES ('load_orders', 'dead_lettered', 'propagated', 'seed', now()::text) RETURNING id`).Scan(&loadRun); err != nil {
		t.Fatalf("seed load_orders run: %v", err)
	}
	if _, err := conn.Exec(ctx,
		`INSERT INTO dead_letters (run_id, reason, failed_upstream) VALUES ($1, 'upstream_dead_lettered', 'extract_orders')`, loadRun); err != nil {
		t.Fatalf("seed load_orders dead letter: %v", err)
	}
	if _, err := conn.Exec(ctx,
		`INSERT INTO run_inputs (run_id, upstream_run_id) VALUES ($1, $2)`, loadRun, extractRun); err != nil {
		t.Fatalf("seed load_orders run_inputs: %v", err)
	}

	loadRef := itoa(loadRun)

	// spec: S13/blast-radius-readout
	t.Run("S13/blast-radius-readout", func(t *testing.T) {
		// deadletter show on the PROPAGATED entry walks to the root cause and names
		// load_orders poisoned and reset_counters untouched (order is not dependency).
		res := bin.Run(t, RunOptions{Args: []string{"--socket", socket, "deadletter", "show", loadRef, "--json"}})
		res.RequireExit(t, 0)
		var env struct {
			Data api.DeadImpactPayload `json:"data"`
		}
		res.DecodeJSON(t, &env)
		got := env.Data

		if got.RootCause.Pipeline != "extract_orders" {
			t.Errorf("root cause pipeline = %q, want extract_orders", got.RootCause.Pipeline)
		}
		if got.RootCause.Run != itoa(extractRun) {
			t.Errorf("root cause run = %q, want %d", got.RootCause.Run, extractRun)
		}
		class := map[string]string{}
		for _, im := range got.Impacts {
			class[im.Pipeline] = im.Class
		}
		if class["load_orders"] != "poisoned_now" {
			t.Errorf("load_orders class = %q, want poisoned_now", class["load_orders"])
		}
		if class["reset_counters"] != "untouched" {
			t.Errorf("reset_counters class = %q, want untouched (order is not dependency)", class["reset_counters"])
		}
	})

	// spec: S13/replay-root-walk-supersedes
	t.Run("S13/replay-root-walk-supersedes", func(t *testing.T) {
		// deadletter replay on the propagated entry auto-walks to the root failure, mints
		// a replacement on current data, clears the worklist, and discards the propagated
		// entry as superseded.
		res := bin.Run(t, RunOptions{Args: []string{"--socket", socket, "deadletter", "replay", loadRef}})
		res.RequireExit(t, 0)

		// The worklist is cleared: the root entry exited when its replacement minted, and
		// the propagated entry was discarded as superseded.
		var depth int
		if err := conn.QueryRow(ctx, `SELECT count(*) FROM dead_letters`).Scan(&depth); err != nil {
			t.Fatalf("count dead_letters: %v", err)
		}
		if depth != 0 {
			t.Errorf("worklist depth after replay = %d, want 0 (root replaced, propagated superseded)", depth)
		}

		// A fresh replacement was minted on current data: cause replay, replayed_from the
		// replaced root run, for the root's pipeline. The propagated entry was NOT replayed
		// (only the root cause is; dependents follow next pass).
		var replacements int
		if err := conn.QueryRow(ctx,
			`SELECT count(*) FROM runs WHERE cause='replay' AND replayed_from=$1 AND pipeline='extract_orders'`,
			extractRun).Scan(&replacements); err != nil {
			t.Fatalf("count replacement runs: %v", err)
		}
		if replacements != 1 {
			t.Errorf("replacement runs for the root = %d, want exactly 1 (the root cause replayed)", replacements)
		}

		// The replaced root run row itself stays in runs (a worklist exit never deletes run
		// history).
		var rootRows int
		if err := conn.QueryRow(ctx, `SELECT count(*) FROM runs WHERE id=$1`, extractRun).Scan(&rootRows); err != nil {
			t.Fatalf("count root run row: %v", err)
		}
		if rootRows != 1 {
			t.Errorf("root run row count = %d, want 1 (run history outlives a worklist exit)", rootRows)
		}
	})
}
