package tui

import (
	"fmt"
	"strings"

	"github.com/MateusAMP2119/iris-lakehouse/internal/api"
)

// This file renders the `iris ps` dashboard: a cell-grid screen buffer the
// frame composer writes plain runes into, with per-cell SGR attributes applied
// only at emission. Layout math never sees an escape code, so clipping, row
// selection accents, and the search overlay's splicing stay correct by
// construction -- no ANSI-aware string surgery anywhere. Each frame emits as
// one cursor-home write with per-line clear-to-EOL (never a full-screen clear),
// so redraws are flicker-free.
//
// The frame is four panes: the lanes rail on the left, and on the right a
// pipelines/runs table, the selected pipeline's detail charts, and the log
// tail, stacked. Narrow terminals shed panes: under psDetailMinWidth the
// detail box goes, under psLogsMinWidth the logs pane, under psRailMinWidth
// the rail; under psMinWidth the frame degrades to a single advisory line.

// The dashboard's width tiers and floor.
const (
	psMinWidth       = 40
	psMinHeight      = 8
	psRailMinWidth   = 70
	psLogsMinWidth   = 90
	psDetailMinWidth = 110
	psDetailMinRows  = 24
	// The bordered header card needs vertical room; short terminals keep the
	// one-line header.
	psHeaderCardMinH = 16
	psHeaderCardH    = 4
)

// psHeaderRows is the header's row budget at the given frame height.
func psHeaderRows(h int) int {
	if h >= psHeaderCardMinH {
		return psHeaderCardH
	}
	return 1
}

// psCell is one screen cell: its rune and the SGR code painting it ("" plain).
type psCell struct {
	r   rune
	sgr string
}

// screenBuf is one frame's cell grid.
type screenBuf struct {
	w, h  int
	cells []psCell
}

// newScreenBuf builds a blank frame of the given geometry.
func newScreenBuf(w, h int) *screenBuf {
	b := &screenBuf{w: w, h: h, cells: make([]psCell, w*h)}
	for i := range b.cells {
		b.cells[i].r = ' '
	}
	return b
}

// text writes s at (x, y) in the given SGR, clipping at the right edge and
// never wrapping.
func (b *screenBuf) text(x, y int, sgr, s string) {
	if y < 0 || y >= b.h {
		return
	}
	for _, r := range s {
		if x >= b.w {
			return
		}
		if x >= 0 {
			b.cells[y*b.w+x] = psCell{r: r, sgr: sgr}
		}
		x++
	}
}

// paintSelAccent marks a selected row Grok-style: a left magenta bar (▌).
// Colorless mode keeps a plain ">" so geometry tests stay SGR-free.
func paintSelAccent(b *screenBuf, x, y int, colorless bool) {
	if colorless {
		b.text(x, y, "", ">")
		return
	}
	b.text(x, y, ansiMagenta, "▌")
}

// dimAll repaints the whole frame dim -- the search overlay's backdrop.
func (b *screenBuf) dimAll() {
	for i := range b.cells {
		b.cells[i].sgr = ansiDim
	}
}

// box clears a rectangle and draws a rounded border around it, with an
// optional title spliced into the top edge in its own SGR.
func (b *screenBuf) box(x, y, w, h int, sgr, titleSGR, title string) {
	if w < 2 || h < 2 {
		return
	}
	for yy := y; yy < y+h; yy++ {
		for xx := x; xx < x+w; xx++ {
			if yy < 0 || yy >= b.h || xx < 0 || xx >= b.w {
				continue
			}
			b.cells[yy*b.w+xx] = psCell{r: ' '}
		}
	}
	horiz := strings.Repeat("─", w-2)
	b.text(x, y, sgr, "╭"+horiz+"╮")
	b.text(x, y+h-1, sgr, "╰"+horiz+"╯")
	for yy := y + 1; yy < y+h-1; yy++ {
		b.text(x, yy, sgr, "│")
		b.text(x+w-1, yy, sgr, "│")
	}
	if title != "" {
		if room := w - 6; room > 0 && len([]rune(title)) > room {
			title = string([]rune(title)[:room])
		}
		b.text(x+2, y, titleSGR, " "+title+" ")
	}
}

// render emits the frame as ANSI: cursor home, then per row a clear-to-EOL
// FIRST (an erase after a full-width row would eat the border's last column
// in the terminal's wrap-pending state) and the coalesced SGR runs, rows
// joined by \r\n (raw mode does no output post-processing), no trailing
// newline so the last row never scrolls, and a clear-below at the end so a
// frame shorter than the last one leaves no residue. With a disabled painter
// the emission is the same geometry with zero SGR bytes.
func (b *screenBuf) render(p painter) []byte {
	var out strings.Builder
	out.WriteString("\x1b[H")
	for y := range b.h {
		if y > 0 {
			out.WriteString("\r\n")
		}
		out.WriteString("\x1b[K")
		open := ""
		for x := range b.w {
			c := b.cells[y*b.w+x]
			sgr := c.sgr
			if !p.enabled {
				sgr = ""
			}
			if sgr != open {
				if open != "" {
					out.WriteString(ansiReset)
				}
				if sgr != "" {
					out.WriteString(sgr)
				}
				open = sgr
			}
			out.WriteRune(c.r)
		}
		if open != "" {
			out.WriteString(ansiReset)
		}
	}
	out.WriteString("\x1b[J")
	return []byte(out.String())
}

// plainLines renders the frame as plain text lines (test golden surface).
func (b *screenBuf) plainLines() []string {
	lines := make([]string, b.h)
	for y := range b.h {
		var sb strings.Builder
		for x := range b.w {
			sb.WriteRune(b.cells[y*b.w+x].r)
		}
		lines[y] = strings.TrimRight(sb.String(), " ")
	}
	return lines
}

// blit copies a sub-frame's cells into the frame at (x, y).
func (b *screenBuf) blit(src *screenBuf, x, y int) {
	for yy := range src.h {
		for xx := range src.w {
			tx, ty := x+xx, y+yy
			if tx < 0 || tx >= b.w || ty < 0 || ty >= b.h {
				continue
			}
			b.cells[ty*b.w+tx] = src.cells[yy*src.w+xx]
		}
	}
}

// psStateSGR maps a run state to its column color.
func psStateSGR(state string) string {
	switch state {
	case "running":
		return ansiCyan
	case "queued":
		return ansiYellow
	case "succeeded":
		return ansiGreen
	case "dead_lettered":
		return ansiRed
	default:
		return ""
	}
}

// psRoleSGR maps the engine role to its badge tint.
func psRoleSGR(role string) string {
	switch role {
	case "leader":
		return ansiGreen
	case "standby":
		return ansiYellow
	default:
		return ansiDim
	}
}

// heatCell quantizes one CPU-percent sample into the 4-tone heat ramp.
// psNoSample renders as an empty cell so absence never reads as idle-green.
func heatCell(pct float64) (rune, string) {
	switch {
	case pct < 0:
		return ' ', ""
	case pct < 25:
		return '░', ansiGreen
	case pct < 50:
		return '▒', ansiYellow
	case pct < 75:
		return '▓', ansiOrange
	default:
		return '█', ansiRed
	}
}

// renderHeatStrip draws the newest w samples right-aligned at (x, y): one
// cell per poll, newest right, older samples falling off the left edge.
func (b *screenBuf) renderHeatStrip(x, y, w int, samples []float64) {
	if w <= 0 {
		return
	}
	if len(samples) > w {
		samples = samples[len(samples)-w:]
	}
	sx := x + w - len(samples)
	for i, s := range samples {
		r, sgr := heatCell(s)
		if r != ' ' {
			b.text(sx+i, y, sgr, string(r))
		}
	}
}

// fitSamples compresses a history to at most w slots: when the recorded
// history is wider than the strip, each cell carries the maximum of its share,
// so a short spike stays visible at any zoom. A cell is absent only when its
// whole share is absent.
func fitSamples(samples []float64, w int) []float64 {
	if w <= 0 || len(samples) <= w {
		return samples
	}
	out := make([]float64, w)
	for i := range out {
		cell := float64(psNoSample)
		for _, s := range samples[i*len(samples)/w : (i+1)*len(samples)/w] {
			if s != psNoSample && (cell == psNoSample || s > cell) {
				cell = s
			}
		}
		out[i] = cell
	}
	return out
}

// stripRing resolves the ring a strip draws for the current view: the fine
// ring live, the coarse (hours-deep) ring under the 'h' history toggle.
func (m *psModel) stripRing(key string) *psRing {
	if m.histView {
		return m.coarse[key]
	}
	return m.rings[key]
}

// stripCPU is a strip's CPU samples for the current view, compressed to the
// strip's width so the coarse history spans the strip instead of scrolling
// off it.
func (m *psModel) stripCPU(key string, w int) []float64 {
	return fitSamples(m.stripRing(key).cpuSamples(), w)
}

// stripMem is a strip's memory samples for the current view, scaled to
// percent-of-peak and compressed to the strip's width.
func (m *psModel) stripMem(key string, w int) []float64 {
	r := m.stripRing(key)
	if r == nil {
		return nil
	}
	return fitSamples(memStripSamples(r), w)
}

// memStripSamples rescales a ring's memory history to percent-of-peak, so the
// heat ramp reads relative pressure within the visible window.
func memStripSamples(r *psRing) []float64 {
	peak := r.memPeak()
	out := make([]float64, len(r.mem))
	for i, m := range r.mem {
		if r.cpu[i] == psNoSample && m == 0 {
			out[i] = psNoSample
			continue
		}
		if peak == 0 {
			out[i] = 0
			continue
		}
		out[i] = float64(m) / float64(peak) * 100
	}
	return out
}

// psColumn is one table column: header, cell values, and the per-row SGR.
type psColumn struct {
	header string
	cells  []string
	sgr    []string // nil, or one SGR per cell
}

// psCol builds one plain column from a per-row accessor.
func psCol(header string, n int, cell func(int) string) psColumn {
	c := psColumn{header: header, cells: make([]string, n)}
	for i := range n {
		c.cells[i] = cell(i)
	}
	return c
}

// psColStyled builds one column whose cells carry a per-row SGR.
func psColStyled(header string, n int, cell func(int) (string, string)) psColumn {
	c := psColumn{header: header, cells: make([]string, n), sgr: make([]string, n)}
	for i := range n {
		c.cells[i], c.sgr[i] = cell(i)
	}
	return c
}

// renderTable lays a uniform table into b: header row, then one row per
// entry, the selected row marked with a left accent bar (or "> " when
// colorless). Column widths fit the widest cell; rows window over the height
// keeping the selection visible.
func renderTable(b *screenBuf, y, bodyH int, cols []psColumn, selRow int, colorless bool) {
	if len(cols) == 0 || bodyH < 2 {
		return
	}
	widths := make([]int, len(cols))
	for i, c := range cols {
		widths[i] = len([]rune(c.header))
		for _, cell := range c.cells {
			if l := len([]rune(cell)); l > widths[i] {
				widths[i] = l
			}
		}
	}
	// Always leave a column for the selection accent (▌ or ">").
	marker := 2
	x := marker
	starts := make([]int, len(cols))
	for i := range cols {
		starts[i] = x
		x += widths[i] + 2
	}

	for i, c := range cols {
		b.text(starts[i], y, ansiDim, c.header)
	}
	rows := len(cols[0].cells)
	visible := bodyH - 1
	// Keep the selected row on screen: scroll the window over the rows.
	top := 0
	if selRow >= visible {
		top = selRow - visible + 1
	}
	for r := top; r < rows && r-top < visible; r++ {
		ry := y + 1 + (r - top)
		for i, c := range cols {
			sgr := ""
			if c.sgr != nil {
				sgr = c.sgr[r]
			}
			if sgr == "" && c.cells[r] == "-" {
				sgr = ansiDim // absent samples recede
			}
			b.text(starts[i], ry, sgr, c.cells[r])
		}
		if r == selRow {
			if colorless {
				b.text(0, ry, "", "> ")
			} else {
				b.text(0, ry, ansiMagenta, "▌")
			}
		}
	}
}

// selIndex finds the cursor's row index among keys (0 when absent).
func selIndex(sel string, keys []string) int {
	for i, k := range keys {
		if k == sel {
			return i
		}
	}
	return 0
}

// paneChrome picks a pane's border and title paint: warm accent when the pane
// holds the focus, receding border chrome otherwise; a colorless focused pane
// marks its title.
func paneChrome(focused, colorless bool, title string) (borderSGR, titleSGR, t string) {
	if focused {
		if colorless {
			return ansiBorder, "", title + " *"
		}
		return ansiAccent, ansiAccent, title
	}
	return ansiBorder, ansiDim, title
}

// psIsEmptyWorkspace reports the quiet-engine zero state: nothing registered,
// nothing running. The four-pane grid is the wrong surface there — it reads as
// a broken dashboard — so the frame swaps in a guided empty card instead.
func psIsEmptyWorkspace(m *psModel) bool {
	if len(m.snap.Pipelines) > 0 || len(m.snap.Ps.Runs) > 0 {
		return false
	}
	// Residents without a listing still imply registered work.
	if len(m.snap.Ps.Residents) > 0 {
		return false
	}
	return len(deriveLanes(m.snap)) == 0
}

// renderPsFrame composes the whole dashboard for the current model state.
func renderPsFrame(m *psModel, w, h int, colorless bool) *screenBuf {
	if w < psMinWidth || h < psMinHeight {
		b := newScreenBuf(w, 1)
		b.text(0, 0, "", "iris ps: terminal too small")
		return b
	}
	b := newScreenBuf(w, h)
	renderPsHeader(b, m)
	renderPsFooter(b, m)

	top := psHeaderRows(h)
	paneH := h - top - 1 // rows between header and footer

	// Quiet engine: skip the hollow multi-pane grid for a guided empty card.
	// Overlays (commands, search, catalog) still compose on top.
	if psIsEmptyWorkspace(m) {
		renderEmptyWorkspace(b, m, 0, top, w, paneH, colorless)
	} else {
		railW := 0
		if w >= psRailMinWidth {
			railW = w / 4
			if railW > 38 {
				railW = 38
			}
			if railW < 26 {
				railW = 26
			}
			renderRailPane(b, m, 0, top, railW, paneH, colorless)
		}

		x := railW
		rw := w - x
		showDetail := w >= psDetailMinWidth && h >= psDetailMinRows
		showLogs := w >= psLogsMinWidth

		detailH := 0
		if showDetail {
			detailH = 5
		}
		tableH := paneH
		if showLogs {
			rows := len(m.pipelineKeys())
			if m.selPipeline != "" {
				rows = len(m.runKeys())
			}
			tableH = rows + 5
			if minLogs := 8; tableH > paneH-detailH-minLogs {
				tableH = paneH - detailH - minLogs
			}
			if tableH < 6 {
				tableH = 6
			}
		}
		renderTablePane(b, m, x, top, rw, tableH, colorless)
		if showDetail {
			renderDetailPane(b, m, x, top+tableH, rw, detailH, colorless)
		}
		if showLogs {
			renderLogsPane(b, m, x, top+tableH+detailH, rw, paneH-tableH-detailH, colorless)
		}
	}

	switch {
	case m.search != nil:
		renderSearchOverlay(b, m)
	case m.catalog != nil:
		renderCatalogOverlay(b, m)
	case m.command != nil:
		renderCommandOverlay(b, m)
	}
	return b
}

// emptyActions are the guided-card rows: the three concrete moves a fresh
// engine wants, key/command in accent, effect in dim.
var emptyActions = []struct{ key, desc string }{
	{"c", "browse the pack catalog, install a pipeline"},
	{":logs <id>", "pin any run's log tail"},
	{"iris declare apply", "register your own pipeline from YAML"},
}

// renderEmptyWorkspace paints the zero-state: engine is alive, nothing is
// registered yet. No brand art — the header wordmark carries the identity;
// the body is a status line, a bordered GET STARTED card, and two prose lines,
// centered with soft margins. Short terminals shed prose and the card's
// breathing rows, then fall back to a one-line nudge.
func renderEmptyWorkspace(b *screenBuf, m *psModel, x, y, w, h int, colorless bool) {
	if h < 3 || w < 20 {
		b.text(x+2, y+1, ansiDim, "no pipelines yet · press c for catalog")
		return
	}

	// Soft margins: content floats, not boxed.
	const hpad, vpad = 3, 1
	innerW := w - 2*hpad
	innerH := h - 2*vpad
	ox, oy := x+hpad, y+vpad
	if innerW < 26 || innerH < 5 {
		b.text(ox, oy, ansiDim, "no pipelines yet · c catalog")
		return
	}
	_ = colorless // accent/dim SGR apply; a disabled painter drops them at emit

	e := m.snap.Ps.Engine
	status := fmt.Sprintf("engine is quiet · %s · %s · pid %d · up %s",
		e.Version, strings.ToUpper(e.Role), e.PID, e.Uptime)
	if e.Version == "" {
		status = "engine is quiet"
	}

	// Card geometry: lead + key column + widest effect, clamped to the body.
	const lead, keyW = 3, 22
	cardW := 74
	if cardW > innerW {
		cardW = innerW
	}
	spacious := innerH >= 15 // breathing rows inside the card + prose below
	cardH := 2 + len(emptyActions)
	if spacious {
		cardH = 4 + 2*len(emptyActions) - 1
	}

	// The star mark leads the card in the roomy tier — the Grok welcome shape.
	markH := 0
	if spacious {
		markH = len(logoStar) + 1
	}

	contentH := markH + 2 + cardH // mark + status + gap + card
	if spacious {
		contentH += 3 // gap + two prose lines
	}
	startY := oy
	if contentH < innerH {
		startY = oy + (innerH-contentH)/2
	}
	center := func(ry int, sgr, s string) {
		runes := []rune(s)
		if len(runes) > innerW {
			s = string(runes[:innerW-1]) + "…"
			runes = []rune(s)
		}
		b.text(ox+(innerW-len(runes))/2, ry, sgr, s)
	}

	if markH > 0 {
		markW := logoWidth(logoStar)
		for i, row := range logoStar {
			b.text(ox+(innerW-markW)/2, startY+i, ansiMagenta, row)
		}
	}
	center(startY+markH, ansiDim, status)

	cardX := ox + (innerW-cardW)/2
	cardY := startY + markH + 2
	b.box(cardX, cardY, cardW, cardH, ansiBorder, ansiDim, "GET STARTED")
	itemY := cardY + 1
	step := 1
	if spacious {
		itemY++
		step = 2
	}
	for i, a := range emptyActions {
		ry := itemY + i*step
		b.text(cardX+1+lead, ry, ansiCyan, a.key)
		desc := a.desc
		if room := cardW - 2 - lead - keyW - 1; len([]rune(desc)) > room && room > 0 {
			desc = string([]rune(desc)[:room-1]) + "…"
		}
		b.text(cardX+1+lead+keyW, ry, ansiDim, desc)
	}

	if spacious {
		center(cardY+cardH+1, ansiDim, "This view is your live ops board — lanes, runs, load, logs.")
		center(cardY+cardH+2, ansiDim, "It lights up the moment work lands on the engine.")
	}
}

// renderPsHeader paints the header: a bordered identity card when the frame
// affords it, else the legacy one-line readout.
func renderPsHeader(b *screenBuf, m *psModel) {
	if psHeaderRows(b.h) == psHeaderCardH {
		renderPsHeaderCard(b, m)
		return
	}
	renderPsHeaderLine(b, m)
}

// renderPsHeaderCard paints rows 0..3: a full-width bordered card — brand mark
// and identity on the first content row with the live CPU/MEM readout right-
// aligned, pid and run counts on the second. A quiet engine keeps the load
// readout (real data) but drops the run counts (the hollow part).
func renderPsHeaderCard(b *screenBuf, m *psModel) {
	e := m.snap.Ps.Engine
	b.box(0, 0, b.w, psHeaderCardH, ansiBorder, "", "")

	left := 2
	if b.w >= psRailMinWidth && len(logoMark) > 0 {
		rows := logoMark
		if len(rows) > 2 {
			rows = rows[:2]
		}
		for i, row := range rows {
			b.text(2, 1+i, ansiMagenta, row)
		}
		left = 2 + logoWidth(logoMark) + 2
	}

	x := left
	put := func(y int, sgr, s string) {
		b.text(x, y, sgr, s)
		x += len([]rune(s))
	}

	put(1, ansiMagenta, "IRIS")
	if e.Version != "" {
		put(1, ansiDim, "  ")
		put(1, ansiDim, e.Version)
	}
	put(1, ansiDim, "  ·  ")
	put(1, psRoleSGR(e.Role), strings.ToUpper(orDefault(e.Role, "engine")))
	if e.Uptime != "" {
		put(1, ansiDim, "  ·  up ")
		put(1, "", e.Uptime)
	}
	renderHeaderLoad(b, m, 1, x, b.w-3, 0)

	x = left
	put(2, ansiDim, fmt.Sprintf("pid %d", e.PID))
	if !psIsEmptyWorkspace(m) {
		rc, qc := ansiCyan, ansiYellow
		if e.RunningRuns == 0 {
			rc = ansiDim
		}
		if e.QueuedRuns == 0 {
			qc = ansiDim
		}
		put(2, ansiDim, "  ·  ")
		put(2, rc, fmt.Sprintf("%d running", e.RunningRuns))
		put(2, ansiDim, "  ·  ")
		put(2, qc, fmt.Sprintf("%d queued", e.QueuedRuns))
	}
}

// renderPsHeaderLine paints the one-line header: identity left, live CPU/MEM
// right. A quiet engine keeps the load readout but drops the run counts.
func renderPsHeaderLine(b *screenBuf, m *psModel) {
	e := m.snap.Ps.Engine
	x := 1
	put := func(sgr, s string) {
		b.text(x, 0, sgr, s)
		x += len([]rune(s))
	}

	if psIsEmptyWorkspace(m) {
		// Quiet identity left; CPU/MEM right — real load even with no work
		// registered. Run counts stay off (they are the hollow part).
		put(ansiMagenta, "iris")
		if e.Version != "" {
			put(ansiDim, "  ")
			put(ansiDim, e.Version)
		}
		put(ansiDim, "  ·  ")
		put(psRoleSGR(e.Role), strings.ToUpper(orDefault(e.Role, "engine")))
		if e.Uptime != "" {
			put(ansiDim, "  ·  up ")
			put("", e.Uptime)
		}
		renderHeaderLoad(b, m, 0, x, b.w-1, 0)
		return
	}

	put(ansiDim, "ENGINE ")
	put(ansiCyan, e.Version)
	put(ansiDim, " · ")
	put(psRoleSGR(e.Role), strings.ToUpper(e.Role))
	put("", fmt.Sprintf(" · pid %d · up %s", e.PID, e.Uptime))
	idEnd := x

	// The right side: CPU heat strip, MEM, run counts, sized to fit and shed
	// leftmost-first when the terminal narrows.
	counts := fmt.Sprintf(" · %d running · %d queued", e.RunningRuns, e.QueuedRuns)
	nx, ok := renderHeaderLoad(b, m, 0, idEnd, b.w-1, len([]rune(counts)))
	if !ok {
		return // identity row only; the panes still carry the numbers
	}
	x = nx
	rc, qc := ansiCyan, ansiYellow
	if e.RunningRuns == 0 {
		rc = ansiDim
	}
	if e.QueuedRuns == 0 {
		qc = ansiDim
	}
	put(ansiDim, " · ")
	put(rc, fmt.Sprintf("%d running", e.RunningRuns))
	put(ansiDim, " · ")
	put(qc, fmt.Sprintf("%d queued", e.QueuedRuns))
}

// renderHeaderLoad right-aligns the CPU heat strip and CPU/MEM readout on
// row y ending at column right, reserving extraW cells after MEM for the
// caller's tail; the strip shrinks, then the whole block sheds, when the
// identity block leaves no room. Returns the x after MEM and whether anything
// was drawn.
func renderHeaderLoad(b *screenBuf, m *psModel, y, idEnd, right, extraW int) (int, bool) {
	e := m.snap.Ps.Engine
	cpu := " " + cpuText(e.Load)
	mem := " · MEM " + memText(e.Load)
	stripW := 30
	fixed := len("CPU ") + len([]rune(cpu+mem)) + extraW
	if avail := right - idEnd - 3; fixed+stripW > avail {
		stripW = avail - fixed
		if stripW < 8 {
			stripW = 0
		}
		if fixed+stripW > avail {
			return 0, false
		}
	}
	x := right - (fixed + stripW)
	b.text(x, y, ansiDim, "CPU ")
	x += len("CPU ")
	b.renderHeatStrip(x, y, stripW, m.stripCPU("", stripW))
	x += stripW
	b.text(x, y, "", cpu)
	x += len([]rune(cpu))
	b.text(x, y, ansiDim, mem)
	x += len([]rune(mem))
	return x, true
}

func orDefault(s, def string) string {
	if s == "" {
		return def
	}
	return s
}

// cpuSamples is the ring's CPU history (nil-safe for a ring not yet grown).
func (r *psRing) cpuSamples() []float64 {
	if r == nil {
		return nil
	}
	return r.cpu
}

// footerHint is one Grok-style shortcuts-bar entry: accent key + dim label.
type footerHint struct {
	key, desc string
}

// renderPsFooter paints the last row: a compact contextual shortcuts bar
// (key in magenta, desc in dim) on the left, watched target on the right.
// Notes and warnings take the bar's slot when set.
func renderPsFooter(b *screenBuf, m *psModel) {
	y := b.h - 1
	target := m.target
	// Bracketed role tag after the target, receding further than the dim
	// target itself; sheds first when the bar tightens.
	tag := ""
	if role := m.snap.Ps.Engine.Role; role != "" && target != "" {
		tag = " [" + strings.ToLower(role) + "]"
	}
	tx := b.w - 1 - len([]rune(target+tag))
	if tx < 1+8+2 && tag != "" {
		tag = ""
		tx = b.w - 1 - len([]rune(target))
	}
	if tx < 1 {
		tx = 1
	}
	maxHints := tx - 2
	if maxHints < 8 {
		maxHints = b.w - 2
	}
	paintTarget := func() {
		if target == "" || tx <= 1 {
			return
		}
		b.text(tx, y, ansiDim, target)
		if tag != "" {
			b.text(tx+len([]rune(target)), y, ansiBorder, tag)
		}
	}

	if m.command != nil {
		if m.command.err != "" {
			b.text(1, y, ansiYellow, clipCells(m.command.err, maxHints))
			return
		}
		paintFooterHints(b, 1, y, maxHints, []footerHint{
			{"↑↓", "select"}, {"tab", "complete"}, {"⏎", "run"}, {"esc", "close"},
		})
		return
	}

	advisory := m.note
	if advisory == "" {
		advisory = m.warn
	}
	if advisory != "" {
		b.text(1, y, ansiYellow, clipCells(advisory, maxHints))
		paintTarget()
		return
	}

	paintFooterHints(b, 1, y, maxHints, psFooterHints(m))
	paintTarget()
}

// paintFooterHints draws "key desc · key desc …" with accent keys, clipping
// whole entries when the bar runs out of room (never mid-glyph soup). A hint
// with an empty key paints desc only (dim) — used for free-text guidance.
func paintFooterHints(b *screenBuf, x, y, maxW int, hints []footerHint) {
	if maxW <= 0 || len(hints) == 0 {
		return
	}
	cur := x
	end := x + maxW
	for i, h := range hints {
		// " · " between entries
		sep := ""
		if i > 0 {
			sep = " · "
		}
		chunk := sep
		if h.key != "" {
			chunk += h.key
			if h.desc != "" {
				chunk += " " + h.desc
			}
		} else {
			chunk += h.desc
		}
		need := len([]rune(chunk))
		if cur+need > end {
			break
		}
		if sep != "" {
			b.text(cur, y, ansiDim, sep)
			cur += len([]rune(sep))
		}
		if h.key != "" {
			b.text(cur, y, ansiMagenta, h.key)
			cur += len([]rune(h.key))
			if h.desc != "" {
				b.text(cur, y, ansiDim, " "+h.desc)
				cur += 1 + len([]rune(h.desc))
			}
		} else if h.desc != "" {
			b.text(cur, y, ansiDim, h.desc)
			cur += len([]rune(h.desc))
		}
	}
}

// psFooterHints returns the few keys that matter for the current focus —
// Grok shortcuts-bar style, not a full key dump.
func psFooterHints(m *psModel) []footerHint {
	if m.frozen {
		return []footerHint{
			{"p", "resume"}, {"", "select text · copy with the terminal"},
		}
	}
	if m.search != nil {
		return []footerHint{{"⏎", "jump"}, {"esc", "close"}}
	}
	if m.confirmCancel {
		return []footerHint{{"y", "cancel " + m.logsTarget()}, {"N", "keep"}}
	}
	if m.catalog != nil {
		return []footerHint{{"↑↓", "move"}, {"⏎", "install"}, {"esc", "close"}}
	}
	if psIsEmptyWorkspace(m) {
		return []footerHint{
			{"c", "catalog"}, {":", "cmds"}, {"p", "freeze"}, {"?", "help"}, {"q", "quit"},
		}
	}
	switch m.pane {
	case psPaneLanes:
		if m.selPipeline == "" {
			return []footerHint{
				{"tab", "panes"}, {"↑↓", "move"}, {"⏎", "unfold"}, {"p", "freeze"}, {"/", "search"}, {"q", "quit"},
			}
		}
		return []footerHint{
			{"tab", "panes"}, {"↑↓", "move"}, {"⏎", "runs"}, {"p", "freeze"}, {"←", "back"}, {"q", "quit"},
		}
	case psPaneTable:
		if m.selPipeline == "" {
			return []footerHint{
				{"tab", "panes"}, {"↑↓", "move"}, {"⏎", "runs"}, {"p", "freeze"}, {"q", "quit"},
			}
		}
		all := "all"
		if m.showAll {
			all = "live"
		}
		return []footerHint{
			{"tab", "panes"}, {"↑↓", "move"}, {"⏎", "logs"}, {"a", all}, {"p", "freeze"}, {"q", "quit"},
		}
	default: // logs
		if m.follow {
			return []footerHint{
				{"tab", "panes"}, {"f", "unfollow"}, {"p", "freeze"}, {"c", "cancel"}, {"q", "quit"},
			}
		}
		return []footerHint{
			{"tab", "panes"}, {"f", "follow"}, {"p", "freeze"}, {"j/k", "scroll"}, {"q", "quit"},
		}
	}
}

// railEntry is one display row of the lanes rail.
type railEntry struct {
	kind     int // 0 lane, 1 metrics, 2 pipeline, 3 blank
	lane     psLaneRow
	pipeline psPipelineRow
}

// renderRailPane paints the LANES rail: per lane a header row with its queue
// badges, a dim metrics line (CPU, MEM, heat strip), and -- unfolded -- its
// member pipelines with state dots.
func renderRailPane(b *screenBuf, m *psModel, x, y, w, h int, colorless bool) {
	borderSGR, titleSGR, title := paneChrome(m.pane == psPaneLanes, colorless, "LANES")
	b.box(x, y, w, h, borderSGR, titleSGR, title)

	// Resident turn tallies (#206): a quiet loop records no rows, so the rail
	// badges its idle pipelines with turns since the last recorded run.
	sinceRun := map[string]uint64{}
	for _, r := range m.snap.Ps.Residents {
		sinceRun[r.Pipeline] = r.TurnsSinceRun
	}

	var entries []railEntry
	cursor := 0
	lanes := deriveLanes(m.snap)
	if len(lanes) == 0 {
		b.text(x+2, y+2, ansiDim, clipCells("no lanes yet", w-4))
		b.text(x+2, y+3, ansiDim, clipCells(":catalog to start", w-4))
		return
	}
	for _, l := range lanes {
		if len(entries) > 0 {
			entries = append(entries, railEntry{kind: 3})
		}
		if l.name == m.selLane && m.selPipeline == "" {
			cursor = len(entries)
		}
		entries = append(entries, railEntry{kind: 0, lane: l})
		entries = append(entries, railEntry{kind: 1, lane: l})
		if m.expanded[l.name] {
			for _, p := range derivePipelines(m.snap, l.name) {
				if l.name == m.selLane && p.name == m.selPipeline {
					cursor = len(entries)
				}
				entries = append(entries, railEntry{kind: 2, lane: l, pipeline: p})
			}
		}
	}

	innerH := h - 2
	top := 0
	if cursor >= innerH {
		top = cursor - innerH + 1
	}
	for i := top; i < len(entries) && i-top < innerH; i++ {
		ry := y + 1 + (i - top)
		e := entries[i]
		switch e.kind {
		case 0:
			fold := "▸"
			if m.expanded[e.lane.name] {
				fold = "▾"
			}
			b.text(x+2, ry, "", fold+" "+e.lane.name)
			badge := fmt.Sprintf("%dr·%dq", e.lane.running, e.lane.queued)
			badgeSGR := ansiDim
			if e.lane.running > 0 {
				badgeSGR = ansiCyan
			}
			b.text(x+w-2-len([]rune(badge)), ry, badgeSGR, badge)
		case 1:
			cpu, mem := cpuText(e.lane.load), memText(e.lane.load)
			b.text(x+4, ry, ansiDim, cpu+" "+mem)
			sx := x + 4 + len([]rune(cpu)) + 1 + len([]rune(mem)) + 1
			sw := x + w - 2 - sx
			b.renderHeatStrip(sx, ry, sw, m.stripCPU("l:"+e.lane.name, sw))
		case 2:
			b.text(x+4, ry, psStateSGR(e.pipeline.latest), "●")
			b.text(x+6, ry, "", e.pipeline.name)
			badge, badgeSGR := "", ""
			switch {
			case e.pipeline.running > 0:
				badge, badgeSGR = "run", ansiCyan
			case e.pipeline.queued > 0:
				badge, badgeSGR = fmt.Sprintf("%dq", e.pipeline.queued), ansiYellow
			case sinceRun[e.pipeline.name] > 0:
				badge, badgeSGR = fmt.Sprintf("t+%d", sinceRun[e.pipeline.name]), ansiDim
			}
			if badge != "" {
				b.text(x+w-2-len([]rune(badge)), ry, badgeSGR, badge)
			}
		}
		if i == cursor && (e.kind == 0 || e.kind == 2) {
			paintSelAccent(b, x+1, ry, colorless)
		}
	}
}

// pipelinesColumns builds the pipelines table's columns. The timing columns
// render dashes until the engine records run timestamps (issue #200), and a
// narrow pane sheds them whole rather than clipping their headers.
func pipelinesColumns(rows []psPipelineRow, wide bool) []psColumn {
	n := len(rows)
	cols := []psColumn{
		psCol("PIPELINE", n, func(i int) string { return rows[i].name }),
		psColStyled("LATEST", n, func(i int) (string, string) { return rows[i].latest, psStateSGR(rows[i].latest) }),
		psCol("Q", n, func(i int) string { return fmt.Sprintf("%d", rows[i].queued) }),
		psCol("R", n, func(i int) string { return fmt.Sprintf("%d", rows[i].running) }),
		psCol("CPU", n, func(i int) string { return cpuText(rows[i].load) }),
		psCol("MEM", n, func(i int) string { return memText(rows[i].load) }),
	}
	if wide {
		cols = append(cols,
			psCol("ELAPSED", n, func(int) string { return "-" }),
			psCol("LAST", n, func(int) string { return "-" }),
			psCol("AVG", n, func(int) string { return "-" }),
		)
	}
	return cols
}

// runsColumns builds the runs table's columns.
func runsColumns(runs []api.PsRun) []psColumn {
	n := len(runs)
	return []psColumn{
		psCol("RUN", n, func(i int) string { return runs[i].ID }),
		psColStyled("STATE", n, func(i int) (string, string) { return runs[i].State, psStateSGR(runs[i].State) }),
		psCol("EXIT", n, func(i int) string { return exitCodeCell(runs[i].ExitCode) }),
		psCol("CPU", n, func(i int) string { return cpuText(runs[i].Load) }),
		psCol("MEM", n, func(i int) string { return memText(runs[i].Load) }),
	}
}

// renderTablePane paints the table pane: the selected lane's pipelines, or the
// selected pipeline's runs. An empty selection gets a short nudge instead of a
// header-only table that looks broken.
func renderTablePane(b *screenBuf, m *psModel, x, y, w, h int, colorless bool) {
	var (
		title string
		cols  []psColumn
		sel   int
		empty string
	)
	if m.selPipeline != "" {
		title = "RUNS · " + m.selLane + "/" + m.selPipeline
		runs := deriveRuns(m.snap, m.selPipeline, m.showAll)
		cols = runsColumns(runs)
		sel = selIndex(m.tblRun, m.runKeys())
		if len(runs) == 0 {
			if m.showAll {
				empty = "no runs in history · press a for live filter"
			} else {
				empty = "no live runs · press a for full history · :logs <id> to pin"
			}
		}
	} else {
		title = "PIPELINES · " + m.selLane
		if m.selLane == "" {
			title = "PIPELINES"
		}
		rows := derivePipelines(m.snap, m.selLane)
		cols = pipelinesColumns(rows, w >= 90)
		sel = selIndex(m.tblPipeline, m.pipelineKeys())
		if len(rows) == 0 {
			empty = "no pipelines in this lane · :catalog to install a pack"
		}
	}
	borderSGR, titleSGR, title := paneChrome(m.pane == psPaneTable, colorless, title)
	b.box(x, y, w, h, borderSGR, titleSGR, title)
	if h < 4 {
		return
	}
	if empty != "" {
		b.text(x+3, y+2, ansiDim, clipCells(empty, w-6))
		return
	}
	sub := newScreenBuf(w-4, h-3)
	renderTable(sub, 0, sub.h, cols, sel, colorless)
	b.blit(sub, x+2, y+2)
}

// renderDetailPane paints the selected pipeline's chart box: CPU and MEM heat
// strips over the recorded load history (recent detail live, the hours-deep
// coarse history under the 'h' toggle), and the TIME row that waits on issue
// #200.
func renderDetailPane(b *screenBuf, m *psModel, x, y, w, h int, colorless bool) {
	name := m.detailPipeline()
	title := name
	if name != "" && m.histView {
		title += " · history"
	}
	borderSGR, titleSGR, title := paneChrome(false, colorless, title)
	b.box(x, y, w, h, borderSGR, titleSGR, title)
	if name == "" {
		b.text(x+3, y+2, ansiDim, "no pipeline selected")
		return
	}
	key := "p:" + name
	ring := m.stripRing(key)
	if ring == nil {
		ring = &psRing{}
	}

	cpuNow, memNow := "-", "-"
	for _, p := range derivePipelines(m.snap, m.selLane) {
		if p.name == name {
			cpuNow, memNow = cpuText(p.load), memText(p.load)
		}
	}
	cpuVal := cpuNow + " now"
	memVal := memNow + " now"
	if peak := ring.memPeak(); peak > 0 {
		memVal += " · " + memBytes(peak) + " peak"
	}

	valW := len([]rune(cpuVal))
	if l := len([]rune(memVal)); l > valW {
		valW = l
	}
	stripX := x + 8
	stripW := x + w - 3 - valW - 2 - stripX
	if stripW < 8 {
		return
	}
	row := func(ry int, label string, samples []float64, val string) {
		b.text(x+3, ry, ansiDim, label)
		b.renderHeatStrip(stripX, ry, stripW, samples)
		b.text(x+w-3-len([]rune(val)), ry, "", val)
	}
	row(y+1, "CPU", m.stripCPU(key, stripW), cpuVal)
	row(y+2, "MEM", m.stripMem(key, stripW), memVal)
	b.text(x+3, y+3, ansiDim, "TIME")
	b.text(stripX, y+3, ansiDim, "run durations arrive with engine timestamps (#200)")
}

// renderLogsPane paints the log tail of the watched run.
func renderLogsPane(b *screenBuf, m *psModel, x, y, w, h int, colorless bool) {
	target := m.logsTarget()
	title := "LOGS"
	if target != "" {
		mode := "following"
		if !m.follow {
			mode = "paused"
		}
		state := ""
		if run, ok := findRun(m.snap, target); ok {
			state = " · " + run.State
			title = "LOGS · " + run.Pipeline + "/" + target + state + " · " + mode
		} else {
			title = "LOGS · " + target + " · " + mode
		}
	}
	borderSGR, titleSGR, title := paneChrome(m.pane == psPaneLogs, colorless, title)
	b.box(x, y, w, h, borderSGR, titleSGR, title)

	innerH := h - 2
	if target == "" {
		b.text(x+2, y+1, ansiDim, "pick a run · ⏎ on a row · or :logs <id>")
		return
	}
	logs := m.snap.Logs
	if m.snap.LogsRun != target {
		logs = nil
	}
	end := len(logs) - m.scroll
	if end < 0 {
		end = 0
	}
	start := end - innerH
	if start < 0 {
		start = 0
	}
	for i, line := range logs[start:end] {
		b.text(x+2, y+1+i, logLineStyle(line), line)
	}
	if len(logs) > 0 {
		tail := fmt.Sprintf(" %d lines ", len(logs))
		b.text(x+w-2-len([]rune(tail)), y+h-1, ansiDim, tail)
	}
}

// renderCommandOverlay paints the dedicated COMMANDS section over a dimmed
// frame: filterable roster on the left (or run completions after `:logs `),
// a living detail pane on the right, and a cyan prompt bar on the bottom.
func renderCommandOverlay(b *screenBuf, m *psModel) {
	b.dimAll()
	c := m.command

	ow := b.w * 9 / 10
	oh := b.h * 8 / 10
	if ow < 36 {
		ow = b.w - 2
		if ow < 20 {
			ow = b.w
		}
	}
	if oh < 10 {
		oh = b.h - 2
		if oh < 6 {
			oh = b.h
		}
	}
	ox := (b.w - ow) / 2
	oy := (b.h - oh) / 2
	leftW := ow * 2 / 5
	if leftW < 22 {
		leftW = ow / 2
	}
	if leftW > 42 {
		leftW = 42
	}
	promptH := 3
	listH := oh - promptH

	// Left: COMMANDS list.
	b.box(ox, oy, leftW, listH, ansiBorder, ansiCyan, "COMMANDS")
	// Right: ABOUT / detail for the selection.
	px := ox + leftW + 1
	pw := ow - leftW - 1
	if pw < 12 {
		pw = 0
	}

	line := string(c.input)
	runsMode := false
	var runIDs []string
	if name, _, hasArg := strings.Cut(line, " "); hasArg && name == "logs" {
		runsMode = true
		runIDs = filteredRuns(line, m.snap)
	}

	innerH := listH - 2
	if innerH < 1 {
		innerH = 1
	}

	if runsMode {
		// Argument mode: show matching runs under the logs command.
		b.text(ox+2, oy+1, ansiDim, clipCells("runs matching prefix", leftW-4))
		top := 0
		// Reuse c.sel as the run cursor when cycling completions; clamp to list.
		sel := c.sel
		if len(runIDs) == 0 {
			sel = 0
		} else if sel >= len(runIDs) {
			sel = len(runIDs) - 1
		}
		if innerH > 1 && sel >= innerH-1 {
			top = sel - (innerH - 2)
		}
		row := 0
		for i := top; i < len(runIDs) && row < innerH-1; i++ {
			run, ok := findRun(m.snap, runIDs[i])
			if !ok {
				continue
			}
			ry := oy + 2 + row
			label := commandRunRowLabel(run, i == sel, leftW-4)
			b.text(ox+2, ry, "", label)
			if i == sel {
				paintSelAccent(b, ox+1, ry, false)
			}
			row++
		}
		if len(runIDs) == 0 {
			b.text(ox+2, oy+2, ansiYellow, clipCells("no matching runs", leftW-4))
		}
		if pw > 0 {
			title := "ABOUT · logs"
			b.box(px, oy, pw, listH, ansiBorder, ansiCyan, title)
			if spec, ok := lookupCmd("logs"); ok {
				body := commandDetailBody(spec, pw-4)
				for i, ln := range body {
					if i >= listH-2 {
						break
					}
					b.text(px+2, oy+1+i, "", clipCells(ln, pw-4))
				}
			}
		}
	} else {
		list := c.filtered()
		top := 0
		if innerH > 0 && c.sel >= innerH {
			top = c.sel - innerH + 1
		}
		for i := top; i < len(list) && i-top < innerH; i++ {
			spec := list[i]
			ry := oy + 1 + (i - top)
			// Category tag in dim, then the command row.
			label := commandListLabel(spec, i == c.sel, leftW-4)
			b.text(ox+2, ry, "", label)
			if i == c.sel {
				paintSelAccent(b, ox+1, ry, false)
			}
		}
		if len(list) == 0 {
			b.text(ox+2, oy+1, ansiYellow, clipCells("no matching commands", leftW-4))
		}
		if top+innerH < len(list) {
			b.text(ox+2, oy+listH-1, ansiDim, fmt.Sprintf("─ %d more ─", len(list)-top-innerH))
		}

		if pw > 0 {
			title := "ABOUT"
			var spec psCmdSpec
			var ok bool
			if spec, ok = c.selected(); ok {
				title = "ABOUT · " + spec.name
			}
			b.box(px, oy, pw, listH, ansiBorder, ansiCyan, title)
			if ok {
				body := commandDetailBody(spec, pw-4)
				for i, ln := range body {
					if i >= listH-2 {
						break
					}
					sgr := ""
					if strings.HasPrefix(ln, "Usage") || strings.HasPrefix(ln, "Keys") || strings.HasPrefix(ln, "Group") {
						sgr = ansiDim
					}
					if ln == "GLOBAL" || ln == "TABLE" || ln == "LOGS" {
						sgr = ansiCyan
					}
					b.text(px+2, oy+1+i, sgr, clipCells(ln, pw-4))
				}
			}
		}
	}

	// Prompt bar spanning the full overlay width.
	b.box(ox, oy+listH, ow, promptH, ansiBorder, ansiCyan, "")
	b.text(ox+2, oy+listH+1, ansiCyan, ":")
	b.text(ox+3, oy+listH+1, "", string(c.input)+"█")
	if c.err != "" {
		// Inline error rides the right side of the prompt when there is room.
		msg := "· " + c.err
		at := ox + 4 + len([]rune(string(c.input))) + 1
		if at < ox+ow-4 {
			b.text(at, oy+listH+1, ansiYellow, clipCells(msg, ox+ow-2-at))
		}
	} else {
		hint := "tab · ↑↓ · ⏎ · esc"
		if c.browse {
			hint = "browse · type to filter · esc"
		}
		at := ox + ow - 2 - len([]rune(hint))
		if at > ox+4+len([]rune(string(c.input))) {
			b.text(at, oy+listH+1, ansiDim, hint)
		}
	}
}

// lookupCmd finds a roster entry by name.
func lookupCmd(name string) (psCmdSpec, bool) {
	for _, c := range psCommandRoster {
		if c.name == name {
			return c, true
		}
	}
	return psCmdSpec{}, false
}

// renderSearchOverlay splices the telescope-style overlay over the dimmed
// frame: results above the prompt on the left, the live preview on the right.
func renderSearchOverlay(b *screenBuf, m *psModel) {
	b.dimAll()
	s := m.search

	ow := b.w * 9 / 10
	oh := b.h * 8 / 10
	ox := (b.w - ow) / 2
	oy := (b.h - oh) / 2
	leftW := ow * 2 / 5
	promptH := 3
	resultsH := oh - promptH

	b.box(ox, oy, leftW, resultsH, ansiBorder, ansiDim, "results")
	b.box(ox, oy+resultsH, leftW, promptH, ansiBorder, ansiDim, "")
	b.text(ox+2, oy+resultsH+1, ansiCyan, "> ")
	b.text(ox+4, oy+resultsH+1, "", string(s.query)+"▏")

	// Results list, bottom-anchored: the best hit (index 0) sits nearest the
	// prompt; the selection renders inverted. The list windows over the hits
	// so a selection moved past the pane height stays visible.
	innerH := resultsH - 2
	top := 0
	if innerH > 0 && s.sel >= innerH {
		top = s.sel - innerH + 1
	}
	for i := top; i < len(s.hits) && i-top < innerH; i++ {
		h := s.hits[i]
		ry := oy + resultsH - 2 - (i - top)
		b.text(ox+2, ry, "", "  ")
		b.text(ox+4, ry, ansiDim, fmt.Sprintf("%-8s", h.kind.kindTag()))
		b.text(ox+14, ry, "", h.label)
		if i == s.sel {
			paintSelAccent(b, ox+1, ry, false)
		}
	}

	// Preview pane follows the selection.
	px := ox + leftW + 1
	pw := ow - leftW - 1
	title := "preview"
	if s.sel < len(s.hits) {
		title = "preview · " + s.hits[s.sel].label
	}
	b.box(px, oy, pw, oh, ansiBorder, ansiDim, title)
	if s.sel < len(s.hits) {
		renderSearchPreview(b, m, s.hits[s.sel], px+2, oy+1, pw-4, oh-2)
	}
}

// renderSearchPreview fills the preview pane for one hit off the held
// snapshot: a lane previews its pipeline table, a pipeline its run table, a
// run its log tail (when it is the watched run) or its fact row.
func renderSearchPreview(b *screenBuf, m *psModel, h psHit, x, y, w, ph int) {
	sub := newScreenBuf(w, ph)
	switch h.kind {
	case psHitLane:
		renderTable(sub, 0, ph, pipelinesColumns(derivePipelines(m.snap, h.lane), w >= 90), -1, false)
	case psHitPipeline:
		renderTable(sub, 0, ph, runsColumns(deriveRuns(m.snap, h.pipeline, true)), -1, false)
	case psHitRun:
		if h.runID == m.snap.LogsRun && len(m.snap.Logs) > 0 {
			logs := m.snap.Logs
			start := len(logs) - ph
			if start < 0 {
				start = 0
			}
			for i, line := range logs[start:] {
				sub.text(0, i, "", line)
			}
		} else if run, ok := findRun(m.snap, h.runID); ok {
			fact := run.State
			if run.ExitCode != nil {
				fact += " · exit " + exitCodeCell(run.ExitCode)
			}
			if run.State == "running" {
				fact += " · CPU " + cpuText(run.Load) + " · MEM " + memText(run.Load)
			}
			sub.text(0, 0, psStateSGR(run.State), "run "+run.ID+" · "+run.Pipeline)
			sub.text(0, 1, "", fact)
		}
	}
	b.blit(sub, x, y)
}

// logLineStyle picks the logs pane's style for one naturalized capture line: a
// framed capture's protocol and stamp lines render marked by origin ([engine],
// [pipeline], [iris]); the pipeline's own log lines stay unstyled.
func logLineStyle(line string) string {
	switch {
	case strings.HasPrefix(line, "[engine] "):
		return ansiCyan
	case strings.HasPrefix(line, "[pipeline] "):
		return ansiOrange
	case strings.HasPrefix(line, "[iris] "):
		return ansiDim
	default:
		return ""
	}
}

// clipCells bounds s to w cells for a box-interior line.
func clipCells(s string, w int) string {
	r := []rune(s)
	if w < 0 {
		return ""
	}
	if len(r) > w {
		return string(r[:w])
	}
	return s
}

// renderCatalogOverlay splices the catalog picker over the dimmed frame (#219):
// pack list left with installed badges and tags, live preview right (README,
// pipeline tree, requires, sha), banner and key hints on the bottom band.
func renderCatalogOverlay(b *screenBuf, m *psModel) {
	b.dimAll()
	c := m.catalog

	ow := b.w * 9 / 10
	oh := b.h * 8 / 10
	ox := (b.w - ow) / 2
	oy := (b.h - oh) / 2
	leftW := ow * 2 / 5
	footH := 3
	listH := oh - footH

	b.box(ox, oy, leftW, listH, ansiBorder, ansiDim, "catalog")

	// Pack list, selection inverted; installed and shadowed badges plus tags ride
	// the row. The list windows over the packs so a selection moved past the pane
	// height stays visible (the search overlay's rule).
	innerH := listH - 2
	top := 0
	if innerH > 0 && c.sel >= innerH {
		top = c.sel - innerH + 1
	}
	for i := top; i < len(c.packs) && i-top < innerH; i++ {
		p := c.packs[i]
		label := p.Name
		if p.Installed {
			label += " ●installed"
		}
		if p.Shadowed {
			label += " (shadowed)"
		}
		if len(p.Tags) > 0 {
			label += "  " + strings.Join(p.Tags, ",")
		}
		row := oy + 1 + (i - top)
		b.text(ox+2, row, "", clipCells(label, leftW-4))
		if i == c.sel {
			paintSelAccent(b, ox+1, row, false)
		}
	}
	if top+innerH < len(c.packs) {
		b.text(ox+2, oy+listH-1, ansiDim, fmt.Sprintf("─ %d more ─", len(c.packs)-top-innerH))
	}
	if c.loading {
		b.text(ox+2, oy+1, ansiDim, "loading…")
	} else if len(c.packs) == 0 {
		b.text(ox+2, oy+1, ansiDim, "no packs")
	}

	// Preview pane follows the selection.
	px := ox + leftW + 1
	pw := ow - leftW - 1
	title := "preview"
	if p := c.selected(); p != nil {
		title = "preview · " + p.Name
	}
	b.box(px, oy, pw, listH, ansiBorder, ansiDim, title)
	if p := c.selected(); p != nil {
		tx, ty, tw := px+2, oy+1, pw-4
		line := func(sgr, s string) {
			if ty < oy+listH-1 {
				b.text(tx, ty, sgr, clipCells(s, tw))
				ty++
			}
		}
		line(ansiCyan, p.Name+"  ["+p.Source+"]")
		if p.Description != "" {
			line("", p.Description)
		}
		if p.Requires != "" {
			line(ansiDim, "requires "+p.Requires)
		}
		if p.SHA256 != "" {
			line(ansiDim, "sha256 "+shortDigest(p.SHA256))
		}
		if len(p.Pipelines) > 0 {
			line("", "pipelines: "+strings.Join(p.Pipelines, ", "))
		}
		if len(p.ApplyOrder) > 0 {
			line(ansiDim, "apply order:")
			for _, step := range p.ApplyOrder {
				line(ansiDim, "  "+step)
			}
		}
		if p.Readme != "" {
			line("", "")
			for _, rl := range strings.Split(p.Readme, "\n") {
				line(ansiDim, rl)
			}
		}
	}

	// Bottom band: banner (yellow) above the key hints.
	b.box(ox, oy+listH, ow, footH, ansiBorder, ansiDim, "")
	hint := "⏎ install · a install+apply · esc close"
	switch {
	case c.busy != "":
		hint = c.busy
	case c.offer:
		hint = "f overwrites existing paths · esc close"
	case c.armed:
		if p := c.selected(); p != nil {
			hint = "install " + p.Name + "? ⏎ confirms · any move disarms"
		}
	}
	if c.banner != "" {
		b.text(ox+2, oy+listH+1, ansiYellow, clipCells(c.banner, ow-4))
		b.text(ox+2, oy+listH+footH-1, ansiDim, clipCells(hint, ow-4))
	} else {
		b.text(ox+2, oy+listH+1, ansiDim, clipCells(hint, ow-4))
	}
}
