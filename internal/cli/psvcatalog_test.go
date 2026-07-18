package cli

import (
	"strings"
	"testing"

	"github.com/MateusAMP2119/iris-lakehouse/internal/api"
)

// openLoadedCatalog opens the overlay and absorbs a two-pack listing.
func openLoadedCatalog(m *psModel) {
	m.update(key(':'))
	typeLine(m, "catalog")
	m.update(psKey{kind: psKeyEnter})
	m.takeCatalogReq()
	m.absorbCatalog(psCatalogMsg{kind: psCatalogList, packs: []api.CatalogPack{
		{Name: "quake-monitor", Installed: false, Pipelines: []string{"quake_feed", "quake_report"}},
		{Name: "dlq-demo"},
	}})
}

// TestPsCatalogOverlay proves the overlay state machine (#219): listing, the
// arm/confirm install gate, the force offer, apply-and-return, inline failures.
func TestPsCatalogOverlay(t *testing.T) {
	t.Run("ps-catalog-overlay", func(t *testing.T) {
		t.Run("list absorb fills packs and clears loading", func(t *testing.T) {
			m := newPsModel(psvFixture(), "")
			openLoadedCatalog(m)
			c := m.catalog
			if c == nil || c.loading || len(c.packs) != 2 {
				t.Fatalf("overlay = %+v, want two packs loaded", c)
			}
		})

		t.Run("a failed list banners inline, view stands", func(t *testing.T) {
			m := newPsModel(psvFixture(), "")
			m.update(key(':'))
			typeLine(m, "catalog")
			m.update(psKey{kind: psKeyEnter})
			m.takeCatalogReq()
			m.absorbCatalog(psCatalogMsg{kind: psCatalogList, err: "catalog list failed: missing scope"})
			if m.catalog == nil || !strings.Contains(m.catalog.banner, "missing scope") || m.quit {
				t.Fatalf("overlay = %+v, want the inline banner", m.catalog)
			}
		})

		t.Run("enter arms, second enter parks the install request", func(t *testing.T) {
			m := newPsModel(psvFixture(), "")
			openLoadedCatalog(m)
			m.update(psKey{kind: psKeyEnter})
			if !m.catalog.armed || m.catalogReq != nil {
				t.Fatal("first enter must only arm")
			}
			m.update(psKey{kind: psKeyEnter})
			req := m.takeCatalogReq()
			if req == nil || req.kind != psCatalogInstall || req.pack != "quake-monitor" || req.force {
				t.Fatalf("second enter parked %+v, want the unforced install", req)
			}
			if m.catalog.busy == "" {
				t.Error("in-flight install must lock the overlay busy")
			}
		})

		t.Run("moving the cursor disarms", func(t *testing.T) {
			m := newPsModel(psvFixture(), "")
			openLoadedCatalog(m)
			m.update(psKey{kind: psKeyEnter})
			m.update(psKey{kind: psKeyDown})
			if m.catalog.armed || m.catalog.sel != 1 {
				t.Fatalf("overlay = %+v, want disarmed on pack 1", m.catalog)
			}
		})

		t.Run("an existing-path refusal offers force, f re-requests forced", func(t *testing.T) {
			m := newPsModel(psvFixture(), "")
			openLoadedCatalog(m)
			m.update(psKey{kind: psKeyEnter})
			m.update(psKey{kind: psKeyEnter})
			m.takeCatalogReq()
			m.absorbCatalog(psCatalogMsg{kind: psCatalogInstall, err: `catalog install failed: existing path(s) pipelines/healthy/iris-declare.yaml`})
			if !m.catalog.offer {
				t.Fatalf("overlay = %+v, want the force offer", m.catalog)
			}
			m.update(key('f'))
			req := m.takeCatalogReq()
			if req == nil || req.kind != psCatalogInstall || !req.force {
				t.Fatalf("f parked %+v, want the forced install", req)
			}
		})

		t.Run("a successful install banners the apply hint", func(t *testing.T) {
			m := newPsModel(psvFixture(), "")
			openLoadedCatalog(m)
			m.update(psKey{kind: psKeyEnter})
			m.update(psKey{kind: psKeyEnter})
			m.takeCatalogReq()
			m.absorbCatalog(psCatalogMsg{kind: psCatalogInstall, res: &api.CatalogInstallResult{Pack: "quake-monitor", Files: []string{"a", "b"}}})
			if !strings.Contains(m.catalog.banner, "installed quake-monitor (2 files)") {
				t.Fatalf("banner = %q, want the install summary", m.catalog.banner)
			}
		})

		t.Run("'a' parks install+apply and success returns to the main frame", func(t *testing.T) {
			m := newPsModel(psvFixture(), "")
			openLoadedCatalog(m)
			m.update(key('a'))
			req := m.takeCatalogReq()
			if req == nil || req.kind != psCatalogApply || !req.force {
				t.Fatalf("'a' parked %+v, want the forced install+apply", req)
			}
			m.absorbCatalog(psCatalogMsg{kind: psCatalogApply, res: &api.CatalogInstallResult{Pack: "quake-monitor", ApplyOrder: []string{"x", "y", "z"}}})
			if m.catalog != nil {
				t.Fatal("a successful apply must close the overlay")
			}
			if !strings.Contains(m.note, "quake-monitor applied (3 declarations)") {
				t.Errorf("note = %q, want the applied summary", m.note)
			}
		})

		t.Run("a not-leader apply banners inline with the leader hint", func(t *testing.T) {
			m := newPsModel(psvFixture(), "")
			openLoadedCatalog(m)
			m.update(key('a'))
			m.takeCatalogReq()
			m.absorbCatalog(psCatalogMsg{kind: psCatalogApply, err: "catalog install failed: this daemon is not the leader · leader: 10.0.0.9:7433"})
			if m.catalog == nil || !strings.Contains(m.catalog.banner, "leader: 10.0.0.9") {
				t.Fatalf("overlay = %+v, want the inline not-leader banner", m.catalog)
			}
		})

		t.Run("busy overlay swallows keys until the outcome lands", func(t *testing.T) {
			m := newPsModel(psvFixture(), "")
			openLoadedCatalog(m)
			m.update(key('a'))
			m.takeCatalogReq()
			m.update(psKey{kind: psKeyEnter})
			if m.catalogReq != nil {
				t.Fatal("keys during a busy action must not park new requests")
			}
		})

		t.Run("esc closes the overlay", func(t *testing.T) {
			m := newPsModel(psvFixture(), "")
			openLoadedCatalog(m)
			m.update(psKey{kind: psKeyEsc})
			if m.catalog != nil {
				t.Fatal("esc must close the overlay")
			}
		})
	})
}
