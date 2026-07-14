package cli

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"

	"github.com/spf13/cobra"
)

// This file is the CLI side of `iris run list [--graph] [--ascii]` and its CLI
// output contract. The flat read prints run history a row at a time; the
// --graph rendering draws the lineage rails, presentation only: the same rows the
// flat read returns, drawn as rails, with --json never carrying drawing.
//
// The rendering contract is honesty made testable: a solid stroke is a run_inputs
// edge and nothing else, a dotted rail is same-pipeline serial order (sequence,
// never ancestry), a replay renders as an annotation never an edge (replayed_from
// is replacement, not parenthood), and run-id gaps stay visible (ids never
// renumber). Past a rail cap the weave refuses and prints the --lane/--pipeline
// filter hint; --ascii swaps to git's own glyph vocabulary and is the render the
// golden files pin. Wiring and lineage are two graphs that never share a
// rendering: this file draws lineage only.

// runRow models a run record served by GET /runs?include=inputs. It carries only
// the attributes the lineage rail renderer needs: Inputs are the consumed
// upstream run ids (each a solid edge), and ReplayedFrom is the replaced run
// (annotation only, never an edge). The wire carries these as plain attributes,
// the lineage payload an external renderer draws.
type runRow struct {
	ID           string   `json:"id"`
	Pipeline     string   `json:"pipeline"`
	State        string   `json:"state"`
	Inputs       []string `json:"inputs,omitempty"`
	ReplayedFrom string   `json:"replayed_from,omitempty"`
}

// railCap bounds how many runs the weave renders before it refuses: past it the
// rail view degrades into an unreadable tangle, so the renderer prints the
// filter hint instead of weaving: past a rail cap the weave refuses.
const railCap = 10

// railGlyphs is one rendering's glyph vocabulary: the node marker, the solid
// stroke (a run_inputs edge), and the dotted rail (same-pipeline serial order).
// The unicode set is the default; --ascii swaps to git's own vocabulary, the set
// the golden files pin.
type railGlyphs struct {
	node   string
	solid  string
	dotted string
}

var (
	unicodeGlyphs = railGlyphs{node: "●", solid: "│", dotted: "┊"}
	asciiGlyphs   = railGlyphs{node: "*", solid: "|", dotted: ":"}
)

// runList is the handler for `iris run list [--graph] [--ascii] [--after R]
// [--before R]`. It GETs /runs with include=inputs (the consumption edges the
// rail view needs) plus any paging cursors, then renders flat or graph per the
// flags. --graph is presentation only: under --json the same rows are returned
// with no drawing. Transport failure is no-daemon (exit 3) with start guidance;
// any other failure is operation-failed (exit 4).
func (a *app) runList() runE {
	return func(cmd *cobra.Command, _ []string) error {
		graph, _ := cmd.Flags().GetBool("graph")
		ascii, _ := cmd.Flags().GetBool("ascii")
		after, _ := cmd.Flags().GetString("after")
		before, _ := cmd.Flags().GetString("before")

		settings := a.resolveTarget(cmd)
		client, base, overTCP := a.daemonHTTPClient(settings)

		q := url.Values{}
		q.Set("include", "inputs")
		if after != "" {
			q.Set("after", after)
		}
		if before != "" {
			q.Set("before", before)
		}
		reqURL := base + "/runs?" + q.Encode()

		hreq, err := http.NewRequestWithContext(cmd.Context(), http.MethodGet, reqURL, nil)
		if err != nil {
			return &fault{code: exitOpFailed, codeStr: "request", message: fmt.Sprintf("run list: build request: %v", err)}
		}
		if overTCP && settings.Token != "" {
			hreq.Header.Set("Authorization", "Bearer "+settings.Token)
		}

		resp, err := client.Do(hreq)
		if err != nil {
			a.logger.Debug("no iris daemon reachable", "op", "run list", "socket", settings.Socket, "host", settings.Host, "err", err)
			return a.noDaemonFault()
		}
		defer func() {
			_, _ = io.Copy(io.Discard, resp.Body)
			_ = resp.Body.Close()
		}()

		if resp.StatusCode != http.StatusOK {
			return a.controlFailure(resp, "run list")
		}

		var env struct {
			Data struct {
				Runs []runRow `json:"runs"`
			} `json:"data"`
		}
		if err := json.NewDecoder(resp.Body).Decode(&env); err != nil {
			return &fault{code: exitOpFailed, codeStr: "decode", message: fmt.Sprintf("run list: decode daemon response: %v", err)}
		}
		rows := env.Data.Runs

		if jsonMode, _ := cmd.Flags().GetBool("json"); jsonMode {
			// Presentation only: the --json read carries the rows as plain
			// attributes, never any drawing, whether or not --graph was set.
			return json.NewEncoder(a.out).Encode(dataEnvelope{Data: map[string]any{"runs": rows}})
		}
		if graph {
			_, err := io.WriteString(a.out, renderGraph(rows, ascii))
			return err
		}
		for _, r := range rows {
			if _, err := fmt.Fprintln(a.out, flatRunLine(r)); err != nil {
				return err
			}
		}
		return nil
	}
}

// flatRunLine is the flat read's one-row rendering: id, pipeline, state. The
// graph rendering presents exactly these fields, drawn as rails, so --graph is
// presentation only.
func flatRunLine(r runRow) string {
	return r.ID + " " + r.Pipeline + " " + r.State
}

// renderGraph draws the lineage rail view of rows (newest first). It is pure: it
// draws a node line per run and a connector line between adjacent runs, where the
// connector is a solid stroke for a run_inputs edge, a dotted rail for
// same-pipeline serial order, a blank line for a visible run-id gap, and an empty
// rail otherwise (an unrelated adjacency draws no stroke, because a solid stroke
// is a run_inputs edge and nothing else). replayed_from rides the node line as an
// annotation, never an edge. Past railCap the weave refuses and returns the
// filter hint. --ascii selects git's glyph vocabulary.
func renderGraph(rows []runRow, ascii bool) string {
	if len(rows) > railCap {
		return fmt.Sprintf(
			"graph too dense to weave (%d runs, rail cap %d); narrow it with --pipeline or --lane\n",
			len(rows), railCap)
	}
	g := unicodeGlyphs
	if ascii {
		g = asciiGlyphs
	}
	var b strings.Builder
	for i, r := range rows {
		b.WriteString(nodeLine(g, r))
		b.WriteByte('\n')
		if i == len(rows)-1 {
			break
		}
		b.WriteString(connectorLine(g, r, rows[i+1]))
		b.WriteByte('\n')
	}
	return b.String()
}

// nodeLine renders one run's node: the node glyph, its id, pipeline, and state,
// with replayed_from appended as a plain-text annotation (never an edge).
func nodeLine(g railGlyphs, r runRow) string {
	line := g.node + " " + r.ID + "  " + r.Pipeline + "  " + r.State
	if r.ReplayedFrom != "" {
		line += " (replayed_from=" + r.ReplayedFrom + ")"
	}
	return line
}

// connectorLine renders the rail between an upper (newer) and lower (older)
// adjacent run. A run_inputs edge between them is the only thing drawn solid; a
// same-pipeline serial adjacency is dotted; a non-consecutive id pair leaves a
// visible (empty) gap; any other adjacency is an empty rail with no stroke.
func connectorLine(g railGlyphs, upper, lower runRow) string {
	switch {
	case inputsEdge(upper, lower):
		return g.solid
	case idGap(upper, lower):
		return "" // visible gap: the runs are not adjacent in history
	case upper.Pipeline == lower.Pipeline:
		return g.dotted
	default:
		return " " // unrelated adjacency: an empty rail, never a stroke
	}
}

// inputsEdge reports whether a run_inputs consumption edge joins the two runs, in
// either direction: one consumed the other. It is the sole source of a solid
// stroke.
func inputsEdge(a, b runRow) bool {
	return containsID(a.Inputs, b.ID) || containsID(b.Inputs, a.ID)
}

// idGap reports whether two adjacent listed runs have non-consecutive numeric
// ids, so an id was pruned between them and the gap must stay visible. A
// non-numeric id (never a real run id) is treated as no gap.
func idGap(a, b runRow) bool {
	ai, aerr := strconv.Atoi(a.ID)
	bi, berr := strconv.Atoi(b.ID)
	if aerr != nil || berr != nil {
		return false
	}
	d := ai - bi
	if d < 0 {
		d = -d
	}
	return d != 1
}

// containsID reports whether id is in ids.
func containsID(ids []string, id string) bool {
	for _, x := range ids {
		if x == id {
			return true
		}
	}
	return false
}
