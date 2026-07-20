package tui

import (
	"strings"
	"testing"
)

// TestPaletteTruecolor proves the GrokNight truecolor swap and the basic
// fallback stay well-formed SGR, and that applyPalette is idempotent both ways.
func TestPaletteTruecolor(t *testing.T) {
	t.Cleanup(func() { applyPalette(false) })

	t.Run("basic palette uses classic bold ANSI hues", func(t *testing.T) {
		applyPalette(false)
		if !strings.HasPrefix(ansiCyan, "\033[1;3") {
			t.Fatalf("basic cyan = %q, want bold 16-color", ansiCyan)
		}
		if strings.Contains(ansiMagenta, "38;2;") {
			t.Fatalf("basic magenta leaked truecolor: %q", ansiMagenta)
		}
	})

	t.Run("truecolor palette uses GrokNight RGB codes", func(t *testing.T) {
		applyPalette(true)
		for name, code := range map[string]string{
			"magenta": ansiMagenta,
			"cyan":    ansiCyan,
			"green":   ansiGreen,
			"yellow":  ansiYellow,
			"red":     ansiRed,
			"orange":  ansiOrange,
			"dim":     ansiDim,
		} {
			if !strings.Contains(code, "38;2;") {
				t.Errorf("%s = %q, want truecolor 38;2;R;G;B", name, code)
			}
		}
		if ansiMagenta != grokNight.magenta {
			t.Errorf("magenta = %q, want grokNight %q", ansiMagenta, grokNight.magenta)
		}
	})

	t.Run("truecolor frame emission carries RGB on focus border", func(t *testing.T) {
		applyPalette(true)
		m := newPsModel(psvFixture(), "")
		out := string(renderPsFrame(m, 150, 40, false).render(painter{enabled: true}))
		if !strings.Contains(out, grokNight.cyan) {
			t.Error("truecolor frame carries no cyan focus/action tone")
		}
		if !strings.Contains(out, grokNight.green) {
			t.Error("truecolor frame carries no green leader tone")
		}
	})

	t.Run("applyPalette(false) restores basic after truecolor", func(t *testing.T) {
		applyPalette(true)
		applyPalette(false)
		if strings.Contains(ansiCyan, "38;2;") {
			t.Fatalf("restore failed: cyan still truecolor %q", ansiCyan)
		}
	})
}
