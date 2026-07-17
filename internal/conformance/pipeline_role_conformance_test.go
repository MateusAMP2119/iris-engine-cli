//go:build conformance

package conformance

import (
	"context"
	"fmt"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
)

// This file is the least-privilege pipeline-role leg under the turn protocol
// (#206). Declare apply still provisions the pipeline's own login role on the
// data database with exactly the declared field grants -- that grants ledger is
// where the engine reads a pipeline's declared access from -- but no run ever
// connects as the role. The predecessor contract asserted each run's
// IRIS_DB_URL authenticated as the role; that variable is gone. The successor:
// the pipeline process is handed no database credential of any kind, every
// write reaches the database as a stdout row frame the engine upserts on its
// own admin connection with the run's exact journal attribution, and the
// pipeline role therefore holds zero backends while its pipeline demonstrably
// writes.

// setupRolePipeline writes a workspace whose pipeline is a frame-speaking
// resident: each turn it logs the IRIS_DB_URL it sees to stderr (the run log),
// answers one declared-write row keyed by the turn number, and echoes done.
func setupRolePipeline(t *testing.T, ws, name string) {
	t.Helper()
	script := "import os\n" + PyTurnPrelude + `
def on_turn(turn, rows):
    sys.stderr.write("DBURL=[" + os.environ.get("IRIS_DB_URL", "") + "]\n")
    sys.stderr.flush()
    emit("testdata.items", {"id": turn, "val": "envprobe"})
    done(turn)

turn_loop(on_turn)
`
	decl := fmt.Sprintf("name: %s\nrun: [python, main.py]\nwrites:\n  - table: testdata.items\n    fields: [id, val]\n", name)
	writeResidentWorkspace(t, ws, name, decl, script)
}

func TestPipelineRoleNeverConnects(t *testing.T) {
	ensurePython(t)
	freshDatabases(t)
	bin := Build(t)

	ws := shortWorkspace(t)
	setupRolePipeline(t, ws, "wrole")
	stop := startEngine(t, bin, ws)
	defer stop()

	bin.Run(t, RunOptions{Args: []string{"declare", "apply", "pipelines/wrole"}, Dir: ws, Timeout: time.Minute}).RequireExit(t, 0)

	ctx := context.Background()
	meta, err := pgx.Connect(ctx, metaDSN(t, ws))
	if err != nil {
		t.Fatalf("connect meta db: %v", err)
	}
	defer func() { _ = meta.Close(ctx) }()
	data, err := pgx.Connect(ctx, dataDSN(t, ws))
	if err != nil {
		t.Fatalf("connect data db: %v", err)
	}
	defer func() { _ = data.Close(ctx) }()

	const role = "iris_pipeline_wrole"

	t.Run("apply-provisions-the-role-with-declared-grants", func(t *testing.T) {
		var login, super bool
		if err := data.QueryRow(ctx, `SELECT rolcanlogin, rolsuper FROM pg_roles WHERE rolname = $1`, role).Scan(&login, &super); err != nil {
			t.Fatalf("the pipeline role was not provisioned: %v", err)
		}
		if !login || super {
			t.Errorf("role attributes login=%v super=%v, want a plain login role", login, super)
		}
		var canWrite bool
		if err := data.QueryRow(ctx, `SELECT has_column_privilege($1, 'testdata.items'::regclass, 'val', 'INSERT')`, role).Scan(&canWrite); err != nil {
			t.Fatalf("has_column_privilege: %v", err)
		}
		if !canWrite {
			t.Error("the role lacks its declared write grant on testdata.items.val")
		}
		var metaConnect bool
		if err := data.QueryRow(ctx, `SELECT has_database_privilege($1, 'meta', 'CONNECT')`, role).Scan(&metaConnect); err != nil {
			t.Fatalf("has_database_privilege(meta): %v", err)
		}
		if metaConnect {
			t.Error("the pipeline role can CONNECT to the meta control database; provisioning must revoke it")
		}
	})

	t.Run("runs-hold-no-credentials-and-the-engine-writes", func(t *testing.T) {
		bin.Run(t, RunOptions{Args: []string{"pipeline", "run", "wrole"}, Dir: ws, Timeout: 2 * time.Minute}).RequireExit(t, 0)
		runID := manualRunForPipeline(t, ws, "wrole")

		var state string
		if err := meta.QueryRow(ctx, "SELECT state FROM runs WHERE id=$1", runID).Scan(&state); err != nil {
			t.Fatalf("read manual run state: %v", err)
		}
		if state != "succeeded" {
			t.Fatalf("manual run %d ended %q, want succeeded", runID, state)
		}

		// No credential in the run environment: the run log (stderr only) carries
		// the IRIS_DB_URL the worker saw -- empty, and no connection string of any
		// identity anywhere in the log.
		res := bin.Run(t, RunOptions{Args: []string{"run", "logs", strconv.FormatInt(runID, 10)}, Dir: ws, Timeout: 30 * time.Second})
		res.RequireExit(t, 0)
		out := string(res.Stdout)
		if !strings.Contains(out, "DBURL=[]") {
			t.Errorf("the run saw a non-empty IRIS_DB_URL; pipelines hold no database credentials:\n%s", out)
		}
		if strings.Contains(out, "postgres://") {
			t.Errorf("the run environment carries a connection string:\n%s", out)
		}

		// The engine performed the run's write with exact attribution: the row
		// frame landed in the journal keyed by this run id, and none of the run's
		// journal rows were written by a pipeline role -- the engine's own
		// connection wrote them.
		var journaled int
		if err := data.QueryRow(ctx,
			`SELECT count(*) FROM public.data_journal WHERE run_id=$1 AND schema='testdata' AND "table"='items'`, runID).Scan(&journaled); err != nil {
			t.Fatalf("read journal for run %d: %v", runID, err)
		}
		if journaled == 0 {
			t.Errorf("run %d journaled no writes; the engine must upsert the run's row frames", runID)
		}
		var asPipeline int
		if err := data.QueryRow(ctx,
			`SELECT count(*) FROM public.data_journal WHERE run_id=$1 AND pg_role LIKE 'iris_pipeline_%'`, runID).Scan(&asPipeline); err != nil {
			t.Fatalf("read journal roles for run %d: %v", runID, err)
		}
		if asPipeline != 0 {
			t.Errorf("run %d has %d journal rows written by a pipeline role, want 0 (the engine writes)", runID, asPipeline)
		}

		// Zero backends: the role exists (provisioned above) and its pipeline just
		// wrote a row, yet the role never opened a connection.
		var backends int
		if err := data.QueryRow(ctx, "SELECT count(*) FROM pg_stat_activity WHERE usename=$1", role).Scan(&backends); err != nil {
			t.Fatalf("read pg_stat_activity: %v", err)
		}
		if backends != 0 {
			t.Errorf("pipeline role holds %d backends, want 0 (the engine mediates every database access)", backends)
		}
	})
}
