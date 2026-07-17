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

// This file proves the resident-pipeline contract under the turn protocol (#206, extending #192) end to end: a script that speaks the JSON frame protocol (stdin go/row/run, stdout row frames and a done terminal) iterates N turns on ONE process with per-turn run rows, engine-mediated writes, and exact journal attribution -- the pipeline itself holds no database credentials; an operator stop parks the pipeline until a manual run; and the folder surface refuses out-of-bound and sibling-conflicting declarations at apply.

// residentSchemaYAML declares the testdata.items table the resident writer targets.
const residentSchemaYAML = "schema: testdata\ntable: items\ncolumns:\n  - name: id\n    type: int\n    primary_key: true\n  - name: val\n    type: text\n"

// writeResidentWorkspace lays out a minimal workspace: schemas/testdata/items plus one pipeline folder running main.py.
func writeResidentWorkspace(t *testing.T, ws, pipeline, declYAML, script string) {
	t.Helper()
	schemaDir := filepath.Join(ws, "schemas", "testdata", "items")
	if err := os.MkdirAll(schemaDir, 0o755); err != nil {
		t.Fatalf("mkdir schema: %v", err)
	}
	if err := os.WriteFile(filepath.Join(schemaDir, "table.yaml"), []byte(residentSchemaYAML), 0o644); err != nil {
		t.Fatalf("write table.yaml: %v", err)
	}
	writePipelineFolder(t, ws, pipeline, declYAML, script)
}

// writePipelineFolder writes one pipeline folder (iris-declare.yaml + main.py) under pipelines/.
func writePipelineFolder(t *testing.T, ws, folder, declYAML, script string) {
	t.Helper()
	dir := filepath.Join(ws, "pipelines", folder)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("mkdir pipeline %s: %v", folder, err)
	}
	if err := os.WriteFile(filepath.Join(dir, "iris-declare.yaml"), []byte(declYAML), 0o644); err != nil {
		t.Fatalf("write decl %s: %v", folder, err)
	}
	if script != "" {
		if err := os.WriteFile(filepath.Join(dir, "main.py"), []byte(script), 0o644); err != nil { //nolint:gosec // G306: workspace script, dev-run convention.
			t.Fatalf("write script %s: %v", folder, err)
		}
	}
}

// startEngine installs and detaches an engine on ws, returning a stop func.
func startEngine(t *testing.T, bin *Binary, ws string) func() {
	t.Helper()
	socket := filepath.Join(ws, ".iris", "iris.sock")
	bin.Run(t, RunOptions{Args: []string{"engine", "install"}, Dir: ws, Timeout: 5 * time.Minute}).RequireExit(t, 0)
	bin.Run(t, RunOptions{Args: []string{"engine", "start", "-d"}, Dir: ws, Timeout: 2 * time.Minute}).RequireExit(t, 0)
	stop := func() {
		bin.Run(t, RunOptions{Args: []string{"engine", "stop"}, Dir: ws, Timeout: 30 * time.Second})
	}
	readyCtx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	if err := WaitForSocket(readyCtx, socket); err != nil {
		stop()
		t.Fatalf("daemon socket never became ready: %v", err)
	}
	if !waitForLeader(t, socket) {
		stop()
		t.Fatal("daemon never became leader")
	}
	return stop
}

// residentWriterScript is a frame-speaking resident: per turn it answers one
// declared-write row keyed by the turn number and echoes done. It opens no
// database connection -- the engine performs the write with the run's exact
// attribution (#206).
const residentWriterScript = PyTurnPrelude + `
def on_turn(turn, rows):
    emit("testdata.items", {"id": turn, "val": "iter"})
    done(turn)

turn_loop(on_turn)
`

func TestResidentPipeline(t *testing.T) {
	t.Run("resident-pipeline", func(t *testing.T) {
		t.Run("iterates-on-one-pid-and-one-backend", func(t *testing.T) {
			ensurePython(t)
			freshDatabases(t)
			bin := Build(t)
			ws := shortWorkspace(t)
			decl := "name: res_writer\nrun: [python, main.py]\nwrites:\n  - table: testdata.items\n    fields: [id, val]\n"
			writeResidentWorkspace(t, ws, "res_writer", decl, residentWriterScript)
			stop := startEngine(t, bin, ws)
			defer stop()
			bin.Run(t, RunOptions{Args: []string{"declare", "apply", "pipelines/res_writer"}, Dir: ws, Timeout: time.Minute}).RequireExit(t, 0)

			ctx := context.Background()
			meta, err := pgx.Connect(ctx, metaDSN(t, ws))
			if err != nil {
				t.Fatalf("connect meta: %v", err)
			}
			defer func() { _ = meta.Close(ctx) }()
			data, err := pgx.Connect(ctx, dataDSN(t, ws))
			if err != nil {
				t.Fatalf("connect data: %v", err)
			}
			defer func() { _ = data.Close(ctx) }()

			// Sample three producing turns: each mints its own run row at commit.
			var mark int64
			dl := time.Now().Add(120 * time.Second)
			var ids []int64
			for time.Now().Before(dl) {
				rows, err := meta.Query(ctx, "SELECT id FROM runs WHERE pipeline='res_writer' AND state='succeeded' AND id>$1 ORDER BY id LIMIT 3", mark)
				if err != nil {
					t.Fatalf("read succeeded runs: %v", err)
				}
				ids = ids[:0]
				for rows.Next() {
					var id int64
					if err := rows.Scan(&id); err != nil {
						t.Fatalf("scan run id: %v", err)
					}
					ids = append(ids, id)
				}
				rows.Close()
				if len(ids) >= 3 {
					break
				}
				time.Sleep(150 * time.Millisecond)
			}
			if len(ids) < 3 {
				t.Fatalf("resident pipeline produced %d succeeded runs within 120s, want 3", len(ids))
			}

			// One process: every sampled iteration's run row carries the same process-group handle.
			var handles int
			if err := meta.QueryRow(ctx,
				"SELECT count(DISTINCT handle) FROM runs WHERE pipeline='res_writer' AND state='succeeded' AND id = ANY($1)", ids).Scan(&handles); err != nil {
				t.Fatalf("read distinct handles: %v", err)
			}
			if handles != 1 {
				t.Errorf("succeeded iterations span %d process groups, want 1 (one resident PID)", handles)
			}

			// Per-iteration attribution: each run journals its own insert, keyed by its own run id.
			for _, id := range ids {
				var n int
				if err := data.QueryRow(ctx,
					`SELECT count(*) FROM public.data_journal WHERE run_id=$1 AND schema='testdata' AND "table"='items' AND op='insert'`, id).Scan(&n); err != nil {
					t.Fatalf("read journal for run %d: %v", id, err)
				}
				if n != 1 {
					t.Errorf("run %d journaled %d inserts, want exactly its own 1", id, n)
				}
			}

			// No pipeline credentials (#206): the worker process never connects to
			// the data database, so its role holds ZERO backends.
			var backends int
			if err := data.QueryRow(ctx,
				"SELECT count(*) FROM pg_stat_activity WHERE usename='iris_pipeline_res_writer'").Scan(&backends); err != nil {
				t.Fatalf("read pg_stat_activity: %v", err)
			}
			if backends != 0 {
				t.Errorf("pipeline role holds %d backends, want 0 (the engine mediates every database access)", backends)
			}
		})

		t.Run("stop-parks-until-manual-run", func(t *testing.T) {
			ensurePython(t)
			freshDatabases(t)
			bin := Build(t)
			ws := shortWorkspace(t)
			// Hangs on its FIRST turn only (marker file), answering nothing -- under
			// the turn protocol a hung turn holds its lane with NO run row, so the
			// operator surface is the pipeline-level stop. After the park, a manual
			// run takes the marker branch, produces a row, and succeeds.
			script := "import os\n" + PyTurnPrelude + `
import time
def on_turn(turn, rows):
    if not os.path.exists("hang.marker"):
        open("hang.marker", "w").close()
        while True:
            time.sleep(0.2)
    emit("testdata.items", {"id": turn, "val": "released"})
    done(turn)

turn_loop(on_turn)
`
			decl := "name: res_park\nrun: [python, main.py]\nwrites:\n  - table: testdata.items\n    fields: [id, val]\n"
			writeResidentWorkspace(t, ws, "res_park", decl, script)
			stop := startEngine(t, bin, ws)
			defer stop()
			bin.Run(t, RunOptions{Args: []string{"declare", "apply", "pipelines/res_park"}, Dir: ws, Timeout: time.Minute}).RequireExit(t, 0)

			ctx := context.Background()
			meta, err := pgx.Connect(ctx, metaDSN(t, ws))
			if err != nil {
				t.Fatalf("connect meta: %v", err)
			}
			defer func() { _ = meta.Close(ctx) }()

			// The hung turn leaves no run row; the marker file is the observable
			// proof the worker took its first turn and is now holding the lane.
			marker := filepath.Join(ws, "pipelines", "res_park", "hang.marker")
			dl := time.Now().Add(90 * time.Second)
			for time.Now().Before(dl) {
				if _, err := os.Stat(marker); err == nil {
					break
				}
				time.Sleep(150 * time.Millisecond)
			}
			if _, err := os.Stat(marker); err != nil {
				t.Fatal("worker never took its first turn within 90s")
			}

			// The pipeline-level stop parks: it mints the park row (a quiet loop has
			// no run to dead-letter) and kills the hung worker.
			bin.Run(t, RunOptions{Args: []string{"pipeline", "stop", "res_park"}, Dir: ws, Timeout: 20 * time.Second}).RequireExit(t, 0)

			var parkID int64
			var state, reason string
			dl = time.Now().Add(20 * time.Second)
			for time.Now().Before(dl) {
				_ = meta.QueryRow(ctx,
					"SELECT r.id, r.state, coalesce(d.reason,'') FROM runs r LEFT JOIN dead_letters d ON d.run_id=r.id WHERE r.pipeline='res_park' ORDER BY r.id DESC LIMIT 1").Scan(&parkID, &state, &reason)
				if state == "dead_lettered" && reason == "stopped" {
					break
				}
				time.Sleep(150 * time.Millisecond)
			}
			if state != "dead_lettered" || reason != "stopped" {
				t.Fatalf("park row = (state %q, reason %q), want a dead_lettered stopped run", state, reason)
			}

			// Parked: the loop must not resurrect the pipeline on its own, and the
			// killed hung turn must not have minted a failed dead letter over the park.
			time.Sleep(3 * time.Second)
			var afterStop int64
			_ = meta.QueryRow(ctx, "SELECT coalesce(max(id),0) FROM runs WHERE pipeline='res_park'").Scan(&afterStop)
			if afterStop != parkID {
				t.Fatalf("loop resurrected the stopped pipeline: max run id %d, want the park row %d", afterStop, parkID)
			}

			// A manual run releases the park; the loop resumes afterwards (a later
			// producing cause=loop run appears).
			bin.Run(t, RunOptions{Args: []string{"pipeline", "run", "res_park"}, Dir: ws, Timeout: 2 * time.Minute}).RequireExit(t, 0)
			dl = time.Now().Add(90 * time.Second)
			var loopID int64
			for time.Now().Before(dl) {
				_ = meta.QueryRow(ctx,
					"SELECT coalesce(max(id),0) FROM runs WHERE pipeline='res_park' AND cause='loop' AND state='succeeded' AND id>$1", parkID).Scan(&loopID)
				if loopID != 0 {
					break
				}
				time.Sleep(150 * time.Millisecond)
			}
			if loopID == 0 {
				t.Fatal("loop did not resume after the manual run released the park")
			}
		})

		t.Run("apply-refuses-surface-violations", func(t *testing.T) {
			ensurePython(t)
			freshDatabases(t)
			bin := Build(t)
			ws := shortWorkspace(t)

			schemaDir := filepath.Join(ws, "schemas", "testdata", "items")
			if err := os.MkdirAll(schemaDir, 0o755); err != nil {
				t.Fatalf("mkdir schema: %v", err)
			}
			if err := os.WriteFile(filepath.Join(schemaDir, "table.yaml"), []byte(residentSchemaYAML), 0o644); err != nil {
				t.Fatalf("write table.yaml: %v", err)
			}

			composer := "lane: grp\norder: [wa, wb]\nreads:\n  - table: testdata.items\n    fields: [id, val]\nwrites:\n  - table: testdata.items\n    fields: [id, val]\n"
			if err := os.MkdirAll(filepath.Join(ws, "pipelines", "grp"), 0o755); err != nil {
				t.Fatalf("mkdir lane folder: %v", err)
			}
			if err := os.WriteFile(filepath.Join(ws, "pipelines", "grp", "iris-declare.yaml"), []byte(composer), 0o644); err != nil {
				t.Fatalf("write composer: %v", err)
			}
			noop := "print(\"noop\")\n"
			writePipelineFolder(t, ws, "grp/wa", "name: wa\nrun: [python, main.py]\nwrites:\n  - table: testdata.items\n    fields: [id, val]\n", noop)
			writePipelineFolder(t, ws, "grp/wb", "name: wb\nrun: [python, main.py]\n", noop)

			stop := startEngine(t, bin, ws)
			defer stop()

			bin.Run(t, RunOptions{Args: []string{"declare", "apply", "pipelines/grp"}, Dir: ws, Timeout: time.Minute}).RequireExit(t, 0)
			bin.Run(t, RunOptions{Args: []string{"declare", "apply", "pipelines/grp/wa"}, Dir: ws, Timeout: time.Minute}).RequireExit(t, 0)
			bin.Run(t, RunOptions{Args: []string{"declare", "apply", "pipelines/grp/wb"}, Dir: ws, Timeout: time.Minute}).RequireExit(t, 0)

			// Sibling write-claim conflict: wb now declares wa's exclusive write table.
			conflict := "name: wb\nrun: [python, main.py]\nwrites:\n  - table: testdata.items\n    fields: [id]\n"
			if err := os.WriteFile(filepath.Join(ws, "pipelines", "grp", "wb", "iris-declare.yaml"), []byte(conflict), 0o644); err != nil {
				t.Fatalf("rewrite wb decl: %v", err)
			}
			res := bin.Run(t, RunOptions{Args: []string{"declare", "apply", "pipelines/grp/wb"}, Dir: ws, Timeout: time.Minute})
			if res.ExitCode == 0 {
				t.Fatalf("apply accepted a sibling write-claim conflict; stdout=%s stderr=%s", res.Stdout, res.Stderr)
			}
			if out := string(res.Stdout) + string(res.Stderr); !strings.Contains(out, "sibling") {
				t.Errorf("refusal does not name the sibling claim: %s", out)
			}

			// Outside the folder surface: wb declares a table the composer never granted the group.
			outside := "name: wb\nrun: [python, main.py]\nwrites:\n  - table: testdata.other\n    fields: [id]\n"
			if err := os.WriteFile(filepath.Join(ws, "pipelines", "grp", "wb", "iris-declare.yaml"), []byte(outside), 0o644); err != nil {
				t.Fatalf("rewrite wb decl: %v", err)
			}
			res = bin.Run(t, RunOptions{Args: []string{"declare", "apply", "pipelines/grp/wb"}, Dir: ws, Timeout: time.Minute})
			if res.ExitCode == 0 {
				t.Fatalf("apply accepted a write outside the folder surface; stdout=%s stderr=%s", res.Stdout, res.Stderr)
			}
			if out := string(res.Stdout) + string(res.Stderr); !strings.Contains(out, "surface") {
				t.Errorf("refusal does not name the folder surface: %s", out)
			}
		})
	})
}
