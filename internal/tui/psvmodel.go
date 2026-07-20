package tui

import (
	"sort"
	"strings"
	"time"

	"github.com/MateusAMP2119/iris-lakehouse/internal/api"
)

// This file is the state of the `iris ps` dashboard: the polled snapshot, the
// pane-focus model over the four panes (lanes rail, table, detail, logs), the
// lane tree's expand/collapse state, the client-side load history rings behind
// every heat strip, and the pure derivations that turn the /ps payload plus
// the pipeline listing into rows. Everything here is plain data and pure
// functions -- no terminal, no HTTP -- so the whole state machine is
// unit-testable with fixture payloads.

// psPane names one of the dashboard's focusable panes. The detail box is
// display-only (nothing to operate), so tab skips it.
type psPane int

// The focusable panes, in tab order.
const (
	psPaneLanes psPane = iota
	psPaneTable
	psPaneLogs
)

// psRingCap bounds every fine load-history ring, comfortably past the widest
// strip any layout renders. The daemon's fine ring is deeper; a re-seed trims
// to this.
const psRingCap = 240

// psNoSample marks a tick with no load sample in a ring (idle lane, absent
// payload load); strips render it as an empty cell, never a fabricated zero.
// It is the same sentinel the wire history uses (api.PsHistoryNoSample), so
// re-seeded slots need no translation.
const psNoSample = api.PsHistoryNoSample

// psRing is one entity's sampled load history, newest last. cpu holds
// percentages (psNoSample for a sampleless tick), mem resident bytes.
type psRing struct {
	cpu []float64
	mem []int64
}

// push appends one tick's sample, evicting past the cap.
func (r *psRing) push(cpu float64, mem int64) {
	r.cpu = append(r.cpu, cpu)
	r.mem = append(r.mem, mem)
	if len(r.cpu) > psRingCap {
		r.cpu = r.cpu[len(r.cpu)-psRingCap:]
		r.mem = r.mem[len(r.mem)-psRingCap:]
	}
}

// memPeak is the largest resident sample in the ring (0 when none).
func (r *psRing) memPeak() int64 {
	var peak int64
	for _, m := range r.mem {
		if m > peak {
			peak = m
		}
	}
	return peak
}

// Snapshot is one poll's worth of view data: the /ps payload (always the
// ?all=true history), the pipeline listing (?all=1, idle pipelines included,
// each row carrying its lane), and the tailed run's log lines. logsRun names
// the run the tail belongs to, so a snapshot buffered before a retarget never
// paints the previous run's lines under the new run's title.
type Snapshot struct {
	Ps        api.PsPayload
	Pipelines []api.PipelineListItem
	Logs      []string
	LogsRun   string
	// staleAge marks a snapshot revived from the last-known-state cache (the
	// engine was unreachable at open): how old the cached state is. Zero on a
	// live snapshot. The view opens it under the unreachable banner.
	StaleAge time.Duration
}

// psModel is the dashboard's whole state. update() is the only mutator the
// event loop drives.
type psModel struct {
	pane psPane

	// The lanes rail: which lanes are unfolded, and the tree cursor. A cursor
	// with selPipeline == "" sits on a lane row; otherwise on that pipeline's
	// row inside its (unfolded) lane.
	expanded    map[string]bool
	selLane     string
	selPipeline string

	// The table pane's cursor, keyed by row identity so a re-poll that
	// reorders rows keeps the cursor on the same entity. tblPipeline cursors
	// the pipelines table (lane row selected), tblRun the runs table
	// (pipeline row selected).
	tblPipeline string
	tblRun      string

	// pinnedRun is an explicit log target picked in the runs table; "" lets
	// the target follow the selection automatically.
	pinnedRun string

	showAll       bool   // runs table: 'a' toggled the whole history in
	follow        bool   // logs pane: tail follows new output
	scroll        int    // logs pane: lines scrolled back when not following
	confirmCancel bool   // logs pane: y/N cancel confirm armed
	histView      bool   // strips: 'h' toggled the coarse hours-deep history in
	note          string // transient action outcome, cleared on the next key
	warn          string // standing soft-fetch warning, cleared by the next good poll
	frozen        bool   // live polls paused so terminal select/copy is stable

	search  *psSearch  // non-nil while the search overlay is open
	command *psCommand // non-nil while the ':' command prompt is open (#218)

	catalog    *psCatalog    // non-nil while the catalog overlay is open (#219)
	catalogReq *psCatalogReq // overlay action parked for the loop, consumed via takeCatalogReq
	catalogSeq int           // monotonic request correlation counter (stale outcomes drop)

	// rings holds every heat strip's fine history: key "" is the engine,
	// "l:<name>" a lane, "p:<name>" a pipeline. Seeded from the daemon's
	// recorded history and grown one slot per collector tick (the payload's
	// sample_tick names the tick, so a poll that races the collector never
	// double-counts). coarse holds the same keys' coarse (per-bucket-maximum)
	// history, hours deep, refreshed only on a history re-seed -- exactly its
	// own cadence. lastTick is the newest absorbed collector tick.
	rings    map[string]*psRing
	coarse   map[string]*psRing
	lastTick uint64

	snap   Snapshot
	target string // footer right slot: "remote <host>" or "local <socket>"
	quit   bool
}

// newPsModel builds the dashboard model over the first snapshot: lanes pane
// focused, the first lane selected and unfolded.
func newPsModel(first Snapshot, target string) *psModel {
	m := &psModel{
		pane:     psPaneLanes,
		expanded: map[string]bool{},
		follow:   true,
		rings:    map[string]*psRing{},
		coarse:   map[string]*psRing{},
		snap:     first,
		target:   target,
	}
	m.absorbRings()
	m.clampTree()
	if m.selLane != "" {
		m.expanded[m.selLane] = true
	}
	m.clampTable()
	if first.StaleAge > 0 {
		m.warn = psUnreachableWarn + " · cached " + first.StaleAge.Truncate(time.Second).String() + " ago"
	}
	return m
}

// focus is the run id the poller should tail logs for -- the logs pane's
// current target. Derived, never stored.
func (m *psModel) focus() string { return m.logsTarget() }

// logsTarget resolves the logs pane's run: the pinned run while it still
// exists, else the newest running run under the tree selection, else the
// newest run under it, else "".
func (m *psModel) logsTarget() string {
	if m.pinnedRun != "" {
		if _, ok := findRun(m.snap, m.pinnedRun); ok {
			return m.pinnedRun
		}
	}
	inScope := func(r api.PsRun) bool {
		if m.selPipeline != "" {
			return r.Pipeline == m.selPipeline
		}
		return runLaneOf(r) == m.selLane
	}
	first := ""
	for _, r := range m.snap.Ps.Runs { // newest first as the wire orders them
		if !inScope(r) {
			continue
		}
		if r.State == "running" {
			return r.ID
		}
		if first == "" {
			first = r.ID
		}
	}
	return first
}

// detailPipeline resolves which pipeline the detail box charts: the selected
// pipeline row, or -- on a lane row -- the pipelines table's cursor.
func (m *psModel) detailPipeline() string {
	if m.selPipeline != "" {
		return m.selPipeline
	}
	return m.tblPipeline
}

// psLaneRow is one lane row of the rail and its metrics line.
type psLaneRow struct {
	name            string
	pipelines       int
	queued, running int
	load            *api.PsLoad // summed over the lane's running runs; nil renders dashes
}

// psPipelineRow is one row of the pipelines table.
type psPipelineRow struct {
	name            string
	latest          string // newest run's state, "-" when the pipeline never ran
	queued, running int
	load            *api.PsLoad
}

// psTreeRow is one visible row of the lanes rail: a lane row (pipeline == "")
// or a member pipeline row inside an unfolded lane.
type psTreeRow struct {
	lane     string
	pipeline string
}

// laneOf resolves the lane a listing row belongs to: its composer lane, or --
// mirroring dispatch.BuildWalk -- the pipeline itself as its own anonymous
// lane when no composer row names it.
func laneOf(p api.PipelineListItem) string {
	if p.Lane != "" {
		return p.Lane
	}
	return p.Name
}

// runLaneOf resolves the lane a run row belongs to, falling back to the run's
// own pipeline as its anonymous lane when the run carries none.
func runLaneOf(r api.PsRun) string {
	if r.Lane != "" {
		return r.Lane
	}
	return r.Pipeline
}

// sumLoad accumulates a run's sampled load into a lane or pipeline total,
// keeping nil -- dashes, never fabricated zeros -- until a real sample lands.
func sumLoad(total *api.PsLoad, l *api.PsLoad) *api.PsLoad {
	if l == nil {
		return total
	}
	if total == nil {
		total = &api.PsLoad{}
	}
	total.CPUPercent += l.CPUPercent
	total.RSSBytes += l.RSSBytes
	return total
}

// deriveLanes composes the rail's lane rows: one per lane, the union of the
// listing's lanes and the run rows' lanes (a run whose pipeline was since
// unregistered still shows), sorted by name.
func deriveLanes(s Snapshot) []psLaneRow {
	byName := map[string]*psLaneRow{}
	row := func(name string) *psLaneRow {
		r := byName[name]
		if r == nil {
			r = &psLaneRow{name: name}
			byName[name] = r
		}
		return r
	}
	for _, p := range s.Pipelines {
		row(laneOf(p)).pipelines++
	}
	for _, run := range s.Ps.Runs {
		r := row(runLaneOf(run))
		switch run.State {
		case "queued":
			r.queued++
		case "running":
			r.running++
			r.load = sumLoad(r.load, run.Load)
		}
	}
	out := make([]psLaneRow, 0, len(byName))
	for _, r := range byName {
		out = append(out, *r)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].name < out[j].name })
	return out
}

// derivePipelines composes one lane's pipeline rows: one per member pipeline,
// idle members included, each with its newest run's state and its running
// load. Runs of pipelines missing from the listing (unregistered since) still
// contribute rows so the counts add up.
func derivePipelines(s Snapshot, lane string) []psPipelineRow {
	byName := map[string]*psPipelineRow{}
	order := []string{}
	row := func(name string) *psPipelineRow {
		r := byName[name]
		if r == nil {
			r = &psPipelineRow{name: name, latest: "-"}
			byName[name] = r
			order = append(order, name)
		}
		return r
	}
	for _, p := range s.Pipelines {
		if laneOf(p) == lane {
			row(p.Name)
		}
	}
	// Runs are newest first: the first run seen per pipeline is its latest.
	for _, run := range s.Ps.Runs {
		if runLaneOf(run) != lane {
			continue
		}
		r := row(run.Pipeline)
		if r.latest == "-" {
			r.latest = run.State
		}
		switch run.State {
		case "queued":
			r.queued++
		case "running":
			r.running++
			r.load = sumLoad(r.load, run.Load)
		}
	}
	sort.Strings(order)
	out := make([]psPipelineRow, 0, len(order))
	for _, name := range order {
		out = append(out, *byName[name])
	}
	return out
}

// deriveRuns filters one pipeline's runs from the history snapshot: newest
// first as the wire orders them, queued and running only until the 'a' toggle
// widens to the whole history.
func deriveRuns(s Snapshot, pipeline string, all bool) []api.PsRun {
	var out []api.PsRun
	for _, run := range s.Ps.Runs {
		if run.Pipeline != pipeline {
			continue
		}
		if !all && run.State != "queued" && run.State != "running" {
			continue
		}
		out = append(out, run)
	}
	return out
}

// findRun resolves a run id in the snapshot, for the logs title and cancel.
func findRun(s Snapshot, id string) (api.PsRun, bool) {
	for _, run := range s.Ps.Runs {
		if run.ID == id {
			return run, true
		}
	}
	return api.PsRun{}, false
}

// treeRows enumerates the rail's visible rows in display order: every lane,
// and inside each unfolded lane its member pipelines.
func (m *psModel) treeRows() []psTreeRow {
	var out []psTreeRow
	for _, l := range deriveLanes(m.snap) {
		out = append(out, psTreeRow{lane: l.name})
		if m.expanded[l.name] {
			for _, p := range derivePipelines(m.snap, l.name) {
				out = append(out, psTreeRow{lane: l.name, pipeline: p.name})
			}
		}
	}
	return out
}

// absorb replaces the snapshot after a poll: grows the load rings, drops a
// log tail that belongs to a run other than the current target, and re-clamps
// the cursors so vanished rows never leave them dangling.
func (m *psModel) absorb(s Snapshot) {
	if s.LogsRun != "" && s.LogsRun != m.logsTargetIn(s) {
		s.Logs, s.LogsRun = nil, ""
	}
	m.snap = s
	m.absorbRings()
	m.clampTree()
	m.clampTable()
	if m.search != nil {
		m.search.rematch(m.snap)
	}
}

// logsTargetIn resolves the logs target against a candidate snapshot, so
// absorb can judge an arriving tail before committing the snapshot.
func (m *psModel) logsTargetIn(s Snapshot) string {
	held := m.snap
	m.snap = s
	defer func() { m.snap = held }()
	return m.logsTarget()
}

// absorbRings advances the load rings for one absorbed snapshot. A snapshot
// carrying the daemon's recorded history re-seeds every ring from it (the
// view-open backfill, and the periodic refresh that keeps the coarse rings
// current). Otherwise the payload's sample_tick decides: unchanged means the
// collector has not sampled since the last poll, so nothing is pushed (the 1s
// poll outpaces the collector's tick on purpose); an advanced tick pushes one
// slot, with any missed ticks filled absent first so the time axis stays
// honest. A tick-less payload (a daemon without a collector) falls back to one
// push per absorb, the pre-history behavior.
func (m *psModel) absorbRings() {
	if h := m.snap.Ps.History; h != nil {
		m.reseedRings(h)
		m.lastTick = m.snap.Ps.SampleTick
		return
	}
	if tick := m.snap.Ps.SampleTick; tick != 0 {
		if tick < m.lastTick {
			// The collector's counter went backwards: a restarted (or different)
			// daemon answers now -- the reconnect case. Reset the gate so its
			// samples land instead of being skipped until the counter catches up.
			m.lastTick = 0
		}
		if tick <= m.lastTick {
			return
		}
		if m.lastTick != 0 {
			gaps := tick - m.lastTick - 1
			if gaps > psRingCap {
				gaps = psRingCap // deeper gaps fall off the ring anyway
			}
			for range gaps {
				m.pushRingsAbsent()
			}
		}
		m.lastTick = tick
	}
	m.pushRings()
}

// reseedRings replaces every ring with the daemon's recorded history: the fine
// series trimmed to the client cap, the coarse series whole. The wire keys
// ("engine", "lane:<name>", "pipeline:<name>") map onto the ring keys ("",
// "l:<name>", "p:<name>"); an unrecognized key is skipped, never guessed.
func (m *psModel) reseedRings(h *api.PsHistory) {
	m.rings = map[string]*psRing{}
	m.coarse = map[string]*psRing{}
	for _, s := range h.Series {
		key, ok := ringKeyFor(s.Key)
		if !ok {
			continue
		}
		fine := &psRing{cpu: append([]float64(nil), s.CPU...), mem: append([]int64(nil), s.RSS...)}
		if len(fine.cpu) > psRingCap {
			fine.cpu = fine.cpu[len(fine.cpu)-psRingCap:]
			fine.mem = fine.mem[len(fine.mem)-psRingCap:]
		}
		m.rings[key] = fine
		m.coarse[key] = &psRing{cpu: append([]float64(nil), s.CoarseCPU...), mem: append([]int64(nil), s.CoarseRSS...)}
	}
}

// ringKeyFor maps a wire history series key onto the model's ring key.
func ringKeyFor(wire string) (string, bool) {
	switch {
	case wire == "engine":
		return "", true
	case strings.HasPrefix(wire, "lane:"):
		return "l:" + strings.TrimPrefix(wire, "lane:"), true
	case strings.HasPrefix(wire, "pipeline:"):
		return "p:" + strings.TrimPrefix(wire, "pipeline:"), true
	}
	return "", false
}

// pushRingsAbsent pushes one absent slot into every existing ring: a collector
// tick the poller missed carries no knowable sample.
func (m *psModel) pushRingsAbsent() {
	for _, r := range m.rings {
		r.push(psNoSample, 0)
	}
}

// pushRings pushes one tick of load history for the engine, every lane, and
// every pipeline seen in the snapshot. Entities without a sample this tick
// push psNoSample so their strips show absence, not zero.
func (m *psModel) pushRings() {
	ring := func(key string) *psRing {
		r := m.rings[key]
		if r == nil {
			r = &psRing{}
			m.rings[key] = r
		}
		return r
	}
	if l := m.snap.Ps.Engine.Load; l != nil {
		ring("").push(l.CPUPercent, l.RSSBytes)
	} else {
		ring("").push(psNoSample, 0)
	}
	for _, lane := range deriveLanes(m.snap) {
		r := ring("l:" + lane.name)
		if lane.load != nil {
			r.push(lane.load.CPUPercent, lane.load.RSSBytes)
		} else {
			r.push(psNoSample, 0)
		}
	}
	perPipe := map[string]*api.PsLoad{}
	seen := map[string]bool{}
	for _, run := range m.snap.Ps.Runs {
		seen[run.Pipeline] = true
		if run.State == "running" {
			perPipe[run.Pipeline] = sumLoad(perPipe[run.Pipeline], run.Load)
		}
	}
	for _, p := range m.snap.Pipelines {
		seen[p.Name] = true
	}
	for name := range seen {
		r := ring("p:" + name)
		if l := perPipe[name]; l != nil {
			r.push(l.CPUPercent, l.RSSBytes)
		} else {
			r.push(psNoSample, 0)
		}
	}
}

// clampTree snaps the rail cursor to a live row: the selected pipeline within
// its lane when both survive, else the selected lane, else the first lane.
func (m *psModel) clampTree() {
	lanes := deriveLanes(m.snap)
	if len(lanes) == 0 {
		m.selLane, m.selPipeline = "", ""
		return
	}
	laneAlive := false
	for _, l := range lanes {
		if l.name == m.selLane {
			laneAlive = true
			break
		}
	}
	if !laneAlive {
		m.selLane, m.selPipeline = lanes[0].name, ""
		return
	}
	if m.selPipeline != "" {
		for _, p := range derivePipelines(m.snap, m.selLane) {
			if p.name == m.selPipeline {
				return
			}
		}
		m.selPipeline = ""
	}
}

// clampTable snaps the table cursor to a live row for the current context.
func (m *psModel) clampTable() {
	if m.selPipeline != "" {
		m.tblRun = clampKey(m.tblRun, m.runKeys())
		return
	}
	m.tblPipeline = clampKey(m.tblPipeline, m.pipelineKeys())
}

// pipelineKeys lists the pipelines table's row identities in display order.
func (m *psModel) pipelineKeys() []string {
	rows := derivePipelines(m.snap, m.selLane)
	keys := make([]string, len(rows))
	for i, r := range rows {
		keys[i] = r.name
	}
	return keys
}

// runKeys lists the runs table's row identities in display order.
func (m *psModel) runKeys() []string {
	runs := deriveRuns(m.snap, m.selPipeline, m.showAll)
	keys := make([]string, len(runs))
	for i, r := range runs {
		keys[i] = r.ID
	}
	return keys
}

// clampKey keeps sel when it still names a row, else the first row, else "".
func clampKey(sel string, keys []string) string {
	if len(keys) == 0 {
		return ""
	}
	for _, k := range keys {
		if k == sel {
			return sel
		}
	}
	return keys[0]
}

// moveSel moves a cursor delta rows within keys, clamped to the ends.
func moveSel(sel string, keys []string, delta int) string {
	if len(keys) == 0 {
		return ""
	}
	at := 0
	for i, k := range keys {
		if k == sel {
			at = i
			break
		}
	}
	at += delta
	if at < 0 {
		at = 0
	}
	if at >= len(keys) {
		at = len(keys) - 1
	}
	return keys[at]
}

// update advances the model by one keypress and returns the run the loop
// should ask the poller to cancel ("" almost always). The loop reads its two
// other signals off the model after the call: m.quit to exit, and m.focus()
// to re-point the poller's log tail when it changed.
func (m *psModel) update(k psKey) (cancelRun string) {
	m.note = ""

	// The search overlay owns the keyboard while open (Esc lives only here).
	if m.search != nil {
		m.updateSearch(k)
		return ""
	}

	// The ':' command prompt owns the keyboard while open (#218).
	if m.command != nil {
		m.updateCommand(k)
		return ""
	}

	// The catalog overlay owns the keyboard while open (#219).
	if m.catalog != nil {
		m.updateCatalog(k)
		return ""
	}

	// An armed cancel confirm consumes the next key: y confirms, all else disarms.
	if m.confirmCancel {
		m.confirmCancel = false
		if k.kind == psKeyRune && (k.r == 'y' || k.r == 'Y') {
			return m.logsTarget()
		}
		if k.kind == psKeyCtrlC {
			m.quit = true
		}
		return ""
	}

	switch k.kind {
	case psKeyCtrlC:
		m.quit = true
	case psKeyTab:
		m.cyclePane()
	case psKeyRune:
		m.updateRune(k.r)
	case psKeyUp:
		m.move(-1)
	case psKeyDown:
		m.move(1)
	case psKeyEnter, psKeyRight:
		m.enter()
	case psKeyLeft:
		m.back()
	}
	return ""
}

// cyclePane advances the pane focus: lanes, table, logs, around.
func (m *psModel) cyclePane() {
	switch m.pane {
	case psPaneLanes:
		m.pane = psPaneTable
	case psPaneTable:
		m.pane = psPaneLogs
	default:
		m.pane = psPaneLanes
	}
}

// updateRune routes a printable keypress outside the overlay.
func (m *psModel) updateRune(r rune) {
	switch r {
	case 'q', 'Q':
		m.quit = true
	case 'j':
		m.move(1)
	case 'k':
		m.move(-1)
	case '/':
		m.openSearch()
	case ':':
		m.openCommand()
	case '?':
		m.openCommandHelp()
	case 'a':
		if m.pane == psPaneTable && m.selPipeline != "" {
			m.showAll = !m.showAll
			m.tblRun = clampKey(m.tblRun, m.runKeys())
		}
	case 'f':
		if m.pane == psPaneLogs {
			m.follow = !m.follow
			m.scroll = 0
		}
	case 'h':
		m.histView = !m.histView
	case 'p', 'P':
		// Freeze the live display so the terminal can select and copy text
		// without the next poll wiping the highlight.
		m.frozen = !m.frozen
		if m.frozen {
			m.note = "frozen · select text to copy · p resumes"
		} else {
			m.note = "live"
		}
	case 'c':
		// Quiet engine: c is the one-key jump into the catalog (the empty
		// card's primary action). With work registered it stays cancel-in-logs.
		if psIsEmptyWorkspace(m) {
			m.openCatalog()
			return
		}
		if m.pane == psPaneLogs {
			if run, ok := findRun(m.snap, m.logsTarget()); ok && run.State == "running" {
				m.confirmCancel = true
			}
		}
	}
}

// move shifts the focused pane's cursor; in a non-following logs pane it
// scrolls the tail instead.
func (m *psModel) move(delta int) {
	switch m.pane {
	case psPaneLanes:
		m.moveTree(delta)
	case psPaneTable:
		if m.selPipeline != "" {
			m.tblRun = moveSel(m.tblRun, m.runKeys(), delta)
		} else {
			m.tblPipeline = moveSel(m.tblPipeline, m.pipelineKeys(), delta)
		}
	case psPaneLogs:
		if !m.follow {
			m.scroll -= delta // up (-1) scrolls further back from the tail
			m.clampScroll(len(m.snap.Logs))
		}
	}
}

// moveTree walks the rail cursor over the visible tree rows.
func (m *psModel) moveTree(delta int) {
	rows := m.treeRows()
	if len(rows) == 0 {
		return
	}
	at := 0
	for i, r := range rows {
		if r.lane == m.selLane && r.pipeline == m.selPipeline {
			at = i
			break
		}
	}
	at += delta
	if at < 0 {
		at = 0
	}
	if at >= len(rows) {
		at = len(rows) - 1
	}
	m.selectTree(rows[at])
}

// selectTree lands the rail cursor on a row, resetting the per-selection
// state that follows it: table cursors, the pinned run, the runs toggle.
func (m *psModel) selectTree(row psTreeRow) {
	if row.lane == m.selLane && row.pipeline == m.selPipeline {
		return
	}
	m.selLane, m.selPipeline = row.lane, row.pipeline
	m.pinnedRun = ""
	m.showAll = false
	m.scroll = 0
	m.follow = true
	m.clampTable()
}

// clampScroll keeps the scrollback offset within the held tail: at the far
// end the first line stays on screen (never a blank pane past the top).
func (m *psModel) clampScroll(lines int) {
	if m.scroll < 0 {
		m.scroll = 0
	}
	if top := lines - 1; top >= 0 && m.scroll > top {
		m.scroll = top
	}
}

// enter acts on the focused pane's selection (Enter / right arrow): a lane
// row unfolds (or folds back), a pipeline row in the rail or table selects it,
// a run row pins the logs target.
func (m *psModel) enter() {
	switch m.pane {
	case psPaneLanes:
		if m.selPipeline == "" {
			if m.selLane != "" {
				m.expanded[m.selLane] = !m.expanded[m.selLane]
			}
			return
		}
		// A pipeline row: focus the table on its runs.
		m.pane = psPaneTable
		m.clampTable()
	case psPaneTable:
		if m.selPipeline == "" {
			if m.tblPipeline == "" {
				return
			}
			// Drill: the tree selection descends to the pipeline row.
			m.expanded[m.selLane] = true
			m.selectTree(psTreeRow{lane: m.selLane, pipeline: m.tblPipeline})
			return
		}
		if m.tblRun != "" {
			m.pinnedRun = m.tblRun
			m.follow = true
			m.scroll = 0
		}
	}
}

// back retreats the focused pane's selection (left arrow): a pipeline row
// climbs to its lane row, an unfolded lane folds, a runs table returns to the
// pipelines table, a pinned logs target unpins.
func (m *psModel) back() {
	switch m.pane {
	case psPaneLanes:
		if m.selPipeline != "" {
			m.selectTree(psTreeRow{lane: m.selLane})
			return
		}
		if m.selLane != "" {
			m.expanded[m.selLane] = false
		}
	case psPaneTable:
		if m.selPipeline != "" {
			m.selectTree(psTreeRow{lane: m.selLane})
			return
		}
	case psPaneLogs:
		if m.pinnedRun != "" {
			m.pinnedRun = ""
			m.follow = true
			m.scroll = 0
		}
	}
}
