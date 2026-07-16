package cli

import (
	"fmt"
	"strings"
)

// This file renders the `iris ps` live view: a cell-grid screen buffer the
// frame composer writes plain runes into, with per-cell SGR attributes applied
// only at emission. Layout math never sees an escape code, so clipping, row
// inversion, and the search overlay's splicing stay correct by construction --
// no ANSI-aware string surgery anywhere. Each frame emits as one cursor-home
// write with per-line clear-to-EOL (never a full-screen clear), so redraws are
// flicker-free.

// psMinWidth/psMinHeight bound the smallest terminal the view lays out in;
// below them the frame degrades to a single advisory line (still restoring
// cleanly on exit).
const (
	psMinWidth  = 40
	psMinHeight = 8
)

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

// hrule draws a horizontal rule across row y.
func (b *screenBuf) hrule(y int, sgr string) {
	b.text(0, y, sgr, strings.Repeat("─", b.w))
}

// invertRow paints row y in inverse video (the selected-row treatment).
func (b *screenBuf) invertRow(y int) {
	if y < 0 || y >= b.h {
		return
	}
	for x := range b.w {
		b.cells[y*b.w+x].sgr = ansiInverse
	}
}

// dimAll repaints the whole frame dim -- the search overlay's backdrop.
func (b *screenBuf) dimAll() {
	for i := range b.cells {
		b.cells[i].sgr = ansiDim
	}
}

// box clears a rectangle and draws a rounded border around it (the overlay
// panes), with an optional title spliced into the top edge.
func (b *screenBuf) box(x, y, w, h int, sgr, title string) {
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
		b.text(x+2, y, sgr, " "+title+" ")
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

// psRoleSGR maps the engine role to its title-bar tint.
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

// psFrameHeight compacts the frame to its content: border, engine chrome, the
// current table plus a breathing row, and the advisory line. The run detail
// (a live log tail) and the search overlay (a centered float) keep the whole
// terminal.
func psFrameHeight(m *psModel, termH int) int {
	if m.screen == psScreenRun || m.search != nil {
		return termH
	}
	var rows int
	switch m.screen {
	case psScreenLanes:
		rows = len(deriveLanes(m.snap))
	case psScreenPipelines:
		rows = len(derivePipelines(m.snap, m.lane))
	case psScreenRuns:
		rows = len(deriveRuns(m.snap, m.pipeline, m.showAll))
	}
	advisory := 0
	if m.note != "" || m.warn != "" {
		advisory = 1
	}
	// border(2) + engine line + rule + blank + header + rows + blank + advisory
	h := 2 + 3 + 1 + rows + 1 + advisory
	if h > termH {
		h = termH
	}
	return h
}

// renderPsFrame composes the whole frame for the current model state: the
// outer border carrying the breadcrumb, role, key hints, and target in its
// edges (the issue's visual contract), the pinned engine line with its rule on
// the table screens, and the body table or run detail inside. Table screens
// compact to their content height; the space below the frame stays blank.
func renderPsFrame(m *psModel, w, h int, colorless bool) *screenBuf {
	if w < psMinWidth || h < psMinHeight {
		b := newScreenBuf(w, 1)
		b.text(0, 0, "", "iris ps: terminal too small")
		return b
	}
	h = psFrameHeight(m, h)
	b := newScreenBuf(w, h)

	// Body content renders into the inset region inside the border.
	inner := newScreenBuf(w-4, h-2)
	e := m.snap.ps.Engine
	body := 0
	if m.screen != psScreenRun {
		// Engine status line pinned atop the table screens, rule below it,
		// one breathing row before the table.
		x := 0
		put := func(sgr, s string) {
			inner.text(x, 0, sgr, s)
			x += len([]rune(s))
		}
		put(ansiDim, "ENGINE ")
		put(ansiCyan, e.Version)
		put("", fmt.Sprintf(" · pid %d · CPU %s · MEM %s · ", e.PID, cpuText(e.Load), memText(e.Load)))
		put(ansiCyan, fmt.Sprintf("%d running", e.RunningRuns))
		put("", " · ")
		put(ansiYellow, fmt.Sprintf("%d queued", e.QueuedRuns))
		inner.hrule(1, ansiDim)
		body = 3
	}
	// One advisory line above the footer: a transient action note, or the
	// standing soft-fetch warning while one is in force.
	advisory := m.note
	if advisory == "" {
		advisory = m.warn
	}
	if advisory != "" {
		inner.text(0, inner.h-1, ansiYellow, advisory)
	}
	bodyH := inner.h - body
	if advisory != "" {
		bodyH--
	}
	switch m.screen {
	case psScreenLanes:
		renderLanesTable(inner, m, body, bodyH, colorless)
	case psScreenPipelines:
		renderPipelinesTable(inner, m, body, bodyH, colorless)
	case psScreenRuns:
		renderRunsTable(inner, m, body, bodyH, colorless)
	case psScreenRun:
		renderRunDetail(inner, m, body, bodyH)
	}
	b.blit(inner, 2, 1)

	// The border: breadcrumb and role in the top edge, key hints and target in
	// the bottom edge -- the issue's title/footer contract.
	b.borderRow(0, "┌", "┐", m.breadcrumb(), ansiCyan, e.Role+" · up "+e.Uptime, psRoleSGR(e.Role))
	for y := 1; y < h-1; y++ {
		b.text(0, y, ansiDim, "│")
		b.text(w-1, y, ansiDim, "│")
	}
	b.borderRow(h-1, "└", "┘", psFooterHints(m), ansiDim, m.target, ansiDim)

	if m.search != nil {
		renderSearchOverlay(b, m)
	}
	return b
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

// borderRow draws one horizontal border edge with a left and a right text
// spliced into the rule (the frame's title and footer slots). The right text
// wins the space when the two collide; an over-wide right text keeps its tail
// (the end of a long socket path names the file).
func (b *screenBuf) borderRow(y int, lead, tail, left, leftSGR, right, rightSGR string) {
	b.text(0, y, ansiDim, lead+strings.Repeat("─", b.w-2)+tail)
	rightRunes := []rune(right)
	if maxRight := b.w - 8; len(rightRunes) > maxRight && maxRight > 0 {
		rightRunes = rightRunes[len(rightRunes)-maxRight:]
	}
	rx := b.w - 3 - len(rightRunes)
	leftMax := rx - 3
	leftRunes := []rune(left)
	if len(leftRunes) > leftMax-2 {
		if leftMax <= 2 {
			leftRunes = nil
		} else {
			leftRunes = leftRunes[:leftMax-2]
		}
	}
	if len(leftRunes) > 0 {
		b.text(2, y, leftSGR, " "+string(leftRunes)+" ")
	}
	if len(rightRunes) > 0 && rx > 2 {
		b.text(rx, y, rightSGR, " "+string(rightRunes)+" ")
	}
}

// psFooterHints names the keys live on the current screen.
func psFooterHints(m *psModel) string {
	if m.search != nil {
		return "⏎ jump · esc close"
	}
	if m.confirmCancel {
		return "cancel run " + m.runID + "? y/N"
	}
	switch m.screen {
	case psScreenLanes:
		return "q quit · ⏎ open lane · / search"
	case psScreenPipelines:
		return "← back · ⏎ open pipeline · / search"
	case psScreenRuns:
		if m.showAll {
			return "← back · ⏎ open run · a live · / search"
		}
		return "← back · ⏎ open run · a all · / search"
	default:
		if m.follow {
			return "← back · f follow off · c cancel"
		}
		return "← back · f follow · j/k scroll · c cancel"
	}
}

// renderTable lays a uniform table into the body: header row, then one row
// per entry, the selected row inverted (or marked with "> " when colorless).
// Column widths fit the widest cell; the last column absorbs the remainder.
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
	marker := 0
	if colorless {
		marker = 2 // room for the "> " selection marker
	}
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
				b.invertRow(ry)
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

// renderLanesTable renders level 1: one row per lane.
func renderLanesTable(b *screenBuf, m *psModel, y, bodyH int, colorless bool) {
	lanes := deriveLanes(m.snap)
	n := len(lanes)
	names := psCol("LANE", n, func(i int) string { return lanes[i].name })
	renderTable(b, y, bodyH, []psColumn{
		names,
		psCol("PIPELINES", n, func(i int) string { return fmt.Sprintf("%d", lanes[i].pipelines) }),
		psCol("QUEUED", n, func(i int) string { return fmt.Sprintf("%d", lanes[i].queued) }),
		psCol("RUNNING", n, func(i int) string { return fmt.Sprintf("%d", lanes[i].running) }),
		psCol("CPU", n, func(i int) string { return cpuText(lanes[i].load) }),
		psCol("MEM", n, func(i int) string { return memText(lanes[i].load) }),
	}, selIndex(m.selLane, names.cells), colorless)
}

// renderPipelinesTable renders level 2: one row per member pipeline.
func renderPipelinesTable(b *screenBuf, m *psModel, y, bodyH int, colorless bool) {
	rows := derivePipelines(m.snap, m.lane)
	n := len(rows)
	names := psCol("PIPELINE", n, func(i int) string { return rows[i].name })
	renderTable(b, y, bodyH, []psColumn{
		names,
		psColStyled("LATEST", n, func(i int) (string, string) { return rows[i].latest, psStateSGR(rows[i].latest) }),
		psCol("QUEUED", n, func(i int) string { return fmt.Sprintf("%d", rows[i].queued) }),
		psCol("RUNNING", n, func(i int) string { return fmt.Sprintf("%d", rows[i].running) }),
		psCol("CPU", n, func(i int) string { return cpuText(rows[i].load) }),
		psCol("MEM", n, func(i int) string { return memText(rows[i].load) }),
	}, selIndex(m.selPipeline, names.cells), colorless)
}

// renderRunsTable renders level 3: the pipeline's runs, newest first.
func renderRunsTable(b *screenBuf, m *psModel, y, bodyH int, colorless bool) {
	runs := deriveRuns(m.snap, m.pipeline, m.showAll)
	n := len(runs)
	ids := psCol("RUN", n, func(i int) string { return runs[i].ID })
	renderTable(b, y, bodyH, []psColumn{
		ids,
		psColStyled("STATE", n, func(i int) (string, string) { return runs[i].State, psStateSGR(runs[i].State) }),
		psCol("EXIT", n, func(i int) string { return exitCodeCell(runs[i].ExitCode) }),
		psCol("CPU", n, func(i int) string { return cpuText(runs[i].Load) }),
		psCol("MEM", n, func(i int) string { return memText(runs[i].Load) }),
	}, selIndex(m.selRun, ids.cells), colorless)
}

// renderRunDetail renders level 4: the fact line, a rule, then the log tail
// filling the body (following the tail, or scrolled back by m.scroll lines).
func renderRunDetail(b *screenBuf, m *psModel, y, bodyH int) {
	fact := "run " + m.runID + " · gone from the readout"
	factSGR := ansiDim
	if run, ok := findRun(m.snap, m.runID); ok {
		fact = run.State
		factSGR = psStateSGR(run.State)
		if run.ExitCode != nil {
			fact += " · exit " + exitCodeCell(run.ExitCode)
		}
		if run.State == "running" {
			fact += " · CPU " + cpuText(run.Load) + " · MEM " + memText(run.Load)
		}
	}
	b.text(0, y, factSGR, fact)
	b.hrule(y+1, ansiDim)

	paneY, paneH := y+2, bodyH-2
	if paneH <= 0 {
		return
	}
	logs := m.snap.logs
	end := len(logs) - m.scroll
	if end < 0 {
		end = 0
	}
	start := end - paneH
	if start < 0 {
		start = 0
	}
	for i, line := range logs[start:end] {
		b.text(0, paneY+i, "", line)
	}
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

	b.box(ox, oy, leftW, resultsH, ansiDim, "results")
	b.box(ox, oy+resultsH, leftW, promptH, ansiDim, "")
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
		marker := "  "
		if i == s.sel {
			marker = "> "
		}
		b.text(ox+2, ry, "", marker)
		b.text(ox+4, ry, ansiDim, fmt.Sprintf("%-8s", h.kind.kindTag()))
		b.text(ox+14, ry, "", h.label)
		if i == s.sel {
			for x := ox + 1; x < ox+leftW-1; x++ {
				b.cells[ry*b.w+x].sgr = ansiInverse
			}
		}
	}

	// Preview pane follows the selection.
	px := ox + leftW + 1
	pw := ow - leftW - 1
	title := "preview"
	if s.sel < len(s.hits) {
		title = "preview · " + s.hits[s.sel].label
	}
	b.box(px, oy, pw, oh, ansiDim, title)
	if s.sel < len(s.hits) {
		renderSearchPreview(b, m, s.hits[s.sel], px+2, oy+1, pw-4, oh-2)
	}
}

// renderSearchPreview fills the preview pane for one hit off the held
// snapshot: a lane previews its pipeline table, a pipeline its run table, a
// run its log tail (when it is the focused run) or its fact row.
func renderSearchPreview(b *screenBuf, m *psModel, h psHit, x, y, w, ph int) {
	sub := newScreenBuf(w, ph)
	switch h.kind {
	case psHitLane:
		pm := *m
		pm.lane = h.lane
		renderPipelinesTable(sub, &pm, 0, ph, false)
	case psHitPipeline:
		pm := *m
		pm.pipeline = h.pipeline
		pm.showAll = true
		renderRunsTable(sub, &pm, 0, ph, false)
	case psHitRun:
		if h.runID == m.runID && len(m.snap.logs) > 0 {
			logs := m.snap.logs
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
	// The preview never shows a selection of its own; strip the inversion,
	// then splice through the one copy routine.
	for i := range sub.cells {
		if sub.cells[i].sgr == ansiInverse {
			sub.cells[i].sgr = ""
		}
	}
	b.blit(sub, x, y)
}
