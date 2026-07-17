package cli

import (
	"os"
)

// ANSI SGR codes for the lifecycle ceremony, matching install.sh's palette; raw escapes keep this dependency-free.
const (
	ansiReset   = "\033[0m"
	ansiDim     = "\033[2m"
	ansiInverse = "\033[7m"
	ansiRed     = "\033[1;31m"
	ansiYellow  = "\033[1;33m"
	ansiGreen   = "\033[1;32m"
	ansiCyan    = "\033[1;36m"
	ansiBlue    = "\033[1;34m"
	ansiMagenta = "\033[1;35m"
)

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
func (p painter) dim(s string) string     { return p.paint(ansiDim, s) }

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
