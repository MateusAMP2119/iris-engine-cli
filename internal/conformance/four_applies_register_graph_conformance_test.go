//go:build conformance

package conformance

import (
	"context"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/MateusAMP2119/iris-lakehouse/internal/fixtures"
)

// TestFourAppliesRegisterGraph drives the real binary and proves that four
// single-file iris declare apply invocations, in the documented order (ingest
// composer first, then extract_orders, reset_counters, load_orders), register
// the full sample graph in meta and drive schema provisioning for each apply.
func TestFourAppliesRegisterGraph(t *testing.T) {
	t.Run("four-applies-register-graph", func(t *testing.T) {
		// Freshen the shared external cluster first: FORCE-dropping meta/data evicts a
		// prior test's lingering daemon sessions (including a still-held leader advisory
		// lock), so this daemon elects promptly instead of timing out behind a stale leader.
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
			t.Fatalf("daemon socket never ready: %v", err)
		}
		cancel()
		if !waitForLeader(t, socket) {
			t.Fatal("no leader")
		}

		// Four single-file applies, composer first, then members in composer order.
		// Schema provisioning rides each.
		targets := []string{
			filepath.Join("pipelines", "ingest"),
			filepath.Join("pipelines", "ingest", "extract_orders"),
			filepath.Join("pipelines", "ingest", "reset_counters"),
			filepath.Join("pipelines", "ingest", "load_orders"),
		}
		for _, tgt := range targets {
			res := bin.Run(t, RunOptions{Args: []string{"declare", "apply", tgt}, Dir: ws})
			res.RequireExit(t, 0)
		}

		// Verify the graph is registered.
		meta := snapshotMeta(t, ws)
		if !strings.Contains(meta, "extract_orders") || !strings.Contains(meta, "reset_counters") || !strings.Contains(meta, "load_orders") {
			t.Fatalf("pipelines not all registered after four applies:\n%s", meta)
		}
		// Dependency edge from load_orders depends_on extract_orders must be present.
		if !strings.Contains(meta, "load_orders | extract_orders") {
			t.Errorf("dependency edge load_orders -> extract_orders not in registry:\n%s", meta)
		}
		// lanes row from the composer apply
		if !strings.Contains(meta, "ingest") {
			t.Errorf("lanes not registered for ingest lane:\n%s", meta)
		}

		// Schema provisioning rode the applies: tables exist in catalog.
		catalog := snapshotCatalog(t, ws)
		if !strings.Contains(catalog, "raw | orders_staging") || !strings.Contains(catalog, "analytics | orders") {
			t.Errorf("schemas not provisioned by the applies:\n%s", catalog)
		}

		// Also exercise fixtures.MaterializeGolden path to prove the added fixture helper works
		// for a throwaway copy (no daemon here, just path sanity).
		g := fixtures.MaterializeGolden(t)
		if g == "" {
			t.Error("MaterializeGolden returned empty path")
		}
	})
}
