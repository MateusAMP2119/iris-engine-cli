package cli

import "strings"

// This file is the `iris ps` ':' command mode (#218): a one-line prompt on the
// footer row whose commands dispatch through the same single-writer event loop
// as every other key. The grammar stays small (:catalog, :logs <run>, :q); the
// palette is the extension point, not a vim empire.

// psCommands is the closed command roster tab cycles through.
var psCommands = []string{"catalog", "logs", "q"}

// psCommand is the open prompt's state: typed input, an inline error, and the tab-completion cursor.
type psCommand struct {
	input   []rune
	err     string
	cycling bool   // a tab cycle is live; any edit ends it
	base    string // the input captured when the cycle started
	comp    int    // next completion index
}

// openCommand opens the ':' prompt.
func (m *psModel) openCommand() { m.command = &psCommand{} }

// updateCommand routes a keypress while the prompt is open: typing edits, tab
// cycles completions, Enter dispatches, Esc (or backspace on empty) closes.
func (m *psModel) updateCommand(k psKey) {
	c := m.command
	switch k.kind {
	case psKeyCtrlC:
		m.quit = true
	case psKeyEsc:
		m.command = nil
	case psKeyRune:
		c.input = append(c.input, k.r)
		c.err, c.cycling = "", false
	case psKeyBackspace:
		if len(c.input) == 0 {
			m.command = nil
			return
		}
		c.input = c.input[:len(c.input)-1]
		c.err, c.cycling = "", false
	case psKeyTab:
		m.completeCommand()
	case psKeyEnter:
		m.runCommand(strings.TrimSpace(string(c.input)))
	}
}

// runCommand dispatches one typed command; an unknown one answers inline and never tears the view down.
func (m *psModel) runCommand(line string) {
	if line == "" {
		m.command = nil
		return
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
		m.commandErr("catalog overlay not built yet") // stage gate: the overlay lands with #219
	default:
		m.commandErr("unknown command :" + name)
	}
}

// commandErr parks an inline error on the open prompt row.
func (m *psModel) commandErr(msg string) {
	m.command.err = msg
	m.command.cycling = false
}

// completeCommand cycles tab completion over the base input: command names on a bare prompt, run ids after "logs ".
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
}

// commandCompletions lists the completions for a prompt prefix, in stable order.
func commandCompletions(base string, snap psSnapshot) []string {
	if name, arg, hasArg := strings.Cut(base, " "); hasArg {
		if name != "logs" {
			return nil
		}
		arg = strings.TrimSpace(arg)
		var out []string
		for _, r := range snap.ps.Runs {
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
