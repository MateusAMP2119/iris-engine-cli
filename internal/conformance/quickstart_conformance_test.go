//go:build conformance

package conformance

import (
	"context"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// TestQuickstartFullTour drives `iris quickstart --yes` (real binary, unattended, the invoking directory as workspace) and proves the #202 contract: the engine is up and idle, nothing is registered, nothing ever ran, the sample is staged, and the wrap-up's printed recipe then works for real through provenance and the park.
func TestQuickstartFullTour(t *testing.T) {
	freshDatabases(t)
	bin := Build(t)
	ws := shortWorkspace(t)

	res := bin.Run(t, RunOptions{Args: []string{"quickstart", "--yes"}, Dir: ws, Timeout: 10 * time.Minute})
	res.RequireExit(t, 0)

	if out := string(res.Stdout); !strings.Contains(out, "running and idle") || !strings.Contains(out, "cheat-sheet") {
		t.Errorf("tour did not close on the idle end state\nstdout:\n%s", out)
	}

	socket := filepath.Join(ws, ".iris", "iris.sock")
	t.Cleanup(func() {
		bin.Run(t, RunOptions{Args: []string{"engine", "stop"}, Dir: ws, Timeout: 30 * time.Second})
	})

	// Staged, not registered: the sample landed on disk and nowhere else.
	if _, err := os.Stat(filepath.Join(ws, "pipelines", "hello_iris", "iris-declare.yaml")); err != nil {
		t.Fatalf("quickstart did not stage the sample declaration: %v\nstdout:\n%s\nstderr:\n%s", err, res.Stdout, res.Stderr)
	}
	if _, err := os.Stat(filepath.Join(ws, "iris-quickstart-demo")); err == nil {
		t.Fatal("quickstart created ./iris-quickstart-demo; the invoking directory is the workspace")
	}
	assertEngineAnswering(t, socket)
	assertNothingRegistered(t, socket)
	assertNoRunsEver(t, bin, ws)

	// The printed recipe works for real: register, run once, ask provenance.
	bin.Run(t, RunOptions{Args: []string{"declare", "apply", "pipelines/hello_iris"}, Dir: ws, Timeout: time.Minute}).RequireExit(t, 0)
	bin.Run(t, RunOptions{Args: []string{"pipeline", "run", "hello_iris"}, Dir: ws, Timeout: 2 * time.Minute}).RequireExit(t, 0)

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

	// The recipe's last line parks the loop and the park holds.
	bin.Run(t, RunOptions{Args: []string{"pipeline", "stop", "hello_iris"}, Dir: ws, Timeout: time.Minute}).RequireExit(t, 0)
	first := newestRunID(t, bin, ws)
	time.Sleep(3 * time.Second)
	if again := newestRunID(t, bin, ws); again != first {
		t.Errorf("parked loop advanced from run %s to %s", first, again)
	}

	// A second unattended run is clean: idempotent steps, running engine adopted.
	again := bin.Run(t, RunOptions{Args: []string{"quickstart", "--yes"}, Dir: ws, Timeout: 5 * time.Minute})
	again.RequireExit(t, 0)
	assertEngineAnswering(t, socket)
}

// TestQuickstartPickedFullTour drives `iris quickstart --yes --pipeline word_frequency`: only the pick is staged, nothing is registered or run by the tour, and the pick's recipe works for real.
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

	if _, err := os.Stat(filepath.Join(ws, "pipelines", "word_frequency", "iris-declare.yaml")); err != nil {
		t.Fatalf("picked entry not staged: %v\nstdout:\n%s\nstderr:\n%s", err, res.Stdout, res.Stderr)
	}
	if _, err := os.Stat(filepath.Join(ws, "pipelines", "hello_iris")); err == nil {
		t.Error("unpicked hello_iris staged; only the pick lands")
	}
	assertEngineAnswering(t, socket)
	assertNothingRegistered(t, socket)
	assertNoRunsEver(t, bin, ws)

	bin.Run(t, RunOptions{Args: []string{"declare", "apply", "pipelines/word_frequency"}, Dir: ws, Timeout: time.Minute}).RequireExit(t, 0)
	bin.Run(t, RunOptions{Args: []string{"pipeline", "run", "word_frequency"}, Dir: ws, Timeout: 2 * time.Minute}).RequireExit(t, 0)

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

	bin.Run(t, RunOptions{Args: []string{"pipeline", "stop", "word_frequency"}, Dir: ws, Timeout: time.Minute}).RequireExit(t, 0)

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

// assertNothingRegistered fails the test unless the full pipeline listing is empty (#202: the tour registers nothing).
func assertNothingRegistered(t *testing.T, socket string) {
	t.Helper()
	resp, err := HTTPOverSocket(socket).Get("http://iris/pipeline/list?all=1")
	if err != nil {
		t.Fatalf("pipeline listing on %s: %v", socket, err)
	}
	defer func() { _ = resp.Body.Close() }()
	body, _ := io.ReadAll(resp.Body)
	if !strings.Contains(string(body), `"pipelines":[]`) {
		t.Errorf("tour left pipelines registered: %s", body)
	}
}

// assertNoRunsEver fails the test unless the whole run history is empty (#202: the tour fires nothing).
func assertNoRunsEver(t *testing.T, bin *Binary, ws string) {
	t.Helper()
	res := bin.Run(t, RunOptions{Args: []string{"ps", "--all", "--json"}, Dir: ws, Timeout: time.Minute})
	res.RequireExit(t, 0)
	var env struct {
		Data struct {
			Runs []struct{} `json:"runs"`
		} `json:"data"`
	}
	res.DecodeJSON(t, &env)
	if len(env.Data.Runs) != 0 {
		t.Errorf("tour left %d runs in the history, want none", len(env.Data.Runs))
	}
}

// newestRunID reads the newest run id off the full history readout.
func newestRunID(t *testing.T, bin *Binary, ws string) string {
	t.Helper()
	res := bin.Run(t, RunOptions{Args: []string{"ps", "--all", "--json"}, Dir: ws, Timeout: time.Minute})
	res.RequireExit(t, 0)
	var env struct {
		Data struct {
			Runs []struct {
				ID string `json:"id"`
			} `json:"runs"`
		} `json:"data"`
	}
	res.DecodeJSON(t, &env)
	if len(env.Data.Runs) == 0 {
		t.Fatal("no runs in the history readout")
	}
	return env.Data.Runs[0].ID
}
