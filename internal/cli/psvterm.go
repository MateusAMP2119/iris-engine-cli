package cli

import (
	"io"
	"os"
	"sync"
	"time"
	"unicode/utf8"

	"golang.org/x/term"
)

// This file is the terminal layer of the `iris ps` live view: the raw-mode +
// alternate-screen session with its idempotent teardown, and the keypress
// decoder feeding the view's event loop. It is hand rolled over x/term and
// raw ANSI -- no TUI framework -- and decodes every printable rune verbatim
// (the search prompt types).

// The alternate-screen control sequences the live view session writes: enter
// switches to the alternate buffer, disables autowrap (writing the border's
// last column must never put the terminal in a wrap-pending state that a
// later erase would eat), hides the cursor, and clears once; leave restores
// autowrap and the cursor and switches back, leaving the primary buffer
// exactly as the command found it.
const (
	psTermEnter = "\x1b[?1049h\x1b[?7l\x1b[?25l\x1b[2J\x1b[H"
	psTermLeave = "\x1b[?7h\x1b[?25h\x1b[?1049l"
)

// psEscDelay is how long the key decoder waits after a bare ESC byte for the
// rest of a CSI sequence before classifying the ESC as a lone Escape press.
const psEscDelay = 50 * time.Millisecond

// psTermSession is one live-view terminal session: raw stdin for keypresses,
// the alternate screen on stdout for frames. leave is idempotent (sync.Once),
// so the deferred teardown and the explicit pre-fault teardown never
// double-restore, and nothing is ever printed while the terminal is raw.
type psTermSession struct {
	inFd  int
	outFd int // -1 when stdout is not a real file (size falls back)
	out   io.Writer
	saved *term.State
	once  sync.Once
}

// isTerminalForTest is a test seam (set only in tests via t.Cleanup) to force
// openPsTerm to report non-terminal so the command falls back to the JSON path.
var isTerminalForTest func(int) bool

// openPsTerm puts stdin in raw mode and stdout on the alternate screen,
// returning false -- take the JSON path instead, never a key-less view -- when
// stdin is not a terminal or raw mode is refused. The saved cooked state is
// captured first so leave can always put the terminal back.
func openPsTerm(out io.Writer) (*psTermSession, bool) {
	inFd := int(os.Stdin.Fd())
	if isTerminalForTest != nil {
		if !isTerminalForTest(inFd) {
			return nil, false
		}
	} else if !term.IsTerminal(inFd) {
		return nil, false
	}
	saved, err := term.GetState(inFd)
	if err != nil {
		return nil, false
	}
	if _, err := term.MakeRaw(inFd); err != nil {
		return nil, false
	}
	s := &psTermSession{inFd: inFd, outFd: -1, out: out, saved: saved}
	if f, ok := out.(*os.File); ok {
		s.outFd = int(f.Fd())
	}
	_, _ = io.WriteString(out, psTermEnter)
	return s, true
}

// leave restores the terminal: cursor back, primary screen back, cooked mode
// back. Safe to call from a defer and again explicitly on the same session.
func (s *psTermSession) leave() {
	s.once.Do(func() {
		_, _ = io.WriteString(s.out, psTermLeave)
		_ = term.Restore(s.inFd, s.saved)
	})
}

// size reports the terminal's current columns and rows, re-read on every call
// so a resize corrects on the next frame; an unprobeable terminal renders at
// the classic 80x24.
func (s *psTermSession) size() (w, h int) {
	if s.outFd >= 0 {
		if cols, rows, err := term.GetSize(s.outFd); err == nil && cols > 0 && rows > 0 {
			return cols, rows
		}
	}
	return 80, 24
}

// psKeyKind classifies one decoded keypress for the live view's event loop.
type psKeyKind int

// The keypress kinds the live view routes on. A printable keypress is
// psKeyRune with the rune verbatim (the search prompt consumes it; screens map
// letters like q, j, k, a, f, c themselves). psKeyNone is a decoded-and-
// discarded sequence (an unbound CSI like PageUp) that must reach no screen.
const (
	psKeyNone psKeyKind = iota
	psKeyRune
	psKeyUp
	psKeyDown
	psKeyLeft
	psKeyRight
	psKeyEnter
	psKeyEsc
	psKeyBackspace
	psKeyCtrlC
	psKeyTab
)

// psKey is one decoded keypress: its kind, and the rune for psKeyRune.
type psKey struct {
	kind psKeyKind
	r    rune
}

// readPsKeys pumps raw bytes from in into the decoder until in fails (the
// process is exiting, or the scripted test input is drained). It is the live
// view's key-reader goroutine; blocked in Read it cannot be cancelled, so it
// dies with the process.
func readPsKeys(in io.Reader, keys chan<- psKey) {
	bytes := make(chan byte)
	go func() {
		defer close(bytes)
		var b [1]byte
		for {
			n, err := in.Read(b[:])
			if n > 0 {
				bytes <- b[0]
			}
			if err != nil {
				return
			}
		}
	}()
	decodePsKeys(bytes, keys, psEscDelay)
}

// decodePsKeys turns a byte stream into keypresses: control bytes, CSI arrow
// sequences (a bare ESC not followed by more bytes within escDelay is the
// Escape key itself), and printable runes (multi-byte UTF-8 gathered whole).
// It returns when the byte stream closes, closing the key channel.
func decodePsKeys(in <-chan byte, out chan<- psKey, escDelay time.Duration) {
	defer close(out)
	for b := range in {
		switch {
		case b == 0x03: // Ctrl-C: raw mode disables ISIG, so it arrives as a byte
			out <- psKey{kind: psKeyCtrlC}
		case b == '\r' || b == '\n':
			out <- psKey{kind: psKeyEnter}
		case b == '\t':
			out <- psKey{kind: psKeyTab}
		case b == 0x7f || b == 0x08:
			out <- psKey{kind: psKeyBackspace}
		case b == 0x1b:
			if k := decodePsEscape(in, escDelay); k.kind != psKeyNone {
				out <- k
			}
		case b >= 0x20 && b < 0x7f:
			out <- psKey{kind: psKeyRune, r: rune(b)}
		case b >= utf8.RuneSelf:
			if r, ok := decodePsRune(b, in, escDelay); ok {
				out <- psKey{kind: psKeyRune, r: r}
			}
		}
		// Remaining control bytes carry no view meaning and are dropped.
	}
}

// decodePsEscape classifies what follows an ESC byte: an arrow's CSI (or SS3)
// sequence, nothing within the delay -- the Escape key itself -- or any other
// complete sequence, consumed whole and discarded (psKeyNone), so a PageUp or
// a modified arrow never leaks its parameter bytes into the search query or
// spuriously closes the overlay.
func decodePsEscape(in <-chan byte, escDelay time.Duration) psKey {
	lead, ok := readPsByte(in, escDelay)
	if !ok {
		return psKey{kind: psKeyEsc}
	}
	if lead != '[' && lead != 'O' {
		// ESC followed by an unrelated byte: the byte was a distinct keypress
		// typed inside the window; treat the ESC as Escape and drop the byte
		// (re-injecting it would reorder against later input).
		return psKey{kind: psKeyEsc}
	}
	// Consume the whole sequence: parameter (0x30-0x3F) and intermediate
	// (0x20-0x2F) bytes end at the final byte (0x40-0x7E), per ECMA-48.
	final, ok := readPsByte(in, escDelay)
	for ok && final < 0x40 {
		final, ok = readPsByte(in, escDelay)
	}
	if !ok {
		return psKey{kind: psKeyEsc}
	}
	switch final {
	case 'A':
		return psKey{kind: psKeyUp}
	case 'B':
		return psKey{kind: psKeyDown}
	case 'C':
		return psKey{kind: psKeyRight}
	case 'D':
		return psKey{kind: psKeyLeft}
	}
	return psKey{kind: psKeyNone}
}

// decodePsRune gathers the continuation bytes of a multi-byte UTF-8 keypress
// (they arrive together with the lead byte; the delay only guards a torn
// write) and decodes the rune.
func decodePsRune(lead byte, in <-chan byte, escDelay time.Duration) (rune, bool) {
	buf := []byte{lead}
	for !utf8.FullRune(buf) && len(buf) < utf8.UTFMax {
		b, ok := readPsByte(in, escDelay)
		if !ok {
			return 0, false
		}
		buf = append(buf, b)
	}
	r, _ := utf8.DecodeRune(buf)
	if r == utf8.RuneError {
		return 0, false
	}
	return r, true
}

// readPsByte reads one byte from the stream, giving up after the delay (a lone
// ESC, a torn sequence) or when the stream closes.
func readPsByte(in <-chan byte, delay time.Duration) (byte, bool) {
	select {
	case b, ok := <-in:
		return b, ok
	case <-time.After(delay):
		return 0, false
	}
}
