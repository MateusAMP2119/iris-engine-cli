package cli

import (
	"bytes"
	"context"
	"net"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/MateusAMP2119/iris-engine-cli/internal/api"
	"github.com/MateusAMP2119/iris-engine-cli/internal/daemon"
)

// startInspectDaemon stands up an in-process daemon over a unix socket serving the
// REAL api mux with the real daemon inspect plane, so `iris engine inspect` reads
// the DDL dump through the real GET /inspect route. The mux is given the leader
// role so a non-GET request reaches the route itself and is refused there --
// proving the route can never mutate even on the one node that may.
func startInspectDaemon(t *testing.T, sock string) {
	t.Helper()
	ln, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatalf("listen unix %s: %v", sock, err)
	}
	role := api.NewRoleState()
	role.SetLeader()
	srv := &http.Server{Handler: api.NewMux(api.WithRole(role), api.WithInspect(daemon.NewInspectPlane())), ReadHeaderTimeout: 5 * time.Second}
	go func() { _ = srv.Serve(ln) }()
	t.Cleanup(func() { _ = srv.Shutdown(context.Background()) })
}

// TestEngineInspectDumpsDDL proves `iris engine inspect` dumps the engine-table
// DDL as a read-only operation: the dump names the meta control tables and the
// data journal as create-if-missing statements,
// every statement is a CREATE (nothing that could mutate state), repeated reads
// return the identical dump, and the route refuses any non-GET method.
func TestEngineInspectDumpsDDL(t *testing.T) {
	t.Setenv("IRIS_HOST", "")
	t.Setenv("IRIS_SOCKET", "")
	t.Setenv("IRIS_TOKEN", "")

	t.Run("inspect-dumps-engine-ddl", func(t *testing.T) {
		sock := shortSocket(t)
		startInspectDaemon(t, sock)

		var out, errb bytes.Buffer
		code := newApp(&out, &errb).run([]string{"--socket", sock, "engine", "inspect", "--json"})
		if code != exitOK {
			t.Fatalf("engine inspect exit = %d, want %d\nstderr: %s", code, exitOK, errb.String())
		}
		var doc struct {
			Data struct {
				DDL []string `json:"ddl"`
			} `json:"data"`
		}
		decodeSingleJSON(t, out.Bytes(), &doc)
		if len(doc.Data.DDL) == 0 {
			t.Fatal("inspect dumped no DDL")
		}
		dump := strings.Join(doc.Data.DDL, "\n")
		for _, table := range []string{"pipelines", "runs", "data_journal"} {
			if !strings.Contains(dump, table) {
				t.Errorf("inspect dump does not name engine table %q", table)
			}
		}
		for _, stmt := range doc.Data.DDL {
			if !strings.HasPrefix(stmt, "CREATE ") {
				t.Errorf("inspect dump statement is not a CREATE (a read-only dump renders create-if-missing DDL only): %q", stmt)
			}
			if strings.HasPrefix(stmt, "CREATE TABLE") && !strings.Contains(stmt, "IF NOT EXISTS") {
				t.Errorf("inspect dump table statement is not create-if-missing: %q", stmt)
			}
		}

		// Read-only: a second read returns the identical dump, and the human
		// rendering carries the same statements.
		var out2, errb2 bytes.Buffer
		code = newApp(&out2, &errb2).run([]string{"--socket", sock, "engine", "inspect", "--json"})
		if code != exitOK {
			t.Fatalf("second engine inspect exit = %d, want %d\nstderr: %s", code, exitOK, errb2.String())
		}
		if out.String() != out2.String() {
			t.Error("two inspect reads returned different dumps; inspect must be a pure read")
		}
		var human, herr bytes.Buffer
		code = newApp(&human, &herr).run([]string{"--socket", sock, "engine", "inspect"})
		if code != exitOK {
			t.Fatalf("human engine inspect exit = %d, want %d\nstderr: %s", code, exitOK, herr.String())
		}
		if !strings.Contains(human.String(), "data_journal") {
			t.Errorf("human inspect output does not name the data journal:\n%s", human.String())
		}

		// Mutation-free surface: the route serves GET only.
		client := &http.Client{Transport: &http.Transport{
			DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
				var d net.Dialer
				return d.DialContext(ctx, "unix", sock)
			},
		}}
		resp, err := client.Post("http://iris/inspect", "application/json", strings.NewReader("{}"))
		if err != nil {
			t.Fatalf("POST /inspect: %v", err)
		}
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusMethodNotAllowed {
			t.Errorf("POST /inspect status = %d, want %d (inspect can never mutate)", resp.StatusCode, http.StatusMethodNotAllowed)
		}
	})

	t.Run("no daemon reachable exits 3", func(t *testing.T) {
		sock := shortSocket(t) // nothing listening
		var out, errb bytes.Buffer
		code := newApp(&out, &errb).run([]string{"--socket", sock, "engine", "inspect"})
		if code != exitNoDaemon {
			t.Fatalf("no-daemon engine inspect exit = %d, want %d\nstderr: %s", code, exitNoDaemon, errb.String())
		}
	})
}
