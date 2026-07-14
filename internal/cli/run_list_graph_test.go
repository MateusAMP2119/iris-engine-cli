package cli

import (
	"bytes"
	"encoding/json"
	"net"
	"net/http"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/MateusAMP2119/iris-lakehouse/internal/golden"
)

// This file is the honesty suite for `iris run list --graph`, the lineage rail
// rendering. The rendering contract is honesty made testable: a solid stroke is
// a run_inputs edge and nothing else, a dotted rail is same-pipeline serial
// order (sequence, never ancestry), a replay is an annotation never an edge,
// run-id gaps stay visible, --graph is presentation only (--json never carries
// drawing), and past the rail cap the weave refuses and prints the
// --lane/--pipeline filter hint. --ascii swaps to git's glyph vocabulary and is
// pinned byte-for-byte by a golden.

// clearTargetEnv unsets the ambient IRIS_* target variables so a test resolves
// its daemon target only from the explicit --socket it passes (test-env
// isolation).
func clearTargetEnv(t *testing.T) {
	t.Helper()
	t.Setenv("IRIS_HOST", "")
	t.Setenv("IRIS_SOCKET", "")
	t.Setenv("IRIS_TOKEN", "")
}

// startRunsDaemon stands an in-process HTTP server over a real unix socket that
// serves the /runs?include=inputs read the CLI issues, returning rows. It is the
// fakes-plus-real-local-socket-I/O integration pattern (no live Postgres): the
// real CLI HTTP client dials the socket and renders the served rows.
func startRunsDaemon(t *testing.T, sock string, rows []runRow) {
	t.Helper()
	ln, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatalf("listen unix %s: %v", sock, err)
	}
	mux := http.NewServeMux()
	mux.HandleFunc("/runs", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"data": map[string]any{"runs": rows},
		})
	})
	srv := &http.Server{Handler: mux, ReadHeaderTimeout: 5 * time.Second}
	go func() { _ = srv.Serve(ln) }()
	t.Cleanup(func() { _ = srv.Close() })
}

// runGraphCLI executes `iris run list <args...>` against the daemon on sock and
// returns stdout, stderr, and the exit code.
func runGraphCLI(t *testing.T, sock string, args ...string) (stdout, stderr string, code int) {
	t.Helper()
	var out, errb bytes.Buffer
	code = newApp(&out, &errb).run(append([]string{"--socket", sock, "run", "list"}, args...))
	return out.String(), errb.String(), code
}

// TestGraphSolidEdgesRunInputsOnly proves a solid stroke is drawn for a
// run_inputs edge and for nothing else: a consumption edge renders solid, while
// same-pipeline serial order renders dotted and an unrelated adjacency draws no
// stroke at all.
func TestGraphSolidEdgesRunInputsOnly(t *testing.T) {
	clearTargetEnv(t)
	t.Run("graph-solid-edges-run-inputs-only", func(t *testing.T) {
		sock := shortSocket(t)
		// Newest-first: 4 unrelated (no edge), 3 consumes 2 (solid edge), 2 and 1
		// are same-pipeline serial (dotted, never solid).
		rows := []runRow{
			{ID: "4", Pipeline: "z", State: "succeeded"},
			{ID: "3", Pipeline: "q", State: "succeeded", Inputs: []string{"2"}},
			{ID: "2", Pipeline: "p", State: "succeeded"},
			{ID: "1", Pipeline: "p", State: "succeeded"},
		}
		startRunsDaemon(t, sock, rows)

		out, errb, code := runGraphCLI(t, sock, "--graph")
		if code != exitOK {
			t.Fatalf("exit=%d stderr=%s", code, errb)
		}
		if got := strings.Count(out, "│"); got != 1 {
			t.Errorf("solid strokes = %d, want exactly 1 (only the run_inputs edge 3->2); got:\n%s", got, out)
		}
		if !strings.Contains(out, "┊") {
			t.Errorf("same-pipeline serial 2->1 must render as a dotted rail, never solid; got:\n%s", out)
		}
		// The unrelated adjacency 4->3 must carry no stroke: with exactly one solid
		// (the edge) and the dotted serial present, no stroke was drawn for the
		// unrelated pair.
	})
}

// TestGraphDottedSerialNeverAncestry proves same-pipeline serial order renders as
// a dotted rail visually distinct from lineage strokes and creates no edge in any
// payload: the --json read carries the runs as plain attributes with no drawing
// and no fabricated ancestry.
func TestGraphDottedSerialNeverAncestry(t *testing.T) {
	clearTargetEnv(t)
	t.Run("graph-dotted-serial-never-ancestry", func(t *testing.T) {
		sock := shortSocket(t)
		rows := []runRow{
			{ID: "2", Pipeline: "p", State: "succeeded"},
			{ID: "1", Pipeline: "p", State: "succeeded"},
		}
		startRunsDaemon(t, sock, rows)

		out, errb, code := runGraphCLI(t, sock, "--graph")
		if code != exitOK {
			t.Fatalf("exit=%d stderr=%s", code, errb)
		}
		if !strings.Contains(out, "┊") {
			t.Errorf("serial order must render dotted; got:\n%s", out)
		}
		if strings.Contains(out, "│") {
			t.Errorf("serial order must never render a solid (ancestry) stroke; got:\n%s", out)
		}

		jout, jerr, jcode := runGraphCLI(t, sock, "--graph", "--json")
		if jcode != exitOK {
			t.Fatalf("json exit=%d stderr=%s", jcode, jerr)
		}
		// No drawing and no fabricated edge/ancestry field in the payload.
		for _, forbidden := range []string{"┊", "│", "●", `"graph"`, `"edge"`, `"parent"`, `"ancestry"`} {
			if strings.Contains(jout, forbidden) {
				t.Errorf("serial --json payload carried %q; it must be plain attributes, never drawing or an edge: %s", forbidden, jout)
			}
		}
	})
}

// TestGraphReplayAnnotationNeverEdge proves replayed_from renders as a textual
// annotation on the run node and never as a graph edge: the annotation text is
// present, and the replay adds no stroke beyond the (unrelated) serial rail.
func TestGraphReplayAnnotationNeverEdge(t *testing.T) {
	t.Run("graph-replay-annotation-never-edge", func(t *testing.T) {
		rows := []runRow{
			{ID: "5", Pipeline: "p", State: "dead_lettered", ReplayedFrom: "2"},
			{ID: "4", Pipeline: "p", State: "succeeded"},
		}
		got := renderGraph(rows, false)
		if !strings.Contains(got, "(replayed_from=2)") {
			t.Errorf("replayed_from must render as a node annotation; got:\n%s", got)
		}
		// The only rail between the two runs is their same-pipeline serial dotted
		// rail; the replay creates no solid ancestry edge.
		if strings.Contains(got, "│") {
			t.Errorf("replayed_from must never render as an edge (it is replacement, not parenthood); got:\n%s", got)
		}
		if n := strings.Count(got, "┊"); n != 1 {
			t.Errorf("dotted serial rails = %d, want exactly 1 (the replay adds no edge); got:\n%s", n, got)
		}
	})
}

// TestGraphIDGapsVisible proves missing run ids leave a visible gap and the
// rendering never renumbers or fabricates continuity.
func TestGraphIDGapsVisible(t *testing.T) {
	t.Run("graph-id-gaps-visible", func(t *testing.T) {
		rows := []runRow{
			{ID: "10", Pipeline: "p", State: "succeeded"},
			{ID: "8", Pipeline: "p", State: "succeeded"}, // 9 is missing
		}
		got := renderGraph(rows, false)
		if !strings.Contains(got, "10") || !strings.Contains(got, "8") {
			t.Errorf("both surviving run ids must appear verbatim; got:\n%s", got)
		}
		if strings.Contains(got, "9") {
			t.Errorf("a pruned id must never be fabricated to fake continuity; got:\n%s", got)
		}
		if !strings.Contains(got, "\n\n") {
			t.Errorf("an id gap must stay visible as a blank line; got:\n%q", got)
		}
	})
}

// TestGraphPresentationalOnly proves --graph changes presentation only: the graph
// carries the same rows the flat read returns, and a --json read never carries
// drawing.
func TestGraphPresentationalOnly(t *testing.T) {
	t.Run("graph-presentational-only", func(t *testing.T) {
		rows := []runRow{
			{ID: "2", Pipeline: "load", State: "succeeded", Inputs: []string{"1"}},
			{ID: "1", Pipeline: "extract", State: "succeeded"},
		}
		graph := renderGraph(rows, false)
		// The graph presents drawing, and still every flat row (id, pipeline, state).
		if !strings.Contains(graph, "●") && !strings.Contains(graph, "│") {
			t.Errorf("the graph rendering must present rails/nodes; got:\n%s", graph)
		}
		for _, r := range rows {
			for _, field := range []string{r.ID, r.Pipeline, r.State} {
				if !strings.Contains(graph, field) {
					t.Errorf("graph dropped flat field %q; got:\n%s", field, graph)
				}
				if !strings.Contains(flatRunLine(r), field) {
					t.Errorf("flat line dropped field %q", field)
				}
			}
		}
		// The --json payload the CLI would emit carries the rows as attributes and
		// never any drawing character or edge field.
		payload, err := json.Marshal(dataEnvelope{Data: map[string]any{"runs": rows}})
		if err != nil {
			t.Fatalf("marshal payload: %v", err)
		}
		js := string(payload)
		for _, drawing := range []string{"●", "│", "┊", `"graph"`} {
			if strings.Contains(js, drawing) {
				t.Errorf("--json payload carried drawing %q: %s", drawing, js)
			}
		}
		if !strings.Contains(js, `"inputs":["1"]`) {
			t.Errorf("--json payload must carry the consumption as a plain attribute: %s", js)
		}
	})
}

// TestGraphRailCapFilterHint proves that past the rail cap the renderer refuses
// to weave and prints the --lane/--pipeline filter hint instead of degrading.
func TestGraphRailCapFilterHint(t *testing.T) {
	t.Run("graph-rail-cap-filter-hint", func(t *testing.T) {
		var rows []runRow
		for i := railCap + 5; i >= 1; i-- {
			rows = append(rows, runRow{
				ID: strconv.Itoa(i), Pipeline: "p", State: "succeeded",
			})
		}
		got := renderGraph(rows, false)
		if !strings.Contains(got, "--pipeline") || !strings.Contains(got, "--lane") {
			t.Errorf("past the rail cap the renderer must print the --lane/--pipeline filter hint; got:\n%s", got)
		}
		// Refuses to weave: no rails or nodes are drawn.
		for _, drawing := range []string{"●", "│", "┊"} {
			if strings.Contains(got, drawing) {
				t.Errorf("past the rail cap the renderer must refuse to weave, not degrade; drew %q:\n%s", drawing, got)
			}
		}
	})
}

// TestGraphASCIIGolden proves --ascii swaps the rail glyphs for git's own
// vocabulary and the --ascii render is pinned byte-for-byte by a golden file. It
// drives the real CLI end to end over a fake daemon socket.
func TestGraphASCIIGolden(t *testing.T) {
	clearTargetEnv(t)
	t.Run("graph-ascii-golden", func(t *testing.T) {
		sock := shortSocket(t)
		rows := []runRow{
			{ID: "7", Pipeline: "load_orders", State: "succeeded", ReplayedFrom: "3"},
			{ID: "6", Pipeline: "load_orders", State: "succeeded", Inputs: []string{"5"}},
			{ID: "5", Pipeline: "extract_orders", State: "succeeded"},
			{ID: "3", Pipeline: "extract_orders", State: "dead_lettered"}, // gap: 4 missing
		}
		startRunsDaemon(t, sock, rows)

		out, errb, code := runGraphCLI(t, sock, "--graph", "--ascii")
		if code != exitOK {
			t.Fatalf("exit=%d stderr=%s", code, errb)
		}
		golden.Assert(t, []byte(out), "testdata/run_list_ascii.txt")
	})
}
