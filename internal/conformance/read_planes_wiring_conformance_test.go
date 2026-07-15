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
// production Run() codepath. The plane behavior itself
// is covered by an integration test that constructs the mux with the handler
// installed directly; that test passes even when Run() forgets to wire the
// handler. Only driving the shipped binary against a live daemon proves the route
// the CLI actually reaches serves the real DDL dump rather than the unwired
// internal-fault envelope -- exactly the regression this guards. The sibling
// ps plane's wiring is proven end to end by TestPsServesRuntimeReadout below.

// TestEngineInspectServesDDL drives the real iris binary against a live daemon and
// proves `iris engine inspect` serves the engine-table DDL through the shipped
// Run() codepath: an unwired inspect plane 500s ("api: inspect not available") and
// the CLI exits operation-failed; the wired plane exits clean with the DDL dump.
func TestEngineInspectServesDDL(t *testing.T) {
	t.Run("inspect-dumps-engine-ddl", func(t *testing.T) {
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

// TestPsServesRuntimeReadout drives the real iris binary against a live daemon
// and proves `iris ps` serves the process-status readout through the shipped
// Run() codepath: an unwired ps plane 500s ("api: ps not available") and the
// CLI exits operation-failed; the wired plane exits clean with the engine
// block -- the leadership role, a rendered uptime, the daemon's pid -- and the
// run rows (empty on a fresh engine, the whole history under -a).
func TestPsServesRuntimeReadout(t *testing.T) {
	t.Run("ps-serves-runtime-readout", func(t *testing.T) {
		bin := Build(t)
		ws := shortWorkspace(t)
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

		// Ps is a read served on any role, but the engine block reports the live
		// role: hold until leadership so the readout is deterministic.
		if !waitForLeader(t, socket) {
			t.Fatal("daemon never became leader; cannot assert the ps engine block")
		}

		jres := bin.Run(t, RunOptions{Args: []string{"--json", "ps"}, Dir: ws, Timeout: time.Minute})
		jres.RequireExit(t, 0)
		var doc struct {
			Data struct {
				Engine struct {
					Role   string `json:"role"`
					Uptime string `json:"uptime"`
					PID    int    `json:"pid"`
				} `json:"engine"`
				Runs []struct {
					ID string `json:"id"`
				} `json:"runs"`
			} `json:"data"`
		}
		jres.DecodeJSON(t, &doc)
		if doc.Data.Engine.Role != "leader" {
			t.Errorf("ps --json engine.role = %q, want leader (the wired plane reports the live role)", doc.Data.Engine.Role)
		}
		if doc.Data.Engine.Uptime == "" {
			t.Error("ps --json reports no uptime; the wired plane always renders one")
		}
		if doc.Data.Engine.PID == 0 {
			t.Error("ps --json reports no pid; the wired plane reports the daemon's")
		}
		if doc.Data.Runs == nil {
			t.Error("ps --json carries no runs array; the readout always carries one, possibly empty")
		}

		// The human surface reports the same facts, and -q stays silent on a
		// fresh engine (no queued or running run).
		hres := bin.Run(t, RunOptions{Args: []string{"ps"}, Dir: ws, Timeout: time.Minute})
		hres.RequireExit(t, 0)
		if out := string(hres.Stdout); !strings.Contains(out, "leader") || !strings.Contains(out, "RUN") {
			t.Errorf("human ps did not render the engine block and run table:\n%s", out)
		}
		qres := bin.Run(t, RunOptions{Args: []string{"ps", "-q"}, Dir: ws, Timeout: time.Minute})
		qres.RequireExit(t, 0)
		if out := strings.TrimSpace(string(qres.Stdout)); out != "" {
			t.Errorf("ps -q on a fresh engine = %q, want no output", out)
		}
	})
}
