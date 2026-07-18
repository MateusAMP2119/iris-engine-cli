package cli

import (
	"fmt"
	"strings"

	"github.com/MateusAMP2119/iris-lakehouse/internal/api"
)

// This file is the `iris ps` catalog overlay's state machine (#219): pack list
// left, live preview right, install through /catalog/install with an arm/confirm
// gate, 'a' applying in the answered order. All outcomes land as messages on the
// single-writer loop; a failure banners inline and never tears the view down.

// psCatalogReqKind names one overlay action the loop hands to the runner.
type psCatalogReqKind int

// The overlay's actions.
const (
	psCatalogList psCatalogReqKind = iota
	psCatalogInstall
	psCatalogApply
)

// psCatalogReq is one action request the model parks for the loop.
type psCatalogReq struct {
	kind  psCatalogReqKind
	pack  string
	force bool
}

// psCatalogMsg is one action outcome the loop absorbs.
type psCatalogMsg struct {
	kind     psCatalogReqKind
	packs    []api.CatalogPack
	warnings []string
	res      *api.CatalogInstallResult
	err      string // inline failure text; "" on success
}

// psCatalog is the open overlay's state.
type psCatalog struct {
	loading bool
	packs   []api.CatalogPack
	sel     int
	armed   bool   // enter pressed once; the next enter confirms the install
	offer   bool   // the last install refused on existing paths; f overwrites
	busy    string // in-flight action label, "" when idle
	banner  string // inline error or notice
}

// openCatalog opens the overlay in its loading state and parks the list request.
func (m *psModel) openCatalog() {
	m.catalog = &psCatalog{loading: true}
	m.catalogReq = &psCatalogReq{kind: psCatalogList}
}

// takeCatalogReq hands the loop the parked request, once.
func (m *psModel) takeCatalogReq() *psCatalogReq {
	r := m.catalogReq
	m.catalogReq = nil
	return r
}

// selected returns the pack under the cursor, nil on an empty list.
func (c *psCatalog) selected() *api.CatalogPack {
	if c.sel < 0 || c.sel >= len(c.packs) {
		return nil
	}
	return &c.packs[c.sel]
}

// updateCatalog routes a keypress while the overlay is open.
func (m *psModel) updateCatalog(k psKey) {
	c := m.catalog
	if c.busy != "" && k.kind != psKeyCtrlC {
		return // one action at a time; the outcome message unlocks the overlay
	}
	switch k.kind {
	case psKeyCtrlC:
		m.quit = true
	case psKeyEsc:
		m.catalog = nil
	case psKeyUp:
		m.catalogMove(-1)
	case psKeyDown:
		m.catalogMove(1)
	case psKeyEnter:
		m.catalogEnter()
	case psKeyRune:
		switch k.r {
		case 'q':
			m.catalog = nil
		case 'j':
			m.catalogMove(1)
		case 'k':
			m.catalogMove(-1)
		case 'f', 'F':
			if p := c.selected(); c.offer && p != nil {
				c.offer, c.banner, c.busy = false, "", "overwriting "+p.Name+"…"
				m.catalogReq = &psCatalogReq{kind: psCatalogInstall, pack: p.Name, force: true}
			}
		case 'a', 'A':
			if p := c.selected(); p != nil {
				c.armed, c.offer, c.banner, c.busy = false, false, "", "applying "+p.Name+"…"
				m.catalogReq = &psCatalogReq{kind: psCatalogApply, pack: p.Name, force: true}
			}
		}
	}
}

// catalogMove shifts the pack cursor and disarms any pending confirm.
func (m *psModel) catalogMove(delta int) {
	c := m.catalog
	c.armed, c.offer, c.banner = false, false, ""
	c.sel += delta
	if c.sel < 0 {
		c.sel = 0
	}
	if c.sel >= len(c.packs) {
		c.sel = len(c.packs) - 1
	}
	if c.sel < 0 {
		c.sel = 0
	}
}

// catalogEnter arms the install on the first press and requests it on the second.
func (m *psModel) catalogEnter() {
	c := m.catalog
	p := c.selected()
	if p == nil {
		return
	}
	if !c.armed {
		c.armed, c.offer, c.banner = true, false, ""
		return
	}
	c.armed, c.busy = false, "installing "+p.Name+"…"
	m.catalogReq = &psCatalogReq{kind: psCatalogInstall, pack: p.Name}
}

// absorbCatalog folds one action outcome into the overlay (which may have closed meanwhile).
func (m *psModel) absorbCatalog(cm psCatalogMsg) {
	c := m.catalog
	if c == nil {
		return
	}
	c.busy = ""
	switch cm.kind {
	case psCatalogList:
		c.loading = false
		c.packs = cm.packs
		c.banner = cm.err
		if cm.err == "" && len(cm.warnings) > 0 {
			c.banner = strings.Join(cm.warnings, " · ")
		}
		if c.sel >= len(c.packs) {
			c.sel = 0
		}
	case psCatalogInstall:
		if cm.err != "" {
			c.banner = cm.err
			c.offer = strings.Contains(cm.err, "existing path")
			return
		}
		c.banner = fmt.Sprintf("installed %s (%d files) · a applies", cm.res.Pack, len(cm.res.Files))
	case psCatalogApply:
		if cm.err != "" {
			c.banner = cm.err
			return
		}
		// Install-then-apply-then-watch: close the overlay; the 1s poll shows the
		// queued and running rows landing in the main frame.
		m.catalog = nil
		m.note = fmt.Sprintf("pack %s applied (%d declarations) · runs landing", cm.res.Pack, len(cm.res.ApplyOrder))
	}
}
