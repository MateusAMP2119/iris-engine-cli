package tui

import (
	"context"
	"io"
	"os"

	"github.com/charmbracelet/bubbles/help"
	bkey "github.com/charmbracelet/bubbles/key"
	"github.com/charmbracelet/bubbles/textinput"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
)

// teaMsg wrappers bridge poller/catalog/note channels into Bubble Tea.
type (
	teaPollMsg    psPollMsg
	teaNoteMsg    string
	teaCatalogMsg psCatalogMsg
)

// psKeyMap is the bubbles/help binding table for the live view. The pure
// model still owns behavior; this is the discoverable surface `?` and the
// help bubble describe.
type psKeyMap struct {
	Up      bkey.Binding
	Down    bkey.Binding
	Enter   bkey.Binding
	Back    bkey.Binding
	Tab     bkey.Binding
	Search  bkey.Binding
	Command bkey.Binding
	Help    bkey.Binding
	Quit    bkey.Binding
	All     bkey.Binding
	Follow  bkey.Binding
	History bkey.Binding
	Cancel  bkey.Binding
}

func newPsKeyMap() psKeyMap {
	return psKeyMap{
		Up:      bkey.NewBinding(bkey.WithKeys("up", "k"), bkey.WithHelp("↑/k", "move up")),
		Down:    bkey.NewBinding(bkey.WithKeys("down", "j"), bkey.WithHelp("↓/j", "move down")),
		Enter:   bkey.NewBinding(bkey.WithKeys("enter", "right"), bkey.WithHelp("⏎/→", "unfold / drill")),
		Back:    bkey.NewBinding(bkey.WithKeys("left"), bkey.WithHelp("←", "ascend")),
		Tab:     bkey.NewBinding(bkey.WithKeys("tab"), bkey.WithHelp("tab", "cycle panes")),
		Search:  bkey.NewBinding(bkey.WithKeys("/"), bkey.WithHelp("/", "search")),
		Command: bkey.NewBinding(bkey.WithKeys(":"), bkey.WithHelp(":", "commands")),
		Help:    bkey.NewBinding(bkey.WithKeys("?"), bkey.WithHelp("?", "help")),
		Quit:    bkey.NewBinding(bkey.WithKeys("q", "ctrl+c"), bkey.WithHelp("q", "quit")),
		All:     bkey.NewBinding(bkey.WithKeys("a"), bkey.WithHelp("a", "all / live runs")),
		Follow:  bkey.NewBinding(bkey.WithKeys("f"), bkey.WithHelp("f", "follow logs")),
		History: bkey.NewBinding(bkey.WithKeys("h"), bkey.WithHelp("h", "history strips")),
		Cancel:  bkey.NewBinding(bkey.WithKeys("c"), bkey.WithHelp("c", "cancel run")),
	}
}

// ShortHelp implements help.KeyMap for the compact footer strip.
func (k psKeyMap) ShortHelp() []bkey.Binding {
	return []bkey.Binding{k.Tab, k.Up, k.Down, k.Enter, k.Search, k.Command, k.Help, k.Quit}
}

// FullHelp implements help.KeyMap for the expanded help columns.
func (k psKeyMap) FullHelp() [][]bkey.Binding {
	return [][]bkey.Binding{
		{k.Up, k.Down, k.Enter, k.Back, k.Tab},
		{k.Search, k.Command, k.Help, k.History},
		{k.All, k.Follow, k.Cancel, k.Quit},
	}
}

// teaProgram is the Bubble Tea model for the live `iris ps` view. It owns the
// pure psModel state machine and renders through the existing frame buffer so
// goldens and key behavior stay stable while the event loop rides tea.Program.
//
// Charm pieces layered on top:
//   - bubbles/textinput  drives the COMMANDS palette prompt (synced both ways)
//   - bubbles/help       documents the key map (full help via the ? palette)
//   - lipgloss           styles the palette chrome tokens used by View helpers
type teaProgram struct {
	m *psModel
	p painter
	w int
	h int

	keys     psKeyMap
	help     help.Model
	cmdInput textinput.Model

	focusCh    chan<- string
	cancelCh   chan<- string
	runCatalog func(psCatalogReq)
	sentFocus  string
	err        error
	quitting   bool
}

func newTeaProgram(m *psModel, color bool) *teaProgram {
	ti := textinput.New()
	ti.Prompt = ":"
	ti.PromptStyle = lipgloss.NewStyle().Foreground(lipgloss.Color("14")).Bold(true)
	ti.TextStyle = lipgloss.NewStyle().Foreground(lipgloss.Color("15"))
	ti.Cursor.Style = lipgloss.NewStyle().Foreground(lipgloss.Color("14"))
	ti.CharLimit = 128
	ti.Placeholder = " catalog · logs · search · help · q"
	ti.PlaceholderStyle = lipgloss.NewStyle().Foreground(lipgloss.Color("8"))

	h := help.New()
	h.Styles.ShortKey = lipgloss.NewStyle().Foreground(lipgloss.Color("14"))
	h.Styles.ShortDesc = lipgloss.NewStyle().Foreground(lipgloss.Color("8"))
	h.Styles.ShortSeparator = lipgloss.NewStyle().Foreground(lipgloss.Color("8"))
	h.Styles.FullKey = lipgloss.NewStyle().Foreground(lipgloss.Color("14")).Bold(true)
	h.Styles.FullDesc = lipgloss.NewStyle().Foreground(lipgloss.Color("7"))
	h.Styles.FullSeparator = lipgloss.NewStyle().Foreground(lipgloss.Color("8"))

	return &teaProgram{
		m:        m,
		p:        makePainter(color),
		keys:     newPsKeyMap(),
		help:     h,
		cmdInput: ti,
	}
}

func (t teaProgram) Init() tea.Cmd {
	// No textinput.Blink here: the blink tick re-renders the whole frame and
	// would wipe a terminal text selection every half-second. Blink starts only
	// while the COMMANDS palette is open (see Update).
	return tea.EnterAltScreen
}

func (t *teaProgram) pushFocus() {
	if t.focusCh == nil {
		return
	}
	if f := t.m.focus(); f != t.sentFocus {
		select {
		case t.focusCh <- f:
			t.sentFocus = f
		default:
		}
	}
}

// syncCmdInput pushes pure-model command state into the bubbles textinput
// (placeholder, value, focus) so the Charm cursor blinks while the palette is
// open. The cell-buffer renderer still paints the authoritative prompt glyphs
// for golden stability; textinput is the input engine.
func (t *teaProgram) syncCmdInput() {
	if t.m.command == nil {
		if t.cmdInput.Focused() {
			t.cmdInput.Blur()
		}
		return
	}
	want := string(t.m.command.input)
	if t.cmdInput.Value() != want {
		t.cmdInput.SetValue(want)
		t.cmdInput.SetCursor(len(want))
	}
	if !t.cmdInput.Focused() {
		t.cmdInput.Focus()
	}
}

func (t teaProgram) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		t.w, t.h = msg.Width, msg.Height
		t.help.Width = msg.Width
		t.cmdInput.Width = maxInt(8, msg.Width/2-4)
		return t, nil
	case tea.KeyMsg:
		// When the COMMANDS palette is open, prefer textinput for printable
		// editing so paste and wide runes work; map structural keys through
		// the pure model so Esc/Enter/Tab/arrows stay one code path.
		if t.m.command != nil {
			return t.updateCommandKeys(msg)
		}
		k := teaKeyToPs(msg)
		if k.kind == psKeyNone {
			return t, nil
		}
		cancelID := t.m.update(k)
		if t.m.quit {
			t.quitting = true
			return t, tea.Quit
		}
		if cancelID != "" && t.cancelCh != nil {
			select {
			case t.cancelCh <- cancelID:
			default:
				t.m.note = "cancel already in flight"
			}
		}
		if req := t.m.takeCatalogReq(); req != nil && t.runCatalog != nil {
			t.runCatalog(*req)
		}
		t.pushFocus()
		t.syncCmdInput()
		// Start cursor blink only once the palette opens (not in Init).
		if t.m.command != nil {
			return t, textinput.Blink
		}
		return t, nil
	case teaPollMsg:
		pm := psPollMsg(msg)
		if pm.err != nil {
			t.err = pm.err
			t.quitting = true
			return t, tea.Quit
		}
		// Frozen: hold the painted frame so terminal mouse-select/copy works.
		// Still surface unreachability so the user knows the engine is gone.
		if t.m.frozen {
			if pm.unreachable {
				t.m.warn = psUnreachableWarn
			}
			return t, nil
		}
		if pm.unreachable {
			t.m.warn = psUnreachableWarn
			return t, nil
		}
		t.m.warn = pm.warn
		t.m.absorb(pm.snap)
		t.pushFocus()
		return t, nil
	case teaNoteMsg:
		t.m.note = string(msg)
		return t, nil
	case teaCatalogMsg:
		if t.m.frozen {
			return t, nil
		}
		t.m.absorbCatalog(psCatalogMsg(msg))
		t.pushFocus()
		return t, nil
	}
	// Ignore textinput blink ticks (and any other noise) when the palette is
	// closed so they do not re-paint and clear a terminal selection.
	return t, nil
}

// updateCommandKeys handles input while the COMMANDS palette is open.
func (t *teaProgram) updateCommandKeys(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	switch msg.Type {
	case tea.KeyCtrlC, tea.KeyEsc, tea.KeyEnter, tea.KeyTab,
		tea.KeyUp, tea.KeyDown, tea.KeyBackspace, tea.KeyDelete:
		k := teaKeyToPs(msg)
		t.m.update(k)
		if t.m.quit {
			t.quitting = true
			return t, tea.Quit
		}
		if req := t.m.takeCatalogReq(); req != nil && t.runCatalog != nil {
			t.runCatalog(*req)
		}
		t.pushFocus()
		t.syncCmdInput()
		return t, nil
	}

	// Browse mode (? help): j/k move the list while the prompt is empty,
	// matching the pure-model path so live and unit tests stay one grammar.
	if t.m.command != nil && t.m.command.browse && len(t.m.command.input) == 0 {
		if s := msg.String(); s == "j" || s == "k" {
			k := teaKeyToPs(msg)
			t.m.update(k)
			t.pushFocus()
			t.syncCmdInput()
			return t, nil
		}
	}

	// Printable / paste path: feed textinput then mirror into the pure model.
	var cmd tea.Cmd
	t.cmdInput, cmd = t.cmdInput.Update(msg)
	if t.m.command != nil {
		t.m.command.input = []rune(t.cmdInput.Value())
		t.m.command.err = ""
		t.m.command.cycling = false
		t.m.command.browse = false
		t.m.command.syncSel()
	}
	t.pushFocus()
	return t, cmd
}

func (t teaProgram) View() string {
	if t.quitting {
		return ""
	}
	w, h := t.w, t.h
	if w <= 0 {
		w = 80
	}
	if h <= 0 {
		h = 24
	}
	frame := renderPsFrame(t.m, w, h, !t.p.enabled)
	return string(frame.render(t.p))
}

// teaKeyToPs maps Bubble Tea key messages onto the view's psKey vocabulary.
func teaKeyToPs(msg tea.KeyMsg) psKey {
	switch msg.Type {
	case tea.KeyCtrlC:
		return psKey{kind: psKeyCtrlC}
	case tea.KeyEsc:
		return psKey{kind: psKeyEsc}
	case tea.KeyEnter:
		return psKey{kind: psKeyEnter}
	case tea.KeyBackspace, tea.KeyDelete:
		return psKey{kind: psKeyBackspace}
	case tea.KeyTab:
		return psKey{kind: psKeyTab}
	case tea.KeyUp:
		return psKey{kind: psKeyUp}
	case tea.KeyDown:
		return psKey{kind: psKeyDown}
	case tea.KeyLeft:
		return psKey{kind: psKeyLeft}
	case tea.KeyRight:
		return psKey{kind: psKeyRight}
	case tea.KeyRunes:
		if len(msg.Runes) == 1 {
			return psKey{kind: psKeyRune, r: msg.Runes[0]}
		}
	}
	if s := msg.String(); len(s) == 1 {
		return psKey{kind: psKeyRune, r: rune(s[0])}
	}
	return psKey{kind: psKeyNone}
}

// startChannelPumps injects poller/note/catalog events into the tea program.
func startChannelPumps(p *tea.Program, polls <-chan psPollMsg, notes <-chan string, catalogs <-chan psCatalogMsg) {
	if polls != nil {
		go func() {
			for pm := range polls {
				p.Send(teaPollMsg(pm))
			}
		}()
	}
	if notes != nil {
		go func() {
			for n := range notes {
				p.Send(teaNoteMsg(n))
			}
		}()
	}
	if catalogs != nil {
		go func() {
			for cm := range catalogs {
				p.Send(teaCatalogMsg(cm))
			}
		}()
	}
}

// RunLive is the production live view powered by Bubble Tea. The first snapshot
// was fetched before this ran (a dead engine never enters the alternate screen).
// color enables SGR styling when stdout is a TTY and NO_COLOR is unset.
//
// When out is not a real interactive terminal, RunLive reports entered=false so
// the CLI falls back to the JSON emit.
func RunLive(ctx context.Context, out io.Writer, color bool, c *Client, first Snapshot, target string) (bool, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	// Bubble Tea needs a real TTY for the alt-screen path. Mirror the old
	// openPsTerm gate so pipes/scripts stay on JSON.
	if f, ok := out.(*os.File); !ok || !isTerminalFile(f) {
		return false, nil
	}
	if !stdinIsTerminal() {
		return false, nil
	}

	ctx, cancel := context.WithCancel(ctx)
	defer cancel()

	polls := make(chan psPollMsg, 1)
	notes := make(chan string, 1)
	focusCh := make(chan string, 4)
	cancelCh := make(chan string, 4)
	catalogMsgs := make(chan psCatalogMsg, 4)
	go pollPs(ctx, c, psPollInterval, focusCh, cancelCh, polls, notes)

	m := newPsModel(first, target)
	model := newTeaProgram(m, color)
	model.focusCh = focusCh
	model.cancelCh = cancelCh
	model.runCatalog = func(req psCatalogReq) {
		go func() {
			msg := c.catalogAction(ctx, req)
			msg.seq = req.seq
			select {
			case catalogMsgs <- msg:
			case <-ctx.Done():
			}
		}()
	}

	// No WithMouseCellMotion: mouse reporting steals drags from the terminal
	// and makes text unselectable. Native select/copy works; press p to freeze
	// live updates so a poll does not wipe the highlight mid-drag.
	prog := tea.NewProgram(model,
		tea.WithOutput(out),
		tea.WithAltScreen(),
		tea.WithContext(ctx),
	)
	startChannelPumps(prog, polls, notes, catalogMsgs)

	final, err := prog.Run()
	cancel() // stop poller

	if err != nil {
		// tea returns an error when the program fails to start (non-TTY etc.).
		return false, nil
	}
	switch fp := final.(type) {
	case *teaProgram:
		return true, fp.err
	case teaProgram:
		return true, fp.err
	default:
		return true, nil
	}
}

func isTerminalFile(f *os.File) bool {
	if f == nil {
		return false
	}
	stat, err := f.Stat()
	if err != nil {
		return false
	}
	return stat.Mode()&os.ModeCharDevice != 0
}

func stdinIsTerminal() bool {
	stat, err := os.Stdin.Stat()
	if err != nil {
		return false
	}
	return stat.Mode()&os.ModeCharDevice != 0
}

func maxInt(a, b int) int {
	if a > b {
		return a
	}
	return b
}

// Style helpers used by install/uninstall ceremony surfaces (lipgloss).
var (
	styleOK   = lipgloss.NewStyle().Foreground(lipgloss.Color("10")).Bold(true)
	styleCyan = lipgloss.NewStyle().Foreground(lipgloss.Color("14")).Bold(true)
	styleDim  = lipgloss.NewStyle().Faint(true)
)

// FormatOK returns a lipgloss-styled success mark when color is wanted.
func FormatOK(s string, color bool) string {
	if !color {
		return s
	}
	return styleOK.Render(s)
}

// FormatCyan returns a lipgloss-styled cyan string when color is wanted.
func FormatCyan(s string, color bool) string {
	if !color {
		return s
	}
	return styleCyan.Render(s)
}

// FormatDim returns a lipgloss-styled dim string when color is wanted.
func FormatDim(s string, color bool) string {
	if !color {
		return s
	}
	return styleDim.Render(s)
}
