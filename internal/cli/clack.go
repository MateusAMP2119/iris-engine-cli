package cli

// The clack-style ceremony kit: the rail-and-box widget vocabulary the
// interactive quickstart tour renders with on a real terminal — an intro
// corner, a running left rail, boxed notes, an arrow-key select, a Yes/No
// confirm, and a spinner. Hand-rolled on the existing painter (no TUI
// framework); raw terminal mode comes from golang.org/x/term. Every widget
// is production-surface only: the tour's injectable seams (tourPick,
// tourInput) bypass this file entirely, so harnessed tests never meet raw
// mode. EXPERIMENT: not yet covered by spec contracts.

import (
	"fmt"
	"io"
	"os"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"golang.org/x/term"
)

// The rail glyphs, matching the @clack/prompts visual language.
const (
	railTop  = "┌"
	railBar  = "│"
	railNote = "◇"
	railAsk  = "◆"
	railEnd  = "└"
)

// rawSession state is narrowly used to allow forcing the terminal back to
// its pre-raw (cooked) state when a ctx cancellation aborts a widget goroutine
// before its own defer restore can run. This prevents abort/error prints from
// happening while the terminal is still raw. Cleared after use.
var (
	rawSessionMu   sync.Mutex
	rawSessionOrig *term.State
)

// forceRestoreTerminal best-effort restores stdin to the state captured before
// the last raw session. Called from askClack* on ctx cancel paths.
func forceRestoreTerminal() {
	rawSessionMu.Lock()
	st := rawSessionOrig
	rawSessionMu.Unlock()
	if st != nil {
		_ = term.Restore(int(os.Stdin.Fd()), st)
		rawSessionMu.Lock()
		rawSessionOrig = nil
		rawSessionMu.Unlock()
	}
}

// isTerminalForTest is a test seam (set only in tests via t.Cleanup) to force
// openRawTTY to report non-terminal so clack* return ok=false (fallback path).
var isTerminalForTest func(int) bool

// defaultClackBoxWidth is the fallback inner width for boxed notes when we
// cannot determine the terminal size.
const defaultClackBoxWidth = 72

// clackBoxInnerWidth returns the target inner width for clack boxes.
// It prefers the actual terminal width (via term.GetSize) for better narrow
// terminal support, with reasonable margins, otherwise the default.
func clackBoxInnerWidth() int {
	fd := int(os.Stdin.Fd())
	if term.IsTerminal(fd) {
		if cols, _, err := term.GetSize(fd); err == nil && cols > 20 {
			// leave room for rail + indent + box borders
			w := cols - 8
			if w > 20 && w < defaultClackBoxWidth {
				return w
			}
			if w >= defaultClackBoxWidth {
				return defaultClackBoxWidth
			}
		}
	}
	return defaultClackBoxWidth
}

// clackIntro opens the rail: `┌  title` and one bare rail line.
func clackIntro(w io.Writer, p painter, title string) {
	fmt.Fprintf(w, "%s  %s\n", railTop, p.cyan(title))
	clackBar(w, p)
}

// clackBar prints one bare rail line.
func clackBar(w io.Writer, p painter) {
	fmt.Fprintln(w, p.dim(railBar))
}

// clackOutro closes the rail: `└  message`.
func clackOutro(w io.Writer, p painter, msg string) {
	fmt.Fprintf(w, "%s  %s\n", railEnd, msg)
}

// wrapLine hard-wraps one line to width columns on spaces.
// Long words (no spaces) are hard-broken to respect width.
func wrapLine(s string, width int) []string {
	if width <= 0 {
		width = 1
	}
	runes := []rune(s)
	if len(runes) <= width {
		return []string{s}
	}
	var out []string
	words := strings.Fields(s)
	cur := ""
	for _, word := range words {
		wlen := utf8.RuneCountInString(word)
		if cur == "" {
			if wlen > width {
				// hard-break long word
				for len(word) > 0 {
					r := []rune(word)
					if len(r) > width {
						out = append(out, string(r[:width]))
						word = string(r[width:])
					} else {
						cur = word
						break
					}
				}
				continue
			}
			cur = word
			continue
		}
		if utf8.RuneCountInString(cur)+1+wlen > width {
			out = append(out, cur)
			if wlen > width {
				// hard-break
				for len(word) > 0 {
					r := []rune(word)
					if len(r) > width {
						out = append(out, string(r[:width]))
						word = string(r[width:])
					} else {
						cur = word
						break
					}
				}
				continue
			}
			cur = word
			continue
		}
		cur += " " + word
	}
	if cur != "" {
		out = append(out, cur)
	}
	if len(out) == 0 {
		out = []string{""}
	}
	return out
}

// clackNote renders one boxed note hanging off the rail.
//
//	◇  Title ────────────────╮
//	│                        │
//	│  body                  │
//	│                        │
//	├────────────────────────╯
func clackNote(w io.Writer, p painter, title, body string) {
	boxW := clackBoxInnerWidth()
	var lines []string
	for _, raw := range strings.Split(strings.TrimRight(body, "\n"), "\n") {
		lines = append(lines, wrapLine(raw, boxW-4)...)
	}
	inner := utf8.RuneCountInString(title) + 4
	for _, l := range lines {
		if n := utf8.RuneCountInString(l) + 4; n > inner {
			inner = n
		}
	}
	if inner > boxW {
		inner = boxW
	}
	// pad for the title rule: we want " Title " + ─s + "╮" to reach the right edge.
	// After "◇  " + cyan(title) there is one space from the format, so:
	// title content + 1 (space) + pad + 1 (╮) + borders accounted in inner.
	pad := inner - utf8.RuneCountInString(title) - 2
	if pad < 1 {
		pad = 1
	}
	fmt.Fprintf(w, "%s  %s %s\n", railNote, p.cyan(title), p.dim(strings.Repeat("─", pad)+"╮"))
	blank := strings.Repeat(" ", inner)
	fmt.Fprintf(w, "%s%s%s\n", p.dim(railBar), blank, p.dim("│"))
	for _, l := range lines {
		fill := inner - 4 - utf8.RuneCountInString(l)
		if fill < 0 {
			fill = 0
		}
		fmt.Fprintf(w, "%s  %s%s  %s\n", p.dim(railBar), l, strings.Repeat(" ", fill), p.dim("│"))
	}
	fmt.Fprintf(w, "%s%s%s\n", p.dim(railBar), blank, p.dim("│"))
	fmt.Fprintf(w, "%s\n", p.dim("├"+strings.Repeat("─", inner)+"╯"))
}

// clackOption is one entry of the arrow-key select: a label and a dim hint.
type clackOption struct {
	label string
	hint  string
}

// The keys the raw-mode reader distinguishes.
type clackKey int

const (
	keyOther clackKey = iota
	keyUp
	keyDown
	keyLeft
	keyRight
	keyEnter
	keyQuit  // q, Esc, Ctrl-C, Ctrl-D, or EOF: the clean decline
	keyDigit // '1'..'9'; the digit value rides in the second return
)

// rawTTY is one raw-mode session over the process stdin.
type rawTTY struct {
	fd    int
	state *term.State
}

// openRawTTY switches the process stdin to raw mode. ok=false — not a
// terminal, or raw mode refused — means the caller must fall back to the
// cooked line dialogue.
func openRawTTY() (*rawTTY, bool) {
	fd := int(os.Stdin.Fd())
	if isTerminalForTest != nil {
		if !isTerminalForTest(fd) {
			return nil, false
		}
	} else if !term.IsTerminal(fd) {
		return nil, false
	}
	// Capture the original cooked state so forceRestore can put the terminal
	// back even if the widget goroutine hasn't returned its defer yet.
	orig, _ := term.GetState(fd)
	if orig != nil {
		rawSessionMu.Lock()
		rawSessionOrig = orig
		rawSessionMu.Unlock()
	}
	state, err := term.MakeRaw(fd)
	if err != nil {
		return nil, false
	}
	return &rawTTY{fd: fd, state: state}, true
}

// restore returns the terminal to cooked mode and clears any stashed session state.
func (r *rawTTY) restore() {
	_ = term.Restore(r.fd, r.state)
	rawSessionMu.Lock()
	rawSessionOrig = nil
	rawSessionMu.Unlock()
}

// readKey reads and classifies one keypress in raw mode.
func (r *rawTTY) readKey() (clackKey, int) {
	var b [1]byte
	n, err := os.Stdin.Read(b[:])
	if err != nil || n == 0 {
		return keyQuit, 0
	}
	switch b[0] {
	case 0x03, 0x04, 'q', 'Q': // Ctrl-C, Ctrl-D
		return keyQuit, 0
	case '\r', '\n':
		return keyEnter, 0
	case 0x1b: // ESC — bare, or the lead of an arrow sequence
		// Read the next two bytes for CSI sequences with short timeout so a lone
		// ESC or slow/garbage input does not hang the prompt for long.
		seq1, ok1 := readByteShort(50 * time.Millisecond)
		if !ok1 || seq1 != '[' {
			return keyQuit, 0 // bare ESC or bad seq -> treat as quit (decline)
		}
		seq2, ok2 := readByteShort(50 * time.Millisecond)
		if !ok2 {
			return keyQuit, 0
		}
		switch seq2 {
		case 'A':
			return keyUp, 0
		case 'B':
			return keyDown, 0
		case 'C':
			return keyRight, 0
		case 'D':
			return keyLeft, 0
		}
		return keyOther, 0
	}
	if b[0] >= '1' && b[0] <= '9' {
		return keyDigit, int(b[0] - '0')
	}
	switch b[0] {
	case 'k':
		return keyUp, 0
	case 'j':
		return keyDown, 0
	case 'y', 'Y':
		return keyLeft, 0 // confirm shorthand: yes
	case 'n', 'N':
		return keyRight, 0 // confirm shorthand: no
	}
	return keyOther, 0
}

// In raw mode the terminal does no output post-processing: every line the
// widgets print while raw ends in \r\n explicitly.
const rawEOL = "\r\n"

// readByteShort tries to read one byte from stdin, returning ok=false if it
// does not arrive within the timeout. Used to keep ESC sequence collection
// from blocking the prompt on incomplete or slow input.
func readByteShort(timeout time.Duration) (byte, bool) {
	type res struct {
		b   byte
		n   int
		err error
	}
	ch := make(chan res, 1)
	go func() {
		var b [1]byte
		n, err := os.Stdin.Read(b[:])
		ch <- res{b: b[0], n: n, err: err}
	}()
	select {
	case r := <-ch:
		if r.err != nil || r.n == 0 {
			return 0, false
		}
		return r.b, true
	case <-time.After(timeout):
		return 0, false
	}
}

// ansiUp moves the cursor up n lines; ansiClear erases the current line.
func ansiUp(w io.Writer, n int) { fmt.Fprintf(w, "\033[%dA", n) }
func ansiClearLine(w io.Writer) { fmt.Fprint(w, "\r\033[2K") }

// clackSelect renders the arrow-key radio select and returns the picked
// 1-based index. Up/Down (or j/k) move, 1-9 jump, Enter accepts, q/Esc/
// Ctrl-C decline. ok=false means raw mode was unavailable and nothing was
// rendered: the caller falls back to the numbered line dialogue.
//
// Note: digits only support 1-9. For >9 entries (rare in catalog) the user
// relies on arrows/jk. The line fallback supports any number.

func clackSelect(w io.Writer, p painter, question string, options []clackOption) (choice int, ans promptAnswer, ok bool) {
	tty, rawOK := openRawTTY()
	if !rawOK {
		return 0, answerQuit, false
	}
	defer tty.restore()

	fmt.Fprintf(w, "%s  %s %s%s", railAsk, question, p.dim("(↑/↓/j/k or 1-9 · Enter picks · q quits)"), rawEOL)
	idx := 0
	render := func() {
		for i, opt := range options {
			marker, label := "○", opt.label
			if i == idx {
				marker, label = p.green("●"), p.cyan(opt.label)
			}
			hint := ""
			if opt.hint != "" {
				hint = "  " + p.dim(opt.hint)
			}
			fmt.Fprintf(w, "%s  %s %s%s%s", p.dim(railBar), marker, label, hint, rawEOL)
		}
	}
	render()
	for {
		key, digit := tty.readKey()
		switch key {
		case keyQuit:
			fmt.Fprint(w, rawEOL)
			return 0, answerQuit, true
		case keyEnter:
			return idx + 1, answerProceed, true
		case keyUp:
			idx = (idx + len(options) - 1) % len(options)
		case keyDown:
			idx = (idx + 1) % len(options)
		case keyDigit:
			if digit >= 1 && digit <= len(options) {
				idx = digit - 1
			}
		default:
			continue
		}
		// Rewind to the start of the options block and repaint.
		// Using \r + clear-to-eol on each line is more robust across terminals
		// than mixing up/down when line lengths change.
		ansiUp(w, len(options))
		for range options {
			fmt.Fprint(w, "\r\033[2K\n")
		}
		ansiUp(w, len(options))
		render()
	}
}

// clackConfirm renders the Yes/No radio confirm and returns the answer.
// Left/Right (or y/n) toggle, Enter accepts, q/Esc/Ctrl-C decline. ok=false
// means raw mode was unavailable and nothing was rendered.
func clackConfirm(w io.Writer, p painter, question string, def bool) (yes bool, ans promptAnswer, ok bool) {
	tty, rawOK := openRawTTY()
	if !rawOK {
		return false, answerQuit, false
	}
	defer tty.restore()

	fmt.Fprintf(w, "%s  %s%s", railAsk, question, rawEOL)
	val := def
	render := func() {
		yesMark, noMark := "○", "○"
		yesLabel, noLabel := "Yes", "No"
		if val {
			yesMark, yesLabel = p.green("●"), p.cyan("Yes")
		} else {
			noMark, noLabel = p.green("●"), p.cyan("No")
		}
		fmt.Fprintf(w, "%s  %s %s / %s %s%s", p.dim(railBar), yesMark, yesLabel, noMark, noLabel, rawEOL)
	}
	render()
	for {
		key, _ := tty.readKey()
		switch key {
		case keyQuit:
			fmt.Fprint(w, rawEOL)
			return false, answerQuit, true
		case keyEnter:
			return val, answerProceed, true
		case keyLeft, keyUp:
			val = true
		case keyRight, keyDown:
			val = false
		default:
			continue
		}
		// Simple single-line repaint for confirm.
		ansiUp(w, 1)
		fmt.Fprint(w, "\r\033[2K")
		render()
	}
}

// engineBannerRows is the block-art IRIS ENGINE mark, the same letterform
// family as install.sh's IRIS CLI banner.
var engineBannerRows = []string{
	"  ██╗██████╗ ██╗███████╗    ███████╗███╗   ██╗ ██████╗ ██╗███╗   ██╗███████╗",
	"  ██║██╔══██╗██║██╔════╝    ██╔════╝████╗  ██║██╔════╝ ██║████╗  ██║██╔════╝",
	"  ██║██████╔╝██║███████╗    █████╗  ██╔██╗ ██║██║  ███╗██║██╔██╗ ██║█████╗  ",
	"  ██║██╔══██╗██║╚════██║    ██╔══╝  ██║╚██╗██║██║   ██║██║██║╚██╗██║██╔══╝  ",
	"  ██║██║  ██║██║███████║    ███████╗██║ ╚████║╚██████╔╝██║██║ ╚████║███████╗",
	"  ╚═╝╚═╝  ╚═╝╚═╝╚══════╝    ╚══════╝╚═╝  ╚═══╝ ╚═════╝ ╚═╝╚═╝  ╚═══╝╚══════╝",
}

// engineBanner paints the IRIS ENGINE mark, one rainbow color per row like
// the installer's banner. Ceremony surface only: a disabled painter (piped,
// NO_COLOR, --json) paints nothing — the chapter mark still names the act.
func engineBanner(w io.Writer, p painter) {
	if !p.enabled {
		return
	}
	for i, row := range engineBannerRows {
		fmt.Fprintf(w, "%s%s%s\n", rainbowPalette[i%len(rainbowPalette)], row, ansiReset)
	}
}

// spinnerFrames is the braille spinner cycle.
var spinnerFrames = []string{"⠋", "⠙", "⠹", "⠸", "⠼", "⠴", "⠦", "⠧", "⠇", "⠏"}

// startSpinner animates `<frame> msg` in place on the ceremony surface and
// returns its stop function, which clears the line and prints `✓ done` — or,
// with an empty done, only clears (the failure path renders its own error).
//
// Contract: call the returned stop exactly once when you want animation to end.
// The stop func is safe to call multiple times (idempotent). Never write to w
// from other goroutines between start and stop.
func startSpinner(w io.Writer, p painter, msg string) func(done string) {
	var stopOnce sync.Once
	if !p.enabled {
		fmt.Fprintf(w, "· %s\n", msg)
		return func(done string) {
			stopOnce.Do(func() {
				if done != "" {
					fmt.Fprintf(w, "✓ %s\n", done)
				}
			})
		}
	}
	stopCh := make(chan struct{})
	doneCh := make(chan struct{})
	go func() {
		defer close(doneCh)
		i := 0
		tick := time.NewTicker(80 * time.Millisecond)
		defer tick.Stop()
		for {
			fmt.Fprintf(w, "\r\033[2K%s %s", p.cyan(spinnerFrames[i%len(spinnerFrames)]), msg)
			select {
			case <-stopCh:
				return
			case <-tick.C:
				i++
			}
		}
	}()
	return func(done string) {
		stopOnce.Do(func() {
			close(stopCh)
			<-doneCh
			fmt.Fprint(w, "\r\033[2K")
			if done != "" {
				fmt.Fprintf(w, "%s %s\n", p.green("✓"), done)
			}
		})
	}
}
