//go:build conformance

package conformance

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
)

// This file proves the resident-pipeline contract (#192) end to end: a script that speaks the iteration protocol (stdin "go <run_id>", stdout "done <status>") iterates N runs on ONE process and ONE Postgres backend with per-iteration run rows and journal attribution; an operator cancel parks the pipeline until a manual run; and the folder surface refuses out-of-bound and sibling-conflicting declarations at apply.

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

// residentWriterScript is a protocol-speaking resident: ONE psql coprocess (one backend) held across iterations; each go re-attributes via SET iris.run_id and inserts one row keyed by the run id.
const residentWriterScript = `import os, subprocess, sys

url = os.environ.get("IRIS_DB_URL", "")
if not url:
    print("missing IRIS_DB_URL", file=sys.stderr)
    sys.exit(2)
p = subprocess.Popen(["psql", url, "-q", "-A", "-t", "-v", "ON_ERROR_STOP=1"],
                     stdin=subprocess.PIPE, stdout=subprocess.PIPE, text=True, bufsize=1)
for line in sys.stdin:
    parts = line.split()
    if len(parts) != 2 or parts[0] != "go":
        continue
    rid = int(parts[1])
    p.stdin.write("SET iris.run_id = %d;\nINSERT INTO testdata.items (id, val) VALUES (%d, 'iter');\n\\echo ITER_OK\n" % (rid, rid))
    p.stdin.flush()
    ok = False
    while True:
        out = p.stdout.readline()
        if not out:
            break
        if out.strip() == "ITER_OK":
            ok = True
            break
    print("done 0" if ok else "done 1", flush=True)
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

			// The very first run can race role provisioning within the apply and spawn on the admin fallback; the base-keyed session recycles onto the scoped role at the next run, so wait for the role's backend before sampling iterations.
			var mark int64
			dl := time.Now().Add(120 * time.Second)
			for time.Now().Before(dl) {
				var backends int
				_ = data.QueryRow(ctx, "SELECT count(*) FROM pg_stat_activity WHERE usename='iris_pipeline_res_writer'").Scan(&backends)
				if backends == 1 {
					_ = meta.QueryRow(ctx, "SELECT coalesce(max(id),0) FROM runs WHERE pipeline='res_writer' AND state='succeeded'").Scan(&mark)
					break
				}
				time.Sleep(150 * time.Millisecond)
			}

			// Three protocol iterations on the scoped session.
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
				t.Fatalf("resident pipeline produced %d succeeded runs on the scoped session within 120s, want 3", len(ids))
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

			// One backend: the resident process holds a single data connection across iterations.
			var backends int
			if err := data.QueryRow(ctx,
				"SELECT count(*) FROM pg_stat_activity WHERE usename='iris_pipeline_res_writer'").Scan(&backends); err != nil {
				t.Fatalf("read pg_stat_activity: %v", err)
			}
			if backends != 1 {
				t.Errorf("pipeline role holds %d backends, want 1 (one persistent connection)", backends)
			}
		})

		t.Run("cancel-parks-until-manual-run", func(t *testing.T) {
			ensurePython(t)
			freshDatabases(t)
			bin := Build(t)
			ws := shortWorkspace(t)
			// Hangs on its FIRST run only (marker file); after the cancel park, a manual run takes the marker branch and succeeds.
			script := "import os, time\nif not os.path.exists(\"hang.marker\"):\n    open(\"hang.marker\", \"w\").close()\n    while True:\n        time.sleep(0.2)\n"
			decl := "name: res_park\nrun: [python, main.py]\n"
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

			var hungID int64
			dl := time.Now().Add(90 * time.Second)
			for time.Now().Before(dl) {
				_ = meta.QueryRow(ctx, "SELECT coalesce(max(id),0) FROM runs WHERE pipeline='res_park' AND state='running'").Scan(&hungID)
				if hungID != 0 {
					break
				}
				time.Sleep(150 * time.Millisecond)
			}
			if hungID == 0 {
				t.Fatal("no running run to cancel within 90s")
			}
			bin.Run(t, RunOptions{Args: []string{"run", "cancel", fmt.Sprint(hungID)}, Dir: ws, Timeout: 20 * time.Second}).RequireExit(t, 0)

			var state string
			dl = time.Now().Add(20 * time.Second)
			for time.Now().Before(dl) {
				_ = meta.QueryRow(ctx, "SELECT state FROM runs WHERE id=$1", hungID).Scan(&state)
				if state == "dead_lettered" {
					break
				}
				time.Sleep(150 * time.Millisecond)
			}
			if state != "dead_lettered" {
				t.Fatalf("cancelled run %d state=%q, want dead_lettered", hungID, state)
			}

			// Parked: the loop must not resurrect the pipeline on its own.
			time.Sleep(3 * time.Second)
			var afterCancel int64
			_ = meta.QueryRow(ctx, "SELECT coalesce(max(id),0) FROM runs WHERE pipeline='res_park'").Scan(&afterCancel)
			if afterCancel != hungID {
				t.Fatalf("loop resurrected the cancelled pipeline: max run id %d, want the cancelled %d", afterCancel, hungID)
			}

			// A manual run releases the park; the loop resumes afterwards (a later cause=loop run appears).
			bin.Run(t, RunOptions{Args: []string{"pipeline", "run", "res_park"}, Dir: ws, Timeout: 2 * time.Minute}).RequireExit(t, 0)
			dl = time.Now().Add(90 * time.Second)
			var loopID int64
			for time.Now().Before(dl) {
				_ = meta.QueryRow(ctx,
					"SELECT coalesce(max(id),0) FROM runs WHERE pipeline='res_park' AND cause='loop' AND state='succeeded' AND id>$1", hungID).Scan(&loopID)
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
