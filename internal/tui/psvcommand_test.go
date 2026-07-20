package tui

import (
	"strings"
	"testing"
)

// typeLine feeds a string into the model one rune at a time.
func typeLine(m *psModel, s string) {
	for _, r := range s {
		m.update(key(r))
	}
}

// TestPsCommandMode proves the ':' prompt (#218): open/close, dispatch through
// the model, inline errors, and tab completion.
func TestPsCommandMode(t *testing.T) {
	t.Run("ps-command-mode", func(t *testing.T) {
		t.Run("':' opens the prompt, esc closes it, the view stands", func(t *testing.T) {
			m := newPsModel(psvFixture(), "")
			m.update(key(':'))
			if m.command == nil {
				t.Fatal("':' did not open the command prompt")
			}
			typeLine(m, "cat")
			m.update(psKey{kind: psKeyEsc})
			if m.command != nil || m.quit {
				t.Fatal("esc must close the prompt without quitting")
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
				t.Fatalf("successful :logs must close the prompt (err %q)", m.command.err)
			}
			if m.pinnedRun != "6" || m.pane != psPaneLogs || m.tblRun != "6" {
				t.Fatalf("pinned %q pane %v tblRun %q, want run 6 in the logs pane", m.pinnedRun, m.pane, m.tblRun)
			}
			if m.selPipeline != "load_orders" || m.selLane != "ingest" {
				t.Errorf("selection = %s/%s, want ingest/load_orders", m.selLane, m.selPipeline)
			}
		})

		t.Run("an unknown command answers inline and keeps the prompt", func(t *testing.T) {
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
			m.update(psKey{kind: psKeyTab})
			m.update(psKey{kind: psKeyTab})
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

		t.Run("'/' search still owns ':' as a literal query rune", func(t *testing.T) {
			m := newPsModel(psvFixture(), "")
			m.update(key('/'))
			m.update(key(':'))
			if m.command != nil {
				t.Fatal("':' inside the search overlay must stay a query rune")
			}
		})
	})
}
