package tui

import (
	"strings"
	"testing"

	"github.com/MateusAMP2119/iris-lakehouse/internal/api"
)

// typeLine feeds a string into the model one rune at a time.
func typeLine(m *psModel, s string) {
	for _, r := range s {
		m.update(key(r))
	}
}

// TestPsCommandMode proves the COMMANDS palette: open/close, dispatch through
// the model, inline errors, tab completion, list navigation, and help browse.
func TestPsCommandMode(t *testing.T) {
	t.Run("ps-command-mode", func(t *testing.T) {
		t.Run("':' opens the palette, esc closes it, the view stands", func(t *testing.T) {
			m := newPsModel(psvFixture(), "")
			m.update(key(':'))
			if m.command == nil {
				t.Fatal("':' did not open the command palette")
			}
			typeLine(m, "cat")
			m.update(psKey{kind: psKeyEsc})
			if m.command != nil || m.quit {
				t.Fatal("esc must close the palette without quitting")
			}
		})

		t.Run("'?' opens help browse focused on the help entry", func(t *testing.T) {
			m := newPsModel(psvFixture(), "")
			m.update(key('?'))
			if m.command == nil || !m.command.browse {
				t.Fatal("'?' must open the palette in browse mode")
			}
			spec, ok := m.command.selected()
			if !ok || spec.name != "help" {
				t.Fatalf("selection = %+v, want help", spec)
			}
		})

		t.Run("backspace edits, and closes on an empty prompt", func(t *testing.T) {
			m := newPsModel(psvFixture(), "")
			m.update(key(':'))
			typeLine(m, "q")
			m.update(psKey{kind: psKeyBackspace})
			if m.command == nil || len(m.command.input) != 0 {
				t.Fatal("backspace must edit the input first")
			}
			m.update(psKey{kind: psKeyBackspace})
			if m.command != nil {
				t.Fatal("backspace on an empty prompt must close it")
			}
		})

		t.Run(":q quits like q", func(t *testing.T) {
			m := newPsModel(psvFixture(), "")
			m.update(key(':'))
			typeLine(m, "q")
			m.update(psKey{kind: psKeyEnter})
			if !m.quit {
				t.Fatal(":q did not quit")
			}
		})

		t.Run(":logs <run> pins the logs pane on the run", func(t *testing.T) {
			m := newPsModel(psvFixture(), "")
			m.update(key(':'))
			typeLine(m, "logs 6")
			m.update(psKey{kind: psKeyEnter})
			if m.command != nil {
				t.Fatalf("successful :logs must close the palette (err %q)", m.command.err)
			}
			if m.pinnedRun != "6" || m.pane != psPaneLogs || m.tblRun != "6" {
				t.Fatalf("pinned %q pane %v tblRun %q, want run 6 in the logs pane", m.pinnedRun, m.pane, m.tblRun)
			}
			if m.selPipeline != "load_orders" || m.selLane != "ingest" {
				t.Errorf("selection = %s/%s, want ingest/load_orders", m.selLane, m.selPipeline)
			}
		})

		t.Run("an unknown command answers inline and keeps the palette", func(t *testing.T) {
			m := newPsModel(psvFixture(), "")
			m.update(key(':'))
			typeLine(m, "wq")
			m.update(psKey{kind: psKeyEnter})
			if m.command == nil || !strings.Contains(m.command.err, "unknown command :wq") {
				t.Fatalf("command state = %+v, want the inline unknown-command error", m.command)
			}
			if m.quit {
				t.Fatal("an unknown command must never tear the view down")
			}
		})

		t.Run(":logs with a missing run answers inline", func(t *testing.T) {
			m := newPsModel(psvFixture(), "")
			m.update(key(':'))
			typeLine(m, "logs 999")
			m.update(psKey{kind: psKeyEnter})
			if m.command == nil || !strings.Contains(m.command.err, "no run 999") {
				t.Fatalf("command state = %+v, want the inline no-run error", m.command)
			}
		})

		t.Run(":catalog opens the overlay loading and parks the list request", func(t *testing.T) {
			m := newPsModel(psvFixture(), "")
			m.update(key(':'))
			typeLine(m, "catalog")
			m.update(psKey{kind: psKeyEnter})
			if m.command != nil || m.catalog == nil || !m.catalog.loading {
				t.Fatalf("command %+v catalog %+v, want the overlay open and loading", m.command, m.catalog)
			}
			if req := m.takeCatalogReq(); req == nil || req.kind != psCatalogList {
				t.Fatalf("parked request = %+v, want the list fetch", req)
			}
		})

		t.Run(":search opens the telescope with an optional query", func(t *testing.T) {
			m := newPsModel(psvFixture(), "")
			m.update(key(':'))
			typeLine(m, "search ord")
			m.update(psKey{kind: psKeyEnter})
			if m.command != nil || m.search == nil {
				t.Fatalf("command %+v search %+v, want search open", m.command, m.search)
			}
			if got := string(m.search.query); got != "ord" {
				t.Fatalf("search query = %q, want ord", got)
			}
		})

		t.Run("tab cycles command names from the typed prefix", func(t *testing.T) {
			m := newPsModel(psvFixture(), "")
			m.update(key(':'))
			m.update(psKey{kind: psKeyTab})
			if got := string(m.command.input); got != "catalog" {
				t.Fatalf("first tab = %q, want catalog", got)
			}
			m.update(psKey{kind: psKeyTab})
			if got := string(m.command.input); got != "logs" {
				t.Fatalf("second tab = %q, want logs", got)
			}
			// Cycle through the remaining roster and wrap past the end.
			for range len(psCommands) - 1 {
				m.update(psKey{kind: psKeyTab})
			}
			if got := string(m.command.input); got != "catalog" {
				t.Fatalf("cycle must wrap to catalog, got %q", got)
			}
		})

		t.Run("tab after 'logs ' completes run ids from the snapshot", func(t *testing.T) {
			m := newPsModel(psvFixture(), "")
			m.update(key(':'))
			typeLine(m, "logs 1")
			m.update(psKey{kind: psKeyTab})
			if got := string(m.command.input); got != "logs 14" {
				t.Fatalf("first tab = %q, want logs 14 (newest first)", got)
			}
			m.update(psKey{kind: psKeyTab})
			if got := string(m.command.input); got != "logs 12" {
				t.Fatalf("second tab = %q, want logs 12", got)
			}
		})

		t.Run("arrows move the filtered list selection", func(t *testing.T) {
			m := newPsModel(psvFixture(), "")
			m.update(key(':'))
			if spec, _ := m.command.selected(); spec.name != "catalog" {
				t.Fatalf("initial selection = %q, want catalog", spec.name)
			}
			m.update(psKey{kind: psKeyDown})
			if spec, _ := m.command.selected(); spec.name != "logs" {
				t.Fatalf("after down = %q, want logs", spec.name)
			}
			m.update(psKey{kind: psKeyUp})
			if spec, _ := m.command.selected(); spec.name != "catalog" {
				t.Fatalf("after up = %q, want catalog", spec.name)
			}
		})

		t.Run("enter on a selected no-arg command with empty input runs it", func(t *testing.T) {
			m := newPsModel(psvFixture(), "")
			m.update(key(':'))
			// Move to q (last entry) via repeated down, or type-filter.
			typeLine(m, "q")
			// Clear to empty but keep selection on q via filter... re-open and select.
			m.command.input = nil
			m.command.sel = len(psCommandRoster) - 1 // q
			m.update(psKey{kind: psKeyEnter})
			if !m.quit {
				t.Fatal("enter on selected :q with empty input must quit")
			}
		})

		t.Run("typing filters the roster to matching prefixes", func(t *testing.T) {
			m := newPsModel(psvFixture(), "")
			m.update(key(':'))
			typeLine(m, "c")
			list := m.command.filtered()
			for _, spec := range list {
				if !strings.HasPrefix(spec.name, "c") {
					t.Fatalf("filtered entry %q does not match prefix c", spec.name)
				}
			}
			if len(list) < 2 {
				t.Fatalf("filtered = %v, want catalog + cancel at least", list)
			}
		})

		t.Run("'/' search still owns ':' as a literal query rune", func(t *testing.T) {
			m := newPsModel(psvFixture(), "")
			m.update(key('/'))
			m.update(key(':'))
			if m.command != nil {
				t.Fatal("':' inside the search overlay must stay a query rune")
			}
		})

		t.Run("roster exposes usage and categories for every command", func(t *testing.T) {
			if len(psCommandRoster) < 5 {
				t.Fatalf("roster too small: %d", len(psCommandRoster))
			}
			for _, spec := range psCommandRoster {
				if spec.name == "" || spec.usage == "" || spec.summary == "" || spec.category == "" {
					t.Fatalf("incomplete roster entry: %+v", spec)
				}
			}
		})

		t.Run("'p' freezes and unfreezes the live display for copy", func(t *testing.T) {
			m := newPsModel(psvFixture(), "")
			m.update(key('p'))
			if !m.frozen {
				t.Fatal("p must freeze the display")
			}
			if !strings.Contains(m.note, "frozen") {
				t.Fatalf("note = %q, want frozen guidance", m.note)
			}
			m.update(key('p'))
			if m.frozen {
				t.Fatal("second p must resume live updates")
			}
		})

		t.Run("'c' on an empty workspace opens the catalog", func(t *testing.T) {
			m := newPsModel(Snapshot{Ps: api.PsPayload{
				Engine: api.PsEngine{Version: "dev", Role: "leader", PID: 1, Uptime: "1s"},
			}}, "")
			if !psIsEmptyWorkspace(m) {
				t.Fatal("fixture must be an empty workspace")
			}
			m.update(key('c'))
			if m.catalog == nil || !m.catalog.loading {
				t.Fatalf("catalog = %+v, want open and loading", m.catalog)
			}
			if req := m.takeCatalogReq(); req == nil || req.kind != psCatalogList {
				t.Fatalf("parked request = %+v, want list", req)
			}
		})

		t.Run("'c' with work registered does not open catalog from the lanes pane", func(t *testing.T) {
			m := newPsModel(psvFixture(), "")
			m.update(key('c'))
			if m.catalog != nil {
				t.Fatal("'c' outside the logs pane must not open catalog when work is registered")
			}
			if m.confirmCancel {
				t.Fatal("'c' outside logs must not arm cancel either")
			}
		})
	})
}
