package cli

import (
	"strings"
	"testing"

	"github.com/MateusAMP2119/iris-lakehouse/internal/golden"
)

// framePlain renders a model at a fixed geometry and joins the plain rune
// grid into the golden surface (styles asserted separately -- geometry never
// depends on them).
func framePlain(m *psModel, w, h int) string {
	b := renderPsFrame(m, w, h, false)
	return strings.Join(b.plainLines(), "\n") + "\n"
}

// withLogs attaches a log tail for the model's current target, as a poll
// carrying the tail would.
func withLogs(m *psModel, lines ...string) {
	m.snap.logs, m.snap.logsRun = lines, m.logsTarget()
}

// TestPsFrameGoldens pins the dashboard byte-for-byte at each width tier:
// all four panes, detail shed, logs shed, rail shed, plus the runs table, the
// search overlay, and the too-small degradation.
func TestPsFrameGoldens(t *testing.T) {
	t.Run("ps-frame-goldens", func(t *testing.T) {
		target := "remote 10.0.0.5:7433"
		logLines := []string{
			"fetching s3://orders/2026-07-15.csv",
			"1204 rows parsed",
			"upserting batch 3/12 into demo.orders",
			"upserting batch 4/12 into demo.orders",
		}

		t.Run("four panes 150x40", func(t *testing.T) {
			m := newPsModel(psvFixture(), target)
			withLogs(m, logLines...)
			golden.Assert(t, []byte(framePlain(m, 150, 40)), "testdata/psv_dashboard_150x40.txt")
		})

		t.Run("no detail box 100x30", func(t *testing.T) {
			m := newPsModel(psvFixture(), target)
			withLogs(m, logLines...)
			golden.Assert(t, []byte(framePlain(m, 100, 30)), "testdata/psv_dashboard_100x30.txt")
		})

		t.Run("no logs pane 80x24", func(t *testing.T) {
			m := newPsModel(psvFixture(), target)
			golden.Assert(t, []byte(framePlain(m, 80, 24)), "testdata/psv_dashboard_80x24.txt")
		})

		t.Run("no rail 60x20", func(t *testing.T) {
			m := newPsModel(psvFixture(), target)
			golden.Assert(t, []byte(framePlain(m, 60, 20)), "testdata/psv_dashboard_60x20.txt")
		})

		t.Run("runs table with history 150x40", func(t *testing.T) {
			m := newPsModel(psvFixture(), target)
			m.update(psKey{kind: psKeyTab}) // table pane
			m.tblPipeline = "load_orders"
			m.update(psKey{kind: psKeyEnter}) // drill into its runs
			m.update(key('a'))                // whole history
			withLogs(m, logLines...)
			golden.Assert(t, []byte(framePlain(m, 150, 40)), "testdata/psv_runs_150x40.txt")
		})

		t.Run("search overlay 100x30", func(t *testing.T) {
			m := newPsModel(psvFixture(), target)
			m.update(key('/'))
			for _, r := range "ord" {
				m.update(key(r))
			}
			golden.Assert(t, []byte(framePlain(m, 100, 30)), "testdata/psv_search_100x30.txt")
		})

		t.Run("too small degrades to one line", func(t *testing.T) {
			m := newPsModel(psvFixture(), target)
			got := framePlain(m, 30, 5)
			if !strings.HasPrefix(got, "iris ps: terminal too small") {
				t.Fatalf("tiny frame = %q, want the advisory line", strings.SplitN(got, "\n", 2)[0])
			}
		})
	})
}

// TestPsFrameStyling proves the SGR layer: state colors land on their cells,
// the focused pane's border is cyan, the selection inverts (or gains the
// marker when colorless), heat cells quantize into the ramp, and the emission
// carries zero escape bytes beyond cursor addressing when the painter is off.
func TestPsFrameStyling(t *testing.T) {
	t.Run("ps-frame-styling", func(t *testing.T) {
		t.Run("state cells and focus border carry their palette color", func(t *testing.T) {
			m := newPsModel(psvFixture(), "")
			m.update(key('j')) // off the lane row...
			m.update(key('j')) // ...and off extract, so inversion hides no state dot
			b := renderPsFrame(m, 150, 40, false)
			out := string(b.render(painter{enabled: true}))
			for _, want := range []string{ansiCyan + "●", ansiYellow + "●", ansiGreen + "LEADER", ansiCyan + "╭"} {
				if !strings.Contains(out, want) {
					t.Errorf("frame carries no %q-styled cell", want)
				}
			}
			if !strings.Contains(out, ansiInverse) {
				t.Error("selected row is not inverted")
			}
		})

		t.Run("heat strip quantizes the ramp", func(t *testing.T) {
			b := newScreenBuf(10, 1)
			b.renderHeatStrip(0, 0, 10, []float64{psNoSample, 3, 30, 60, 90})
			line := b.plainLines()[0]
			if !strings.HasSuffix(line, "░▒▓█") {
				t.Fatalf("heat strip = %q, want tone per quartile, absence blank", line)
			}
			if got := b.cells[9].sgr; got != ansiRed {
				t.Errorf("hot cell sgr = %q, want red", got)
			}
			if got := b.cells[6].sgr; got != ansiGreen {
				t.Errorf("cool cell sgr = %q, want green", got)
			}
		})

		t.Run("fitSamples compresses by per-cell maximum", func(t *testing.T) {
			short := []float64{1, 2, 3}
			if got := fitSamples(short, 5); len(got) != 3 {
				t.Fatalf("fitSamples must pass a narrow history through, got %v", got)
			}
			wide := []float64{psNoSample, psNoSample, 10, 90, 5, 5, psNoSample, 20}
			got := fitSamples(wide, 4)
			if len(got) != 4 {
				t.Fatalf("fitSamples width = %d, want 4", len(got))
			}
			// Cells of two: [absent absent] [10 90] [5 5] [absent 20] -- an
			// all-absent share stays absent, a spike survives its share.
			if got[0] != psNoSample || got[1] != 90 || got[2] != 5 || got[3] != 20 {
				t.Errorf("fitSamples = %v, want [no-sample, 90, 5, 20]", got)
			}
		})

		t.Run("the history toggle swaps the strips to the coarse rings", func(t *testing.T) {
			m := newPsModel(psvFixture(), "")
			m.coarse[""] = &psRing{cpu: []float64{90, 90, 90}, mem: []int64{1, 1, 1}}
			live := m.stripCPU("", 10)
			m.histView = true
			hist := m.stripCPU("", 10)
			if len(hist) != 3 || hist[0] != 90 {
				t.Fatalf("history strip = %v, want the coarse ring", hist)
			}
			if len(live) == len(hist) && live[0] == hist[0] {
				t.Error("live and history strips read the same ring")
			}
		})

		t.Run("disabled painter emits no SGR", func(t *testing.T) {
			m := newPsModel(psvFixture(), "")
			b := renderPsFrame(m, 150, 40, true)
			out := string(b.render(painter{}))
			for _, sgr := range []string{ansiInverse, ansiCyan, ansiGreen, ansiYellow, ansiRed, ansiOrange, ansiDim, ansiReset} {
				if strings.Contains(out, sgr) {
					t.Errorf("colorless frame carries SGR %q", sgr)
				}
			}
			if !strings.Contains(strings.Join(b.plainLines(), "\n"), ">") {
				t.Error("colorless frame carries no selection marker")
			}
		})

		t.Run("no line exceeds the frame width", func(t *testing.T) {
			m := newPsModel(psvFixture(), "remote very-long-hostname.example.internal:7433")
			m.snap.pipelines[0].Name = strings.Repeat("very_long_pipeline_name_", 5)
			m.absorb(m.snap)
			for _, geo := range []struct{ w, h int }{{150, 40}, {100, 30}, {80, 24}, {60, 20}, {45, 10}} {
				b := renderPsFrame(m, geo.w, geo.h, false)
				for i, line := range b.plainLines() {
					if n := len([]rune(line)); n > geo.w {
						t.Errorf("%dx%d line %d is %d runes wide: %q", geo.w, geo.h, i, n, line)
					}
				}
			}
		})

		t.Run("logs pane titles the watched run and pauses read honestly", func(t *testing.T) {
			m := newPsModel(psvFixture(), "")
			withLogs(m, "one", "two")
			lines := renderPsFrame(m, 150, 40, false).plainLines()
			joined := strings.Join(lines, "\n")
			if !strings.Contains(joined, "LOGS · load_orders/14 · running · following") {
				t.Errorf("logs title missing, frame:\n%s", joined)
			}
			m.pane = psPaneLogs
			m.update(key('f'))
			joined = strings.Join(renderPsFrame(m, 150, 40, false).plainLines(), "\n")
			if !strings.Contains(joined, "paused") {
				t.Error("paused tail not titled")
			}
		})

		t.Run("header names the engine and its role", func(t *testing.T) {
			m := newPsModel(psvFixture(), "")
			top := renderPsFrame(m, 150, 40, false).plainLines()[0]
			for _, want := range []string{"ENGINE dev", "LEADER", "pid 42", "up 2h13m", "1 running", "1 queued"} {
				if !strings.Contains(top, want) {
					t.Errorf("header %q missing %q", top, want)
				}
			}
		})
	})
}
