package cli

import (
	"reflect"
	"testing"

	"github.com/MateusAMP2119/iris-lakehouse/internal/api"
)

// psvFixture is a two-lane snapshot: ingest carries a running and a queued
// run plus an idle member, reporting is fully idle, and one laneless pipeline
// (solo) stands as its own anonymous lane. Runs are newest first, as the wire
// orders them.
func psvFixture() psSnapshot {
	exit0, exit3 := 0, 3
	return psSnapshot{
		ps: api.PsPayload{
			Engine: api.PsEngine{Version: "dev", Role: "leader", PID: 42, Uptime: "2h13m",
				QueuedRuns: 1, RunningRuns: 1, Load: &api.PsLoad{CPUPercent: 3.2, RSSBytes: 126 << 20}},
			Runs: []api.PsRun{
				{ID: "14", Pipeline: "load_orders", Lane: "ingest", State: "running",
					Load: &api.PsLoad{CPUPercent: 51, RSSBytes: 24 << 20}},
				{ID: "12", Pipeline: "extract", Lane: "ingest", State: "queued"},
				{ID: "9", Pipeline: "load_orders", Lane: "ingest", State: "succeeded", ExitCode: &exit0},
				{ID: "6", Pipeline: "load_orders", Lane: "ingest", State: "dead_lettered", ExitCode: &exit3},
				{ID: "2", Pipeline: "solo", State: "succeeded", ExitCode: &exit0},
			},
		},
		pipelines: []api.PipelineListItem{
			{Name: "extract", Active: true, Lane: "ingest"},
			{Name: "hello_iris", Active: false, Lane: "ingest"},
			{Name: "load_orders", Active: true, Lane: "ingest"},
			{Name: "monthly", Active: false, Lane: "reporting"},
			{Name: "solo", Active: false},
		},
	}
}

// TestPsDerivations proves the pure table derivations: lane rollup with
// anonymous own-lanes, per-lane pipeline rows with idle members and latest
// states, and the runs filter.
func TestPsDerivations(t *testing.T) {
	t.Run("ps-derivations", func(t *testing.T) {
		s := psvFixture()

		t.Run("lanes roll up counts, loads, and anonymous own-lanes", func(t *testing.T) {
			lanes := deriveLanes(s)
			names := make([]string, len(lanes))
			for i, l := range lanes {
				names[i] = l.name
			}
			if want := []string{"ingest", "reporting", "solo"}; !reflect.DeepEqual(names, want) {
				t.Fatalf("lane names = %v, want %v", names, want)
			}
			ingest := lanes[0]
			if ingest.pipelines != 3 || ingest.queued != 1 || ingest.running != 1 {
				t.Errorf("ingest = %+v, want 3 pipelines, 1 queued, 1 running", ingest)
			}
			if ingest.load == nil || ingest.load.CPUPercent != 51 {
				t.Errorf("ingest load = %+v, want the running run's 51%%", ingest.load)
			}
			if rep := lanes[1]; rep.load != nil || rep.queued != 0 || rep.running != 0 {
				t.Errorf("idle reporting lane = %+v, want zero counts and nil load", rep)
			}
			if solo := lanes[2]; solo.pipelines != 1 {
				t.Errorf("laneless pipeline must stand as its own lane: %+v", solo)
			}
		})

		t.Run("a lane's pipelines include idle members with dash latest", func(t *testing.T) {
			rows := derivePipelines(s, "ingest")
			byName := map[string]psPipelineRow{}
			for _, r := range rows {
				byName[r.name] = r
			}
			if len(rows) != 3 {
				t.Fatalf("ingest pipelines = %+v, want extract, hello_iris, load_orders", rows)
			}
			if lo := byName["load_orders"]; lo.latest != "running" || lo.running != 1 || lo.load == nil {
				t.Errorf("load_orders = %+v, want latest running with live load", lo)
			}
			if ex := byName["extract"]; ex.latest != "queued" || ex.queued != 1 || ex.load != nil {
				t.Errorf("extract = %+v, want latest queued, one queued, nil load", ex)
			}
			if hi := byName["hello_iris"]; hi.latest != "-" {
				t.Errorf("idle hello_iris latest = %q, want dash", hi.latest)
			}
		})

		t.Run("run filter: queued+running default, whole history under all", func(t *testing.T) {
			def := deriveRuns(s, "load_orders", false)
			if len(def) != 1 || def[0].ID != "14" {
				t.Fatalf("default runs = %+v, want only running 14", def)
			}
			all := deriveRuns(s, "load_orders", true)
			ids := make([]string, len(all))
			for i, r := range all {
				ids[i] = r.ID
			}
			if want := []string{"14", "9", "6"}; !reflect.DeepEqual(ids, want) {
				t.Errorf("history runs = %v, want %v (newest first)", ids, want)
			}
		})
	})
}

// key is shorthand for a rune keypress.
func key(r rune) psKey { return psKey{kind: psKeyRune, r: r} }

// TestPsModelUpdate proves the dashboard state machine: the lanes tree, the
// pane focus cycle, table drilling, the logs target, selection stability
// across re-polls, the cancel confirm, and quit paths.
func TestPsModelUpdate(t *testing.T) {
	t.Run("ps-model-update", func(t *testing.T) {
		t.Run("opens on the first lane, unfolded, logs on its running run", func(t *testing.T) {
			m := newPsModel(psvFixture(), "local /tmp/iris.sock")
			if m.pane != psPaneLanes || m.selLane != "ingest" || m.selPipeline != "" {
				t.Fatalf("initial cursor: pane %d lane %q pipeline %q", m.pane, m.selLane, m.selPipeline)
			}
			if !m.expanded["ingest"] {
				t.Fatal("the initially selected lane must open unfolded")
			}
			if m.focus() != "14" {
				t.Fatalf("initial logs target = %q, want the newest running run 14", m.focus())
			}
		})

		t.Run("tree walk crosses pipeline rows and folds on enter", func(t *testing.T) {
			m := newPsModel(psvFixture(), "")
			m.update(key('j')) // into ingest's members: extract first (sorted)
			if m.selLane != "ingest" || m.selPipeline != "extract" {
				t.Fatalf("after j: lane %q pipeline %q", m.selLane, m.selPipeline)
			}
			m.update(key('j'))
			m.update(key('j'))
			if m.selPipeline != "load_orders" {
				t.Fatalf("after jjj: pipeline %q, want load_orders", m.selPipeline)
			}
			m.update(key('j')) // past the lane's members: the next lane row
			if m.selLane != "reporting" || m.selPipeline != "" {
				t.Fatalf("after jjjj: lane %q pipeline %q, want reporting lane row", m.selLane, m.selPipeline)
			}
			m.update(psKey{kind: psKeyLeft})
			m.update(key('k')) // back onto ingest's last member
			if m.selPipeline != "load_orders" {
				t.Fatalf("k after collapse of reporting: pipeline %q", m.selPipeline)
			}
			m.update(psKey{kind: psKeyLeft}) // pipeline row climbs to its lane
			if m.selLane != "ingest" || m.selPipeline != "" {
				t.Fatalf("left on a pipeline row: lane %q pipeline %q", m.selLane, m.selPipeline)
			}
			m.update(psKey{kind: psKeyEnter}) // lane row folds
			if m.expanded["ingest"] {
				t.Fatal("enter on an unfolded lane must fold it")
			}
		})

		t.Run("tab cycles panes", func(t *testing.T) {
			m := newPsModel(psvFixture(), "")
			for _, want := range []psPane{psPaneTable, psPaneLogs, psPaneLanes} {
				m.update(psKey{kind: psKeyTab})
				if m.pane != want {
					t.Fatalf("pane = %d, want %d", m.pane, want)
				}
			}
		})

		t.Run("table drills a pipeline, run enter pins the logs target", func(t *testing.T) {
			m := newPsModel(psvFixture(), "")
			m.update(psKey{kind: psKeyTab}) // table pane, pipelines mode
			m.update(key('j'))              // extract -> hello_iris
			m.update(psKey{kind: psKeyEnter})
			if m.selPipeline != "hello_iris" {
				t.Fatalf("drill: selPipeline %q, want hello_iris", m.selPipeline)
			}
			m.update(psKey{kind: psKeyLeft}) // back to the pipelines table
			if m.selPipeline != "" {
				t.Fatalf("left in a runs table must climb back: %q", m.selPipeline)
			}
			m.tblPipeline = "load_orders"
			m.update(psKey{kind: psKeyEnter})
			m.update(key('a')) // whole history in
			if !m.showAll {
				t.Fatal("a did not widen the runs table")
			}
			m.update(key('j')) // 14 -> 9
			m.update(psKey{kind: psKeyEnter})
			if m.pinnedRun != "9" || m.focus() != "9" {
				t.Fatalf("enter on a run row must pin it: pinned %q focus %q", m.pinnedRun, m.focus())
			}
		})

		t.Run("a is inert outside the runs table", func(t *testing.T) {
			m := newPsModel(psvFixture(), "")
			m.update(key('a')) // lanes pane
			if m.showAll {
				t.Fatal("a toggled history from the lanes pane")
			}
			m.update(psKey{kind: psKeyTab}) // table pane, pipelines mode
			m.update(key('a'))
			if m.showAll {
				t.Fatal("a toggled history in the pipelines table")
			}
		})

		t.Run("logs target follows the selection and survives pin loss", func(t *testing.T) {
			m := newPsModel(psvFixture(), "")
			m.update(key('j')) // extract row: no running run, newest is queued 12
			if m.focus() != "12" {
				t.Fatalf("extract target = %q, want its only run 12", m.focus())
			}
			m.pinnedRun = "9"
			if m.focus() != "9" {
				t.Fatalf("pinned target = %q, want 9", m.focus())
			}
			s := psvFixture() // run 9 pruned from the history
			s.ps.Runs = append(s.ps.Runs[:2:2], s.ps.Runs[3:]...)
			m.absorb(s)
			if m.focus() != "12" {
				t.Fatalf("target after pin loss = %q, want the selection's 12", m.focus())
			}
		})

		t.Run("selection survives a re-poll reorder and clamps when gone", func(t *testing.T) {
			m := newPsModel(psvFixture(), "")
			m.update(psKey{kind: psKeyEnter}) // fold ingest
			m.update(key('j'))                // reporting
			if m.selLane != "reporting" {
				t.Fatalf("selLane = %q", m.selLane)
			}
			m.absorb(psvFixture()) // same entities: selection sticks
			if m.selLane != "reporting" {
				t.Errorf("selection lost across re-poll: %q", m.selLane)
			}
			s2 := psvFixture() // reporting vanishes: clamp to first
			s2.pipelines = s2.pipelines[:3]
			m.absorb(s2)
			if m.selLane != "ingest" {
				t.Errorf("vanished selection clamps to first lane, got %q", m.selLane)
			}
		})

		t.Run("cancel confirm arms on a running target and disarms on anything but y", func(t *testing.T) {
			m := newPsModel(psvFixture(), "")
			m.pane = psPaneLogs // target is running 14
			m.update(key('c'))
			if !m.confirmCancel {
				t.Fatal("c on a running target did not arm the confirm")
			}
			if got := m.update(key('n')); got != "" || m.confirmCancel {
				t.Fatalf("n must disarm without cancelling, got %q", got)
			}
			m.update(key('c'))
			if got := m.update(key('y')); got != "14" {
				t.Fatalf("y must confirm the cancel, got %q", got)
			}
			m.pinnedRun = "9" // terminal target: c never arms
			m.update(key('c'))
			if m.confirmCancel {
				t.Fatal("c armed a cancel on a terminal run")
			}
		})

		t.Run("follow toggles and scroll clamps in the logs pane", func(t *testing.T) {
			m := newPsModel(psvFixture(), "")
			m.pane = psPaneLogs
			m.snap.logs, m.snap.logsRun = []string{"a", "b", "c", "d"}, "14"
			m.update(key('f'))
			if m.follow {
				t.Fatal("f did not stop following")
			}
			for range 10 {
				m.update(key('k'))
			}
			if m.scroll != 3 {
				t.Errorf("scroll = %d, want clamped at len-1 so the top line stays visible", m.scroll)
			}
			m.update(key('j'))
			if m.scroll != 2 {
				t.Errorf("scroll after one down = %d, want 2", m.scroll)
			}
			m.update(key('f'))
			if !m.follow || m.scroll != 0 {
				t.Errorf("f must resume following at the tail: follow %v scroll %d", m.follow, m.scroll)
			}
		})

		t.Run("rings grow one sample per absorb, absence marked", func(t *testing.T) {
			m := newPsModel(psvFixture(), "")
			m.absorb(psvFixture())
			eng := m.rings[""]
			if len(eng.cpu) != 2 || eng.cpu[1] != 3.2 {
				t.Fatalf("engine ring = %+v, want two samples ending 3.2", eng.cpu)
			}
			if ing := m.rings["l:ingest"]; len(ing.cpu) != 2 || ing.cpu[1] != 51 {
				t.Fatalf("ingest ring = %+v, want the running run's 51", ing.cpu)
			}
			if rep := m.rings["l:reporting"]; rep.cpu[1] != psNoSample {
				t.Fatalf("idle lane ring = %+v, want psNoSample", rep.cpu)
			}
			if lo := m.rings["p:load_orders"]; lo.mem[1] != 24<<20 || lo.memPeak() != 24<<20 {
				t.Fatalf("pipeline ring mem = %+v", lo.mem)
			}
		})

		t.Run("a sample tick gates the ring push", func(t *testing.T) {
			first := psvFixture()
			first.ps.SampleTick = 3
			m := newPsModel(first, "")
			if eng := m.rings[""]; len(eng.cpu) != 1 {
				t.Fatalf("engine ring after open = %+v, want one sample", eng.cpu)
			}
			same := psvFixture() // the collector has not ticked: no push
			same.ps.SampleTick = 3
			m.absorb(same)
			if eng := m.rings[""]; len(eng.cpu) != 1 {
				t.Fatalf("engine ring after a same-tick poll = %+v, want still one sample", eng.cpu)
			}
			next := psvFixture() // tick 5: the missed tick 4 fills absent
			next.ps.SampleTick = 5
			m.absorb(next)
			eng := m.rings[""]
			if len(eng.cpu) != 3 || eng.cpu[1] != psNoSample || eng.cpu[2] != 3.2 {
				t.Fatalf("engine ring after a tick jump = %+v, want [3.2, no-sample, 3.2]", eng.cpu)
			}
		})

		t.Run("a collector restart resets the tick gate", func(t *testing.T) {
			first := psvFixture()
			first.ps.SampleTick = 40
			m := newPsModel(first, "")
			back := psvFixture() // a restarted daemon answers with a small tick
			back.ps.SampleTick = 2
			m.absorb(back)
			eng := m.rings[""]
			if len(eng.cpu) != 2 || eng.cpu[1] != 3.2 {
				t.Fatalf("engine ring after a tick regression = %+v, want the restarted collector's sample appended", eng.cpu)
			}
			if m.lastTick != 2 {
				t.Errorf("lastTick = %d, want the restarted collector's 2", m.lastTick)
			}
		})

		t.Run("a history payload re-seeds the rings", func(t *testing.T) {
			s := psvFixture()
			s.ps.SampleTick = 40
			s.ps.History = &api.PsHistory{
				FineIntervalSeconds: 2, CoarseIntervalSeconds: 60,
				Series: []api.PsSeries{
					{Key: "engine", CPU: []float64{1, 2, 3}, RSS: []int64{10, 20, 30},
						CoarseCPU: []float64{9}, CoarseRSS: []int64{90}},
					{Key: "lane:ingest", CPU: []float64{51}, RSS: []int64{24 << 20}},
					{Key: "pipeline:load_orders", CPU: []float64{51}, RSS: []int64{24 << 20}},
					{Key: "wat:unknown", CPU: []float64{7}, RSS: []int64{7}},
				},
			}
			m := newPsModel(s, "")
			if eng := m.rings[""]; len(eng.cpu) != 3 || eng.cpu[2] != 3 {
				t.Fatalf("engine ring = %+v, want the wire fine series", eng.cpu)
			}
			if c := m.coarse[""]; c == nil || len(c.cpu) != 1 || c.cpu[0] != 9 || c.mem[0] != 90 {
				t.Fatalf("engine coarse ring = %+v, want the wire coarse series", c)
			}
			if ing := m.rings["l:ingest"]; ing == nil || ing.cpu[0] != 51 {
				t.Fatalf("lane ring = %+v, want the wire series under the l: key", ing)
			}
			if lo := m.rings["p:load_orders"]; lo == nil || lo.mem[0] != 24<<20 {
				t.Fatalf("pipeline ring = %+v, want the wire series under the p: key", lo)
			}
			if len(m.rings) != 3 {
				t.Errorf("rings = %d entries, want 3 (the unknown wire key is skipped)", len(m.rings))
			}
			if m.lastTick != 40 {
				t.Errorf("lastTick = %d, want the payload's 40", m.lastTick)
			}
			// The next tick appends to the re-seeded ring, no re-seed needed.
			live := psvFixture()
			live.ps.SampleTick = 41
			m.absorb(live)
			if eng := m.rings[""]; len(eng.cpu) != 4 || eng.cpu[3] != 3.2 {
				t.Fatalf("engine ring after the next tick = %+v, want the live sample appended", eng.cpu)
			}
		})

		t.Run("h toggles the history view anywhere", func(t *testing.T) {
			m := newPsModel(psvFixture(), "")
			m.update(key('h'))
			if !m.histView {
				t.Fatal("h did not toggle the history view on")
			}
			m.update(psKey{kind: psKeyTab})
			m.update(key('h'))
			if m.histView {
				t.Fatal("h did not toggle the history view back off")
			}
		})

		t.Run("q and ctrl-c quit", func(t *testing.T) {
			m := newPsModel(psvFixture(), "")
			m.update(key('q'))
			if !m.quit {
				t.Fatal("q did not quit")
			}
			m = newPsModel(psvFixture(), "")
			m.update(psKey{kind: psKeyCtrlC})
			if !m.quit {
				t.Fatal("ctrl-c did not quit")
			}
		})
	})
}

// TestPsSearch proves the fuzzy matcher and the overlay's state: scoring
// order, per-keystroke narrowing, kind-aware jumps, and Esc closing.
func TestPsSearch(t *testing.T) {
	t.Run("ps-search", func(t *testing.T) {
		t.Run("fuzzy scoring", func(t *testing.T) {
			cases := []struct {
				query, cand string
				match       bool
			}{
				{"ord", "load_orders", true},
				{"ORD", "load_orders", true},
				{"lo", "load_orders", true},
				{"xyz", "load_orders", false},
				{"", "anything", true},
				{"14", "14 · running", true},
			}
			for _, tc := range cases {
				if _, ok := fuzzyScore(tc.query, tc.cand); ok != tc.match {
					t.Errorf("fuzzyScore(%q, %q) match = %v, want %v", tc.query, tc.cand, ok, tc.match)
				}
			}
			word, _ := fuzzyScore("or", "load_orders") // word-start o
			mid, _ := fuzzyScore("or", "sensor")       // mid-word
			if word <= mid {
				t.Errorf("word-start match must outscore mid-word: %d <= %d", word, mid)
			}
		})

		t.Run("narrowing, jump, and esc", func(t *testing.T) {
			m := newPsModel(psvFixture(), "")
			m.update(key('/'))
			if m.search == nil {
				t.Fatal("/ did not open search")
			}
			// Empty query holds every entity: 3 lanes + 5 pipelines + 5 runs.
			if got := len(m.search.hits); got != 13 {
				t.Fatalf("empty-query hits = %d, want 13", got)
			}
			for _, r := range "ord" {
				m.update(key(r))
			}
			for _, h := range m.search.hits {
				if _, ok := fuzzyScore("ord", h.label); !ok {
					t.Errorf("hit %q does not match the query", h.label)
				}
			}
			if m.search.hits[0].label != "load_orders" {
				t.Fatalf("best hit = %+v, want the load_orders pipeline", m.search.hits[0])
			}
			m.update(psKey{kind: psKeyEnter})
			if m.search != nil || m.pane != psPaneTable || m.selPipeline != "load_orders" || m.selLane != "ingest" {
				t.Fatalf("enter on a pipeline hit must select it and focus the table: %+v", m)
			}
		})

		t.Run("run hit pins the logs target", func(t *testing.T) {
			m := newPsModel(psvFixture(), "")
			m.update(key('/'))
			m.update(key('1'))
			m.update(key('4'))
			m.update(psKey{kind: psKeyEnter})
			if m.pane != psPaneLogs || m.pinnedRun != "14" || m.focus() != "14" {
				t.Fatalf("run jump landed wrong: pane %d pinned %q focus %q", m.pane, m.pinnedRun, m.focus())
			}
			if m.selLane != "ingest" || m.selPipeline != "load_orders" {
				t.Errorf("run jump selection: lane %q pipeline %q", m.selLane, m.selPipeline)
			}
		})

		t.Run("esc closes and only closes", func(t *testing.T) {
			m := newPsModel(psvFixture(), "")
			m.update(key('/'))
			m.update(psKey{kind: psKeyEsc})
			if m.search != nil || m.quit {
				t.Fatal("esc must close the overlay and nothing else")
			}
			m.update(psKey{kind: psKeyEsc}) // outside the overlay: inert
			if m.quit || m.pane != psPaneLanes {
				t.Fatal("esc outside the overlay must be inert")
			}
		})

		t.Run("j and k are literal query characters while open", func(t *testing.T) {
			m := newPsModel(psvFixture(), "")
			m.update(key('/'))
			m.update(key('j'))
			m.update(key('k'))
			if got := string(m.search.query); got != "jk" {
				t.Fatalf("query = %q, want jk", got)
			}
			if m.selLane != "ingest" || m.selPipeline != "" {
				t.Error("j/k moved the backdrop selection while the overlay was open")
			}
		})
	})
}
