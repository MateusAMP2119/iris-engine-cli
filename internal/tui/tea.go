package tui

import (
	"context"
	"io"
	"os"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
)

// teaMsg wrappers bridge poller/catalog/note channels into Bubble Tea.
type (
	teaPollMsg    psPollMsg
	teaNoteMsg    string
	teaCatalogMsg psCatalogMsg
)

// teaProgram is the Bubble Tea model for the live `iris ps` view. It owns the
// pure psModel state machine and renders through the existing frame buffer so
// goldens and key behavior stay stable while the event loop rides tea.Program.
type teaProgram struct {
	m *psModel
	p painter
	w int
	h int

	focusCh    chan<- string
	cancelCh   chan<- string
	runCatalog func(psCatalogReq)
	sentFocus  string
	err        error
	quitting   bool
}

func (t teaProgram) Init() tea.Cmd {
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

func (t teaProgram) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		t.w, t.h = msg.Width, msg.Height
		return t, nil
	case tea.KeyMsg:
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
		return t, nil
	case teaPollMsg:
		pm := psPollMsg(msg)
		if pm.err != nil {
			t.err = pm.err
			t.quitting = true
			return t, tea.Quit
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
		t.m.absorbCatalog(psCatalogMsg(msg))
		t.pushFocus()
		return t, nil
	}
	return t, nil
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
	model := &teaProgram{
		m: m, p: makePainter(color),
		focusCh: focusCh, cancelCh: cancelCh,
		runCatalog: func(req psCatalogReq) {
			go func() {
				msg := c.catalogAction(ctx, req)
				msg.seq = req.seq
				select {
				case catalogMsgs <- msg:
				case <-ctx.Done():
				}
			}()
		},
	}

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
