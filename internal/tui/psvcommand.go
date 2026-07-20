package tui

import (
	"fmt"
	"strings"

	"github.com/MateusAMP2119/iris-lakehouse/internal/api"
)

// This file is the `iris ps` command palette (#218, reworked): a dedicated
// COMMANDS overlay — list + detail + prompt — beside the '/' telescope search.
// The closed roster stays small; each entry carries usage, description, and
// category so the right pane is a living cheat-sheet, not a vim empire.

// psCmdCategory groups palette rows for the section headers.
type psCmdCategory string

const (
	psCmdNav    psCmdCategory = "navigate"
	psCmdWatch  psCmdCategory = "watch"
	psCmdAction psCmdCategory = "action"
	psCmdMeta   psCmdCategory = "meta"
)

// psCmdSpec is one registered palette command: the name the user types, its
// usage line, a one-sentence description, and optional key chords that do the
// same thing outside the palette.
type psCmdSpec struct {
	name        string
	usage       string
	summary     string
	detail      string
	category    psCmdCategory
	keys        string // display-only chords, e.g. "/  :"
	needsArg    bool   // true when Enter on a bare name parks usage, not dispatch
	argHint     string // completion mode after "name "
}

// psCommandRoster is the closed, stable-order command set the palette lists.
// Order is the tab-cycle order and the default list order.
var psCommandRoster = []psCmdSpec{
	{
		name: "catalog", usage: ":catalog", summary: "Browse and install pipeline packs",
		detail: "Opens the catalog overlay over the dashboard: pack list on the left, README and tree on the right. Install and apply without leaving iris ps.",
		category: psCmdNav, keys: ":catalog",
	},
	{
		name: "logs", usage: ":logs <run>", summary: "Pin the logs pane on a run",
		detail: "Selects the run's lane and pipeline, pins the log tail, and focuses the LOGS pane. Tab after `:logs ` cycles run ids from the current snapshot.",
		category: psCmdWatch, keys: "⏎ on a run", needsArg: true, argHint: "run",
	},
	{
		name: "search", usage: ":search [query]", summary: "Fuzzy-find lanes, pipelines, runs",
		detail: "Opens the telescope search overlay. An optional query is applied immediately. Outside the palette, press /.",
		category: psCmdNav, keys: "/",
	},
	{
		name: "all", usage: ":all", summary: "Toggle full run history in the table",
		detail: "When a pipeline's runs table is open, flip between live (queued + running) and the whole history. Same as the a key in that pane.",
		category: psCmdWatch, keys: "a",
	},
	{
		name: "follow", usage: ":follow", summary: "Toggle log-tail follow mode",
		detail: "When following, the logs pane sticks to the newest lines. When paused, j/k scroll the buffer. Same as the f key in the logs pane.",
		category: psCmdWatch, keys: "f",
	},
	{
		name: "history", usage: ":history", summary: "Toggle hours-deep load strips",
		detail: "Swaps every heat strip between the live fine ring and the coarse per-bucket history the daemon keeps. Same as the h key.",
		category: psCmdWatch, keys: "h",
	},
	{
		name: "cancel", usage: ":cancel", summary: "Cancel the watched running run",
		detail: "Arms a y/N confirm for the run the logs pane is watching, when that run is still running. Same as the c key in the logs pane.",
		category: psCmdAction, keys: "c",
	},
	{
		name: "help", usage: ":help", summary: "Keyboard reference",
		detail: "Highlights this entry and parks the key map in the detail pane. Scroll the list for every command's chords.",
		category: psCmdMeta, keys: "?",
	},
	{
		name: "q", usage: ":q", summary: "Quit iris ps",
		detail: "Leaves the live view and restores the terminal. Same as q or Ctrl-C.",
		category: psCmdMeta, keys: "q  Ctrl-C",
	},
}

// psCommands is the closed name roster tab cycles through (stable order).
var psCommands []string

func init() {
	psCommands = make([]string, len(psCommandRoster))
	for i, c := range psCommandRoster {
		psCommands[i] = c.name
	}
}

// psCommand is the open palette's state: typed input, filtered list selection,
// an inline error, and tab-completion cursor.
type psCommand struct {
	input   []rune
	err     string
	sel     int      // index into filtered()
	cycling bool     // a tab cycle is live; any edit ends it
	base    string   // the input captured when the cycle started
	comp    int      // next completion index
	browse  bool     // opened via '?' — start focused on help, empty input ok
}

// openCommand opens the ':' palette with an empty prompt.
func (m *psModel) openCommand() {
	m.command = &psCommand{}
	m.command.syncSel()
}

// openCommandHelp opens the palette focused on the help entry (the ? key).
func (m *psModel) openCommandHelp() {
	m.command = &psCommand{browse: true}
	// Land the selection on "help" so the detail pane is the key map.
	for i, c := range m.command.filtered() {
		if c.name == "help" {
			m.command.sel = i
			return
		}
	}
	m.command.syncSel()
}

// updateCommand routes a keypress while the palette is open: typing filters,
// arrows move the list, tab cycles completions, Enter dispatches, Esc closes.
func (m *psModel) updateCommand(k psKey) {
	c := m.command
	switch k.kind {
	case psKeyCtrlC:
		m.quit = true
	case psKeyEsc:
		m.command = nil
	case psKeyRune:
		// In browse mode, j/k move the list like the search overlay's arrows
		// would — only when the prompt is still empty so typed filters win.
		if c.browse && len(c.input) == 0 && (k.r == 'j' || k.r == 'k') {
			if k.r == 'j' {
				c.moveSel(1)
			} else {
				c.moveSel(-1)
			}
			return
		}
		c.input = append(c.input, k.r)
		c.err, c.cycling, c.browse = "", false, false
		c.syncSel()
	case psKeyBackspace:
		if len(c.input) == 0 {
			m.command = nil
			return
		}
		c.input = c.input[:len(c.input)-1]
		c.err, c.cycling, c.browse = "", false, false
		c.syncSel()
	case psKeyUp:
		m.moveCommandSel(-1)
	case psKeyDown:
		m.moveCommandSel(1)
	case psKeyTab:
		m.completeCommand()
	case psKeyEnter:
		m.runCommand(strings.TrimSpace(string(c.input)))
	}
}

// moveCommandSel moves the palette cursor: over the filtered roster normally,
// over matching run ids while the prompt is in `:logs <prefix>` mode.
func (m *psModel) moveCommandSel(delta int) {
	c := m.command
	line := string(c.input)
	if name, _, hasArg := strings.Cut(line, " "); hasArg && name == "logs" {
		runs := filteredRuns(line, m.snap)
		if len(runs) == 0 {
			return
		}
		c.sel += delta
		if c.sel < 0 {
			c.sel = len(runs) - 1
		}
		if c.sel >= len(runs) {
			c.sel = 0
		}
		// Mirror the selection into the prompt so Enter dispatches that run.
		c.input = []rune("logs " + runs[c.sel])
		c.err, c.cycling = "", false
		return
	}
	c.moveSel(delta)
}

// moveSel shifts the filtered-list cursor, wrapping at the ends.
func (c *psCommand) moveSel(delta int) {
	list := c.filtered()
	if len(list) == 0 {
		c.sel = 0
		return
	}
	c.sel += delta
	if c.sel < 0 {
		c.sel = len(list) - 1
	}
	if c.sel >= len(list) {
		c.sel = 0
	}
	c.err = ""
}

// syncSel clamps the selection after a filter change and prefers a name that
// still prefixes the typed head when possible.
func (c *psCommand) syncSel() {
	list := c.filtered()
	if len(list) == 0 {
		c.sel = 0
		return
	}
	head := strings.TrimSpace(string(c.input))
	if name, _, ok := strings.Cut(head, " "); ok {
		head = name
	}
	if head != "" {
		for i, spec := range list {
			if strings.HasPrefix(spec.name, head) {
				c.sel = i
				return
			}
		}
	}
	if c.sel >= len(list) {
		c.sel = len(list) - 1
	}
}

// filtered returns the roster rows matching the typed command name prefix.
// After "logs " the list becomes run-id completions from the snapshot held by
// the model at render/dispatch time — filtered() itself is pure over input.
func (c *psCommand) filtered() []psCmdSpec {
	line := string(c.input)
	if name, arg, hasArg := strings.Cut(line, " "); hasArg {
		// Argument mode: only the command being completed is "active".
		name = strings.TrimSpace(name)
		for _, spec := range psCommandRoster {
			if spec.name == name {
				// Synthetic rows for run completions are built by filteredRuns.
				if spec.argHint == "run" {
					return []psCmdSpec{spec} // detail stays on the parent command
				}
				return []psCmdSpec{spec}
			}
		}
		_ = arg
		return nil
	}
	prefix := strings.TrimSpace(line)
	var out []psCmdSpec
	for _, spec := range psCommandRoster {
		if prefix == "" || strings.HasPrefix(spec.name, prefix) {
			out = append(out, spec)
		}
	}
	return out
}

// filteredRuns lists run-id rows for the logs argument, used by the renderer
// when the input is in `:logs <prefix>` mode.
func filteredRuns(base string, snap Snapshot) []string {
	name, arg, hasArg := strings.Cut(base, " ")
	if !hasArg || name != "logs" {
		return nil
	}
	arg = strings.TrimSpace(arg)
	var out []string
	for _, r := range snap.Ps.Runs {
		if strings.HasPrefix(r.ID, arg) {
			out = append(out, r.ID)
		}
	}
	return out
}

// selected returns the currently highlighted roster entry, if any.
func (c *psCommand) selected() (psCmdSpec, bool) {
	list := c.filtered()
	if len(list) == 0 || c.sel < 0 || c.sel >= len(list) {
		return psCmdSpec{}, false
	}
	return list[c.sel], true
}

// runCommand dispatches one typed command; an unknown one answers inline and
// never tears the view down. An empty line on a selected no-arg command runs
// the selection (browse mode / arrow-then-enter).
func (m *psModel) runCommand(line string) {
	c := m.command
	if line == "" {
		if c.browse {
			// Empty enter in help browse just keeps the palette open on help.
			return
		}
		if spec, ok := c.selected(); ok && !spec.needsArg {
			line = spec.name
		} else if spec, ok := c.selected(); ok && spec.needsArg {
			// Park the selected command name so the user can type the arg.
			c.input = []rune(spec.name + " ")
			c.err, c.cycling = "", false
			c.syncSel()
			return
		} else {
			m.command = nil
			return
		}
	}
	name, arg, _ := strings.Cut(line, " ")
	arg = strings.TrimSpace(arg)
	switch name {
	case "q":
		m.quit = true
	case "logs":
		if arg == "" {
			m.commandErr("usage: :logs <run>")
			return
		}
		run, ok := findRun(m.snap, arg)
		if !ok {
			m.commandErr("no run " + arg + " in the current snapshot")
			return
		}
		m.expanded[runLaneOf(run)] = true
		m.selectTree(psTreeRow{lane: runLaneOf(run), pipeline: run.Pipeline})
		m.tblRun = run.ID
		m.pinnedRun = run.ID
		m.pane = psPaneLogs
		m.command = nil
	case "catalog":
		m.command = nil
		m.openCatalog()
	case "search":
		m.command = nil
		m.openSearch()
		if arg != "" {
			m.search.query = []rune(arg)
			m.search.rematch(m.snap)
		}
	case "all":
		if m.selPipeline == "" {
			m.commandErr("open a pipeline's runs first (⏎ on a pipeline)")
			return
		}
		m.showAll = !m.showAll
		m.tblRun = clampKey(m.tblRun, m.runKeys())
		m.pane = psPaneTable
		m.command = nil
		if m.showAll {
			m.note = "showing full run history"
		} else {
			m.note = "showing live runs only"
		}
	case "follow":
		m.follow = !m.follow
		m.scroll = 0
		m.pane = psPaneLogs
		m.command = nil
		if m.follow {
			m.note = "log follow on"
		} else {
			m.note = "log follow off"
		}
	case "history":
		m.histView = !m.histView
		m.command = nil
		if m.histView {
			m.note = "load strips: hours-deep history"
		} else {
			m.note = "load strips: live"
		}
	case "cancel":
		run, ok := findRun(m.snap, m.logsTarget())
		if !ok || run.State != "running" {
			m.commandErr("no running run under the current selection")
			return
		}
		m.pane = psPaneLogs
		m.confirmCancel = true
		m.command = nil
	case "help":
		// Stay open; land on the help row so the detail pane is the key map.
		c.input = nil
		c.err, c.cycling, c.browse = "", false, true
		for i, spec := range c.filtered() {
			if spec.name == "help" {
				c.sel = i
				return
			}
		}
		c.syncSel()
	default:
		// If the typed head is a prefix of exactly one command, accept it.
		if matches := c.filtered(); len(matches) == 1 && !strings.Contains(line, " ") {
			spec := matches[0]
			if spec.needsArg {
				c.input = []rune(spec.name + " ")
				c.err, c.cycling = "", false
				c.syncSel()
				return
			}
			m.runCommand(spec.name)
			return
		}
		m.commandErr("unknown command :" + name)
	}
}

// commandErr parks an inline error on the open prompt row.
func (m *psModel) commandErr(msg string) {
	m.command.err = msg
	m.command.cycling = false
}

// completeCommand cycles tab completion over the base input: command names on
// a bare prompt, run ids after "logs ".
func (m *psModel) completeCommand() {
	c := m.command
	if !c.cycling {
		c.base, c.comp, c.cycling = string(c.input), 0, true
	}
	cands := commandCompletions(c.base, m.snap)
	if len(cands) == 0 {
		c.cycling = false
		return
	}
	c.input = []rune(cands[c.comp%len(cands)])
	c.comp++
	c.err = ""
	c.syncSel()
}

// commandCompletions lists the completions for a prompt prefix, in stable order.
func commandCompletions(base string, snap Snapshot) []string {
	if name, arg, hasArg := strings.Cut(base, " "); hasArg {
		if name != "logs" {
			return nil
		}
		arg = strings.TrimSpace(arg)
		var out []string
		for _, r := range snap.Ps.Runs {
			if strings.HasPrefix(r.ID, arg) {
				out = append(out, "logs "+r.ID)
			}
		}
		return out
	}
	var out []string
	for _, cmd := range psCommands {
		if strings.HasPrefix(cmd, base) {
			out = append(out, cmd)
		}
	}
	return out
}

// commandDetailBody is the right-pane text for the selected command (or the
// global key map when help is selected / browse mode).
func commandDetailBody(spec psCmdSpec, width int) []string {
	if width < 8 {
		width = 8
	}
	var lines []string
	if spec.name == "help" {
		lines = append(lines, wrapWords("Keyboard reference for iris ps. Select a command on the left for its detail, or type : to filter.", width)...)
		lines = append(lines, "")
		lines = append(lines, "GLOBAL")
		lines = append(lines, "  tab        cycle panes")
		lines = append(lines, "  ↑↓ j/k     move")
		lines = append(lines, "  ⏎ →        unfold / drill")
		lines = append(lines, "  ←          ascend")
		lines = append(lines, "  /          search")
		lines = append(lines, "  :          commands")
		lines = append(lines, "  ?          this help")
		lines = append(lines, "  p          freeze (select & copy)")
		lines = append(lines, "  h          history strips")
		lines = append(lines, "  q          quit")
		lines = append(lines, "")
		lines = append(lines, "TABLE")
		lines = append(lines, "  a          all / live runs")
		lines = append(lines, "")
		lines = append(lines, "LOGS")
		lines = append(lines, "  f          follow on/off")
		lines = append(lines, "  c          cancel run")
		return lines
	}
	lines = append(lines, wrapWords(spec.summary, width)...)
	lines = append(lines, "")
	if spec.detail != "" {
		lines = append(lines, wrapWords(spec.detail, width)...)
		lines = append(lines, "")
	}
	lines = append(lines, "Usage  "+spec.usage)
	if spec.keys != "" {
		lines = append(lines, "Keys   "+spec.keys)
	}
	lines = append(lines, "Group  "+string(spec.category))
	return lines
}

// wrapWords soft-wraps s to width runes on word boundaries.
func wrapWords(s string, width int) []string {
	if width <= 0 {
		return []string{s}
	}
	words := strings.Fields(s)
	if len(words) == 0 {
		return nil
	}
	var lines []string
	var cur string
	for _, w := range words {
		if cur == "" {
			cur = w
			continue
		}
		if len([]rune(cur))+1+len([]rune(w)) <= width {
			cur += " " + w
			continue
		}
		lines = append(lines, cur)
		cur = w
	}
	if cur != "" {
		lines = append(lines, cur)
	}
	return lines
}

// commandListLabel is one left-pane row: marker + name + · summary clip.
func commandListLabel(spec psCmdSpec, selected bool, width int) string {
	marker := "  "
	if selected {
		marker = "▸ "
	}
	name := spec.name
	// "▸ name · summary…" — keep the name intact, clip only the summary.
	prefix := marker + name
	rest := width - len([]rune(prefix))
	if rest < 4 {
		return clipCells(prefix, width)
	}
	sum := " · " + spec.summary
	if len([]rune(sum)) > rest {
		sum = string([]rune(sum)[:rest-1]) + "…"
	}
	return prefix + sum
}

// commandRunRowLabel labels a run completion row in argument mode.
func commandRunRowLabel(run api.PsRun, selected bool, width int) string {
	marker := "  "
	if selected {
		marker = "▸ "
	}
	label := fmt.Sprintf("%s%-6s %s  %s", marker, run.ID, run.State, run.Pipeline)
	return clipCells(label, width)
}
