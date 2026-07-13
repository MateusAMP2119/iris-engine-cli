package cli

import (
	"os"
	"strings"
)

// The ANSI SGR codes for the lifecycle-command terminal ceremony. They match the
// bright palette the curl installer and uninstaller paint (install.sh /
// uninstall.sh): bright red/yellow/green/cyan/blue/magenta, plus dim and reset.
// Raw escape codes keep this dependency-free.
const (
	ansiReset   = "\033[0m"
	ansiDim     = "\033[2m"
	ansiRed     = "\033[1;31m"
	ansiYellow  = "\033[1;33m"
	ansiGreen   = "\033[1;32m"
	ansiCyan    = "\033[1;36m"
	ansiBlue    = "\033[1;34m"
	ansiMagenta = "\033[1;35m"
)

// rainbowPalette is the per-letter color cycle of the "Goodbye" farewell,
// matching uninstall.sh (R, Y, G, C, B, M, then wrapping).
var rainbowPalette = []string{ansiRed, ansiYellow, ansiGreen, ansiCyan, ansiBlue, ansiMagenta}

// painter renders the lifecycle-command terminal ceremony. When enabled its
// methods wrap text in ANSI SGR codes; when disabled every method returns its
// argument byte-for-byte unchanged, so piped and --json output stays plain text
// and no escape ever reaches a non-terminal consumer.
type painter struct {
	enabled bool
}

// makePainter decides whether the ceremony is on for one command invocation.
// Styling activates only when --json is off AND NO_COLOR is unset AND stdout is
// an interactive terminal (isTTY). The isTTY seam is injectable so tests can force
// either mode without a real terminal.
func makePainter(jsonMode bool, isTTY func() bool) painter {
	if jsonMode {
		return painter{}
	}
	if _, noColor := os.LookupEnv("NO_COLOR"); noColor {
		return painter{}
	}
	return painter{enabled: isTTY()}
}

// paint wraps s in an SGR code and a reset when styling is on; otherwise it
// returns s unchanged.
func (p painter) paint(code, s string) string {
	if !p.enabled {
		return s
	}
	return code + s + ansiReset
}

func (p painter) green(s string) string   { return p.paint(ansiGreen, s) }
func (p painter) cyan(s string) string    { return p.paint(ansiCyan, s) }
func (p painter) magenta(s string) string { return p.paint(ansiMagenta, s) }
func (p painter) yellow(s string) string  { return p.paint(ansiYellow, s) }
func (p painter) dim(s string) string     { return p.paint(ansiDim, s) }

// rainbow renders s one bright color per rune (cycling R, Y, G, C, B, M), the
// farewell gradient of uninstall.sh. When disabled it returns s unchanged, so the
// plain "Goodbye from iris." line stays byte-exact.
func (p painter) rainbow(s string) string {
	if !p.enabled {
		return s
	}
	var b strings.Builder
	i := 0
	for _, r := range s {
		b.WriteString(rainbowPalette[i%len(rainbowPalette)])
		b.WriteRune(r)
		i++
	}
	b.WriteString(ansiReset)
	return b.String()
}

// newPainter builds the painter for one invocation, resolving the terminal gate
// through the injected isTTY seam or, in production, the real stdout stat.
func (a *app) newPainter(jsonMode bool) painter {
	tty := a.isTTY
	if tty == nil {
		tty = a.stdoutIsTerminal
	}
	return makePainter(jsonMode, tty)
}

// stdoutIsTerminal reports whether the command's stdout is an interactive
// terminal: it is a real *os.File and its mode carries the char-device bit (the
// same detection terminalConfirm uses for stdin). A buffer (tests) or a redirected
// file or pipe is not a char device, so the ceremony stays plain there.
func (a *app) stdoutIsTerminal() bool {
	f, ok := a.out.(*os.File)
	if !ok {
		return false
	}
	stat, err := f.Stat()
	if err != nil {
		return false
	}
	return stat.Mode()&os.ModeCharDevice != 0
}
