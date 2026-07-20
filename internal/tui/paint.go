package tui

import (
	"fmt"
	"os"
	"strings"
)

// Palette SGR codes for the live view. Basic 16-color values are the default;
// when the terminal advertises truecolor (COLORTERM=truecolor / 24bit) the
// package swaps in a GrokNight-adjacent truecolor set — neutral dark greys
// with a magenta accent — without changing any call site.
//
// Reset / inverse / dim stay structural (not hue-keyed) so selection and
// emphasis behave the same on every terminal.
var (
	ansiReset   = "\033[0m"
	ansiDim     = "\033[2m"
	ansiInverse = "\033[7m"
	ansiRed     = "\033[1;31m"
	ansiYellow  = "\033[1;33m"
	ansiGreen   = "\033[1;32m"
	ansiCyan    = "\033[1;36m"
	ansiBlue    = "\033[1;34m"
	ansiMagenta = "\033[1;35m"
	// ansiOrange is the heat ramp's third tone. Basic palette has no orange;
	// 256-color index 208 is the fallback, truecolor uses a soft amber.
	ansiOrange = "\033[38;5;208m"
)

// grokNight is the truecolor (RGB) face of the live view: magenta brand,
// cyan actions, soft status hues. Tuned for dark terminal backgrounds.
var grokNight = struct {
	magenta, cyan, green, yellow, red, blue, orange, dim string
}{
	magenta: rgb(192, 132, 252), // soft violet — brand / focus accent
	cyan:    rgb(34, 211, 238),  // action keys, running state
	green:   rgb(74, 222, 128),  // leader / succeeded
	yellow:  rgb(251, 191, 36),  // queued / warn
	red:     rgb(248, 113, 113), // hot heat / dead-lettered
	blue:    rgb(96, 165, 250),  // secondary accent
	orange:  rgb(251, 146, 60),  // mid heat
	dim:     rgb(107, 114, 128), // muted chrome (truecolor dim; no faint bit)
}

func rgb(r, g, b int) string {
	return fmt.Sprintf("\033[38;2;%d;%d;%dm", r, g, b)
}

// wantsTruecolor reports whether this process should emit 24-bit SGR.
// COLORTERM=truecolor|24bit is the de-facto signal; NO_COLOR always wins off.
func wantsTruecolor() bool {
	if _, no := os.LookupEnv("NO_COLOR"); no {
		return false
	}
	ct := strings.ToLower(os.Getenv("COLORTERM"))
	return ct == "truecolor" || ct == "24bit"
}

// applyPalette installs either the GrokNight truecolor set or the basic
// 16-color set into the package-level SGR vars. Safe to call from tests to
// pin a palette without depending on the ambient environment.
func applyPalette(truecolor bool) {
	if truecolor {
		ansiMagenta = grokNight.magenta
		ansiCyan = grokNight.cyan
		ansiGreen = grokNight.green
		ansiYellow = grokNight.yellow
		ansiRed = grokNight.red
		ansiBlue = grokNight.blue
		ansiOrange = grokNight.orange
		// Prefer a true gray over faint: faint multiplies poorly with RGB.
		ansiDim = grokNight.dim
		return
	}
	ansiReset = "\033[0m"
	ansiDim = "\033[2m"
	ansiInverse = "\033[7m"
	ansiRed = "\033[1;31m"
	ansiYellow = "\033[1;33m"
	ansiGreen = "\033[1;32m"
	ansiCyan = "\033[1;36m"
	ansiBlue = "\033[1;34m"
	ansiMagenta = "\033[1;35m"
	ansiOrange = "\033[38;5;208m"
}

func init() {
	applyPalette(wantsTruecolor())
}

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
