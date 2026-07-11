//go:build conformance

package conformance

import (
	"context"
	"net/http"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// TestQuickstartFullTour drives `iris quickstart --yes` -- the real binary,
// unattended, in a fresh workspace -- through the whole first session against a
// real Postgres: it bootstraps the engine (creating ./iris-quickstart-demo and
// working there), materializes and applies the hello_iris sample, runs it, and
// `iris data provenance demo.colors green` names the authoring run. The engine
// is left running afterwards, and a second unattended run adopts it and exits 0.
//
// spec: S08/quickstart-full-tour
func TestQuickstartFullTour(t *testing.T) {
	freshDatabases(t)
	bin := Build(t)
	ws := shortWorkspace(t)

	// Fresh non-workspace directory: --yes must create ./iris-quickstart-demo and
	// bootstrap the whole session there without a single prompt (stdin is not a
	// terminal here, which is exactly the piped --yes contract).
	res := bin.Run(t, RunOptions{Args: []string{"quickstart", "--yes"}, Dir: ws, Timeout: 10 * time.Minute})
	res.RequireExit(t, 0)

	demo := filepath.Join(ws, "iris-quickstart-demo")
	socket := filepath.Join(demo, ".iris", "iris.sock")
	t.Cleanup(func() {
		bin.Run(t, RunOptions{Args: []string{"engine", "stop"}, Dir: demo, Timeout: 30 * time.Second})
	})

	// The sample was materialized into the demo workspace.
	if _, err := os.Stat(filepath.Join(demo, "pipelines", "hello_iris", "iris-declare.yaml")); err != nil {
		t.Fatalf("quickstart did not materialize the sample declaration: %v\nstdout:\n%s\nstderr:\n%s", err, res.Stdout, res.Stderr)
	}

	// The engine is left running: /healthz answers on the demo workspace socket.
	assertEngineAnswering(t, socket)

	// The sample run landed the seven rainbow colors in demo.colors.
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	conn := connectPG(t, dataDSN(t, demo))
	defer func() { _ = conn.Close(context.Background()) }()
	var colors int
	if err := conn.QueryRow(ctx, "SELECT count(*) FROM demo.colors").Scan(&colors); err != nil {
		t.Fatalf("count demo.colors: %v", err)
	}
	if colors != 7 {
		t.Errorf("demo.colors holds %d rows, want the 7 rainbow colors", colors)
	}

	// The finale's read answers for real: provenance on a row the run wrote names
	// the authoring run and the sample pipeline.
	prov := bin.Run(t, RunOptions{
		Args:    []string{"--json", "data", "provenance", "demo.colors", "green"},
		Dir:     demo,
		Timeout: time.Minute,
	})
	prov.RequireExit(t, 0)
	var env struct {
		Data struct {
			Authored bool           `json:"authored"`
			Author   map[string]any `json:"author"`
			Pipeline string         `json:"pipeline"`
		} `json:"data"`
	}
	prov.DecodeJSON(t, &env)
	if !env.Data.Authored || env.Data.Author == nil || env.Data.Author["run_id"] == nil {
		t.Errorf("provenance does not name the authoring run; authored=%v author=%v", env.Data.Authored, env.Data.Author)
	}
	if env.Data.Pipeline != "hello_iris" {
		t.Errorf("provenance pipeline = %q, want hello_iris", env.Data.Pipeline)
	}

	// A second unattended run is clean: every step is idempotent, the running
	// engine is adopted (install/start skipped), and the tour exits 0 again.
	again := bin.Run(t, RunOptions{Args: []string{"quickstart", "--yes"}, Dir: ws, Timeout: 5 * time.Minute})
	again.RequireExit(t, 0)
	assertEngineAnswering(t, socket)
}

// assertEngineAnswering fails the test unless a daemon answers GET /healthz
// with 200 on the given workspace socket.
func assertEngineAnswering(t *testing.T, socket string) {
	t.Helper()
	resp, err := HTTPOverSocket(socket).Get("http://iris/healthz")
	if err != nil {
		t.Fatalf("engine not answering on %s: %v", socket, err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("engine /healthz on %s = %d, want %d", socket, resp.StatusCode, http.StatusOK)
	}
}
