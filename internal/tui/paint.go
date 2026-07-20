package tui

import "os"

// ANSI SGR codes for the live view; raw escapes keep this dependency-free.
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

// painter renders SGR styling for the live view. When disabled every method
// returns its argument unchanged so colorless frames stay plain text.
type painter struct {
	enabled bool
}

func makePainter(enabled bool) painter {
	if _, noColor := os.LookupEnv("NO_COLOR"); noColor {
		return painter{}
	}
	return painter{enabled: enabled}
}

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
