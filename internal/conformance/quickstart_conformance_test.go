//go:build conformance

package conformance

import (
	"context"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// TestQuickstartFullTour drives `iris quickstart --yes` -- the real binary,
// unattended, in a fresh directory that IS the workspace (--yes never prompts
// and uses the invoking directory unchanged) -- through the whole first
// session against a real Postgres: it bootstraps the engine, materializes and
// applies the hello_iris sample, runs it, and `iris data provenance
// demo.colors green` names the authoring run. The engine is left running
// afterwards, and a second unattended run adopts it and exits 0.
func TestQuickstartFullTour(t *testing.T) {
	freshDatabases(t)
	bin := Build(t)
	ws := shortWorkspace(t)

	// Fresh directory: under --yes the invoking directory is the workspace, so
	// the whole session bootstraps right here without a single prompt (stdin is
	// not a terminal here, which is exactly the piped --yes contract).
	res := bin.Run(t, RunOptions{Args: []string{"quickstart", "--yes"}, Dir: ws, Timeout: 10 * time.Minute})
	res.RequireExit(t, 0)

	// The ceremony's end state: the engine is announced as left running and the
	// cheat-sheet of what the session used closes the tour.
	if out := string(res.Stdout); !strings.Contains(out, "still running") || !strings.Contains(out, "cheat-sheet") {
		t.Errorf("tour did not close on the ceremony end state (engine left running + cheat-sheet)\nstdout:\n%s", out)
	}

	socket := filepath.Join(ws, ".iris", "iris.sock")
	t.Cleanup(func() {
		bin.Run(t, RunOptions{Args: []string{"engine", "stop"}, Dir: ws, Timeout: 30 * time.Second})
	})

	// The sample was materialized into the workspace itself, no demo subdir.
	if _, err := os.Stat(filepath.Join(ws, "pipelines", "hello_iris", "iris-declare.yaml")); err != nil {
		t.Fatalf("quickstart did not materialize the sample declaration: %v\nstdout:\n%s\nstderr:\n%s", err, res.Stdout, res.Stderr)
	}
	if _, err := os.Stat(filepath.Join(ws, "iris-quickstart-demo")); err == nil {
		t.Fatal("quickstart created ./iris-quickstart-demo; the invoking directory is the workspace")
	}

	// The engine is left running: /healthz answers on the workspace socket.
	assertEngineAnswering(t, socket)

	// The sample run landed the seven rainbow colors in demo.colors.
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	conn := connectPG(t, dataDSN(t, ws))
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
		Dir:     ws,
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

// TestQuickstartPickedFullTour drives `iris quickstart --yes --pipeline
// word_frequency` -- the real binary, unattended, a non-default catalog pick,
// in a fresh directory that IS the workspace -- through the whole first
// session against a real Postgres: it bootstraps the engine, materializes and
// applies only the picked entry, runs it (the word counts computed wholly in
// Postgres), and `iris data provenance demo.word_counts hope` names the
// authoring run. A second unattended run exits 0.
func TestQuickstartPickedFullTour(t *testing.T) {
	freshDatabases(t)
	bin := Build(t)
	ws := shortWorkspace(t)

	res := bin.Run(t, RunOptions{Args: []string{"quickstart", "--yes", "--pipeline", "word_frequency"}, Dir: ws, Timeout: 10 * time.Minute})
	res.RequireExit(t, 0)

	socket := filepath.Join(ws, ".iris", "iris.sock")
	t.Cleanup(func() {
		bin.Run(t, RunOptions{Args: []string{"engine", "stop"}, Dir: ws, Timeout: 30 * time.Second})
	})

	// Only the picked entry materialized.
	if _, err := os.Stat(filepath.Join(ws, "pipelines", "word_frequency", "iris-declare.yaml")); err != nil {
		t.Fatalf("picked entry not materialized: %v\nstdout:\n%s\nstderr:\n%s", err, res.Stdout, res.Stderr)
	}
	if _, err := os.Stat(filepath.Join(ws, "pipelines", "hello_iris")); err == nil {
		t.Error("unpicked hello_iris materialized; only the pick lands")
	}
	assertEngineAnswering(t, socket)

	// The deterministic counts landed: 'hope' was counted.
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	conn := connectPG(t, dataDSN(t, ws))
	defer func() { _ = conn.Close(context.Background()) }()
	var hopeN int
	if err := conn.QueryRow(ctx, "SELECT n FROM demo.word_counts WHERE word = 'hope'").Scan(&hopeN); err != nil {
		t.Fatalf("read demo.word_counts row 'hope': %v", err)
	}
	if hopeN < 1 {
		t.Errorf("word 'hope' counted %d times, want at least 1", hopeN)
	}

	// The finale names the authoring run and the picked pipeline.
	prov := bin.Run(t, RunOptions{
		Args:    []string{"--json", "data", "provenance", "demo.word_counts", "hope"},
		Dir:     ws,
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
	if env.Data.Pipeline != "word_frequency" {
		t.Errorf("provenance pipeline = %q, want word_frequency", env.Data.Pipeline)
	}

	// Re-running the picked tour is clean: idempotent steps, exit 0.
	again := bin.Run(t, RunOptions{Args: []string{"quickstart", "--yes", "--pipeline", "word_frequency"}, Dir: ws, Timeout: 5 * time.Minute})
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
