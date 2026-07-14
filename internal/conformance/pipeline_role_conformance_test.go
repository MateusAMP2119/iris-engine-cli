//go:build conformance

package conformance

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
)

// This file is the least-privilege pipeline-role leg: a declare apply provisions
// the pipeline's own login role on the data database with exactly the declared
// field grants, records it in the meta access ledger, and every run's
// IRIS_DB_URL authenticates as that role -- never as the engine's admin
// identity, and never able to reach the meta control database.

// setupRolePipeline writes a pipeline whose main prints the IRIS_DB_URL it was
// handed, so the captured run log carries the identity the run connected as.
func setupRolePipeline(t *testing.T, ws, name, lane string) {
	t.Helper()
	schemaDir := filepath.Join(ws, "schemas", "testdata", "items")
	if err := os.MkdirAll(schemaDir, 0o755); err != nil {
		t.Fatalf("mkdir schema: %v", err)
	}
	tableYAML := "schema: testdata\ntable: items\ncolumns:\n  - name: id\n    type: int\n    primary_key: true\n  - name: val\n    type: text\n"
	if err := os.WriteFile(filepath.Join(schemaDir, "table.yaml"), []byte(tableYAML), 0o644); err != nil {
		t.Fatalf("write table.yaml: %v", err)
	}
	pipeDir := filepath.Join(ws, "pipelines", name)
	if err := os.MkdirAll(pipeDir, 0o755); err != nil {
		t.Fatalf("mkdir pipeline: %v", err)
	}
	decl := fmt.Sprintf("name: %s\nrun: [\"go\", \"run\", \"main.go\"]\nlane: %s\nwrites:\n  - table: testdata.items\n    fields: [id, val]\n", name, lane)
	if err := os.WriteFile(filepath.Join(pipeDir, "iris-declare.yaml"), []byte(decl), 0o644); err != nil {
		t.Fatalf("write decl: %v", err)
	}
	mainGo := "package main\n\nimport (\n\t\"fmt\"\n\t\"os\"\n)\n\nfunc main() { fmt.Println(\"DBURL=\" + os.Getenv(\"IRIS_DB_URL\")) }\n"
	if err := os.WriteFile(filepath.Join(pipeDir, "main.go"), []byte(mainGo), 0o644); err != nil {
		t.Fatalf("write main.go: %v", err)
	}
}

func TestRunsConnectAsPipelineRole(t *testing.T) {
	freshDatabases(t)
	bin := Build(t)

	ws := shortWorkspace(t)
	socket := filepath.Join(ws, ".iris", "iris.sock")
	setupRolePipeline(t, ws, "wrole", "roles")

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

	bin.Run(t, RunOptions{Args: []string{"declare", "apply", "pipelines/wrole"}, Dir: ws, Timeout: time.Minute}).RequireExit(t, 0)

	ctx, cancel2 := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel2()
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

	t.Run("runs-authenticate-as-the-role-not-the-admin", func(t *testing.T) {
		bin.Run(t, RunOptions{Args: []string{"pipeline", "run", "wrole"}, Dir: ws, Timeout: time.Minute}).RequireExit(t, 0)
		runID := manualRunForPipeline(t, ws, "wrole")

		res := bin.Run(t, RunOptions{Args: []string{"run", "logs", strconv.FormatInt(runID, 10)}, Dir: ws, Timeout: 30 * time.Second})
		res.RequireExit(t, 0)
		out := string(res.Stdout)
		if !strings.Contains(out, "DBURL=postgres://"+role+":") {
			t.Errorf("the run's IRIS_DB_URL does not authenticate as the pipeline role:\n%s", out)
		}
		if strings.Contains(out, "postgres://postgres:") || strings.Contains(out, "postgres://iris_admin:") {
			t.Errorf("the run's IRIS_DB_URL carries the admin identity:\n%s", out)
		}
		if !strings.Contains(out, "iris.run_id") {
			t.Errorf("the run's IRIS_DB_URL does not carry the iris.run_id attribution GUC:\n%s", out)
		}
	})
}
