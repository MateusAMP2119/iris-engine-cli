//go:build conformance

package conformance

import (
	"context"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// This file proves the daemon's engine-inspect read plane is wired into the
// production Run() codepath (specification section 11). The plane behavior itself
// is covered by an integration test that constructs the mux with the handler
// installed directly; that test passes even when Run() forgets to wire the
// handler. Only driving the shipped binary against a live daemon proves the route
// the CLI actually reaches serves the real DDL dump rather than the unwired
// internal-fault envelope -- exactly the regression this guards. The sibling
// engine-info plane's wiring is proven end to end by TestEngineInfoShowsEngineKeyPublic,
// whose merged `iris engine info` readout now asserts the role and uptime GET /info
// supplies.

// TestEngineInspectServesDDL drives the real iris binary against a live daemon and
// proves `iris engine inspect` serves the engine-table DDL through the shipped
// Run() codepath: an unwired inspect plane 500s ("api: inspect not available") and
// the CLI exits operation-failed; the wired plane exits clean with the DDL dump.
func TestEngineInspectServesDDL(t *testing.T) {
	t.Run("S11/inspect-dumps-engine-ddl", func(t *testing.T) {
		bin := Build(t)
		ws := shortWorkspace(t)
		socket := filepath.Join(ws, ".iris", "iris.sock")

		// Install (external: no-op under IRIS_PG_DSN; managed: cached download), then
		// start the daemon detached so it serves the read API against a real Postgres.
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

		// GET /inspect serves the embedded engine-table DDL. Inspect is a read served
		// on any role, so it needs no leadership; a bare reachable daemon suffices.
		jres := bin.Run(t, RunOptions{Args: []string{"--json", "engine", "inspect"}, Dir: ws, Timeout: time.Minute})
		jres.RequireExit(t, 0)
		var doc struct {
			Data struct {
				DDL []string `json:"ddl"`
			} `json:"data"`
		}
		jres.DecodeJSON(t, &doc)
		if len(doc.Data.DDL) == 0 {
			t.Fatal("engine inspect --json served no DDL statements; the wired inspect plane dumps the engine schema")
		}
		joined := strings.Join(doc.Data.DDL, "\n")
		if !strings.Contains(joined, "CREATE TABLE") {
			t.Errorf("engine inspect DDL carries no CREATE TABLE statement:\n%s", joined)
		}
	})
}
