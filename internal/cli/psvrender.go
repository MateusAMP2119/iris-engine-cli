package cli

import (
	"fmt"
	"strings"

	"github.com/MateusAMP2119/iris-lakehouse/internal/api"
)

// This file renders the `iris ps` dashboard: a cell-grid screen buffer the
// frame composer writes plain runes into, with per-cell SGR attributes applied
// only at emission. Layout math never sees an escape code, so clipping, row
// inversion, and the search overlay's splicing stay correct by construction --
// no ANSI-aware string surgery anywhere. Each frame emits as one cursor-home
// write with per-line clear-to-EOL (never a full-screen clear), so redraws are
// flicker-free.
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
)

// ansiOrange is the heat ramp's third tone. The basic 16-color palette has no
// orange; 256-color index 208 is universal enough for a decorative tone, and
// the colorless path never emits it.
const ansiOrange = "\033[38;5;208m"

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

// invertRange paints cells [x0, x1) of row y in inverse video.
func (b *screenBuf) invertRange(y, x0, x1 int) {
	if y < 0 || y >= b.h {
		return
	}
	for x := x0; x < x1 && x < b.w; x++ {
		if x >= 0 {
			b.cells[y*b.w+x].sgr = ansiInverse
		}
	}
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
// entry, the selected row inverted (or marked with "> " when colorless).
// Column widths fit the widest cell; rows window over the height keeping the
// selection visible.
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
				b.invertRange(ry, 0, b.w)
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

// paneChrome picks a pane's border and title paint: cyan border when the pane
// holds the focus, dim otherwise; a colorless focused pane marks its title.
func paneChrome(focused, colorless bool, title string) (borderSGR, titleSGR, t string) {
	if focused {
		if colorless {
			return ansiDim, "", title + " *"
		}
		return ansiCyan, ansiCyan, title
	}
	return ansiDim, ansiDim, title
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

	top := 1
	paneH := h - 2 // rows between header and footer

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

	if m.search != nil {
		renderSearchOverlay(b, m)
	}
	return b
}

// renderPsHeader paints row 0: engine identity left, engine load right.
func renderPsHeader(b *screenBuf, m *psModel) {
	e := m.snap.ps.Engine
	x := 1
	put := func(sgr, s string) {
		b.text(x, 0, sgr, s)
		x += len([]rune(s))
	}
	put(ansiDim, "ENGINE ")
	put(ansiCyan, e.Version)
	put(ansiDim, " · ")
	put(psRoleSGR(e.Role), strings.ToUpper(e.Role))
	put("", fmt.Sprintf(" · pid %d · up %s", e.PID, e.Uptime))
	idEnd := x

	// The right side: CPU heat strip, MEM, run counts, sized to fit and shed
	// leftmost-first when the terminal narrows.
	cpu := " " + cpuText(e.Load)
	mem := " · MEM " + memText(e.Load)
	counts := fmt.Sprintf(" · %d running · %d queued", e.RunningRuns, e.QueuedRuns)
	stripW := 30
	fixed := len("CPU ") + len([]rune(cpu+mem+counts))
	if avail := b.w - 1 - idEnd - 3; fixed+stripW > avail {
		stripW = avail - fixed
		if stripW < 8 {
			stripW = 0
		}
		if fixed+stripW > avail {
			return // identity row only; the panes still carry the numbers
		}
	}
	x = b.w - 1 - (fixed + stripW)
	put(ansiDim, "CPU ")
	b.renderHeatStrip(x, 0, stripW, m.stripCPU("", stripW))
	x += stripW
	put("", cpu)
	put(ansiDim, mem)
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

// cpuSamples is the ring's CPU history (nil-safe for a ring not yet grown).
func (r *psRing) cpuSamples() []float64 {
	if r == nil {
		return nil
	}
	return r.cpu
}

// renderPsFooter paints the last row: the focused pane's key hints left, the
// watched target right (kept whole; its tail names the socket file). A
// transient action note or a standing soft-fetch warning takes the hints'
// slot -- the footer is the one row every width tier keeps.
func renderPsFooter(b *screenBuf, m *psModel) {
	y := b.h - 1
	// The open ':' prompt takes the whole footer row (#218): input, cursor, inline error.
	if m.command != nil {
		b.text(1, y, ansiCyan, ":")
		b.text(2, y, "", string(m.command.input)+"▏")
		if m.command.err != "" {
			b.text(4+len([]rune(string(m.command.input))), y, ansiYellow, "· "+m.command.err)
		}
		return
	}
	hints, hintSGR := psFooterHints(m), ansiDim
	advisory := m.note
	if advisory == "" {
		advisory = m.warn
	}
	if advisory != "" {
		hints, hintSGR = advisory, ansiYellow
	}
	target := m.target
	tx := b.w - 1 - len([]rune(target))
	if tx < 1 {
		tx = 1
	}
	if len([]rune(hints)) > tx-2 && tx > 3 {
		hints = string([]rune(hints)[:tx-3])
	}
	b.text(1, y, hintSGR, hints)
	b.text(tx, y, ansiDim, target)
}

// psFooterHints names the keys live for the current focus.
func psFooterHints(m *psModel) string {
	if m.search != nil {
		return "⏎ jump · esc close"
	}
	if m.confirmCancel {
		return "cancel run " + m.logsTarget() + "? y/N"
	}
	histHint := "h history"
	if m.histView {
		histHint = "h live"
	}
	switch m.pane {
	case psPaneLanes:
		if m.selPipeline == "" {
			return "tab panes · ↑↓ move · ⏎ unfold · / search · : cmd · " + histHint + " · q quit"
		}
		return "tab panes · ↑↓ move · ⏎ open runs · ← lane · / search · : cmd · " + histHint + " · q quit"
	case psPaneTable:
		if m.selPipeline == "" {
			return "tab panes · ↑↓ move · ⏎ open runs · / search · q quit"
		}
		if m.showAll {
			return "tab panes · ↑↓ move · ⏎ watch logs · a live · ← pipelines · q quit"
		}
		return "tab panes · ↑↓ move · ⏎ watch logs · a all · ← pipelines · q quit"
	default:
		if m.follow {
			return "tab panes · f follow off · c cancel · q quit"
		}
		return "tab panes · f follow · j/k scroll · c cancel · q quit"
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
	for _, r := range m.snap.ps.Residents {
		sinceRun[r.Pipeline] = r.TurnsSinceRun
	}

	var entries []railEntry
	cursor := 0
	for _, l := range deriveLanes(m.snap) {
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
			if colorless {
				b.text(x+1, ry, "", ">")
			} else {
				b.invertRange(ry, x+1, x+w-1)
			}
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
// selected pipeline's runs.
func renderTablePane(b *screenBuf, m *psModel, x, y, w, h int, colorless bool) {
	var (
		title string
		cols  []psColumn
		sel   int
	)
	if m.selPipeline != "" {
		title = "RUNS · " + m.selLane + "/" + m.selPipeline
		cols = runsColumns(deriveRuns(m.snap, m.selPipeline, m.showAll))
		sel = selIndex(m.tblRun, m.runKeys())
	} else {
		title = "PIPELINES · " + m.selLane
		cols = pipelinesColumns(derivePipelines(m.snap, m.selLane), w >= 90)
		sel = selIndex(m.tblPipeline, m.pipelineKeys())
	}
	borderSGR, titleSGR, title := paneChrome(m.pane == psPaneTable, colorless, title)
	b.box(x, y, w, h, borderSGR, titleSGR, title)
	if h < 4 {
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
		b.text(x+2, y+1, ansiDim, "no runs under this selection yet")
		return
	}
	logs := m.snap.logs
	if m.snap.logsRun != target {
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

	b.box(ox, oy, leftW, resultsH, ansiDim, ansiDim, "results")
	b.box(ox, oy+resultsH, leftW, promptH, ansiDim, ansiDim, "")
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
			b.invertRange(ry, ox+1, ox+leftW-1)
		}
	}

	// Preview pane follows the selection.
	px := ox + leftW + 1
	pw := ow - leftW - 1
	title := "preview"
	if s.sel < len(s.hits) {
		title = "preview · " + s.hits[s.sel].label
	}
	b.box(px, oy, pw, oh, ansiDim, ansiDim, title)
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
		if h.runID == m.snap.logsRun && len(m.snap.logs) > 0 {
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
