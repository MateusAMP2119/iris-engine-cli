package cli

import (
	"strings"
	"testing"

	"github.com/charmbracelet/lipgloss"
)

func TestCeremonyMarkColumnAlignsChecksAndBars(t *testing.T) {
	checkLine := formatCeremonyLine("Engine state removed.", ceremonyCheckMark("✓"))
	// Body-only line ends where the mark column begins.
	bodyEnd := formatCeremonyLine("Removing engine state", "")

	prefix := ceremonyIndent + ceremonyBullet
	wantMarkAt := lipgloss.Width(prefix) + ceremonyBodyCols

	checkMarkStart := strings.Index(checkLine, "[")
	if checkMarkStart < 0 {
		t.Fatal("check line missing [")
	}
	if got := lipgloss.Width(checkLine[:checkMarkStart]); got != wantMarkAt {
		t.Errorf("check mark column = %d, want %d\nline=%q", got, wantMarkAt, checkLine)
	}
	if got := lipgloss.Width(bodyEnd); got != wantMarkAt {
		t.Errorf("progress body column = %d, want %d\nline=%q", got, wantMarkAt, bodyEnd)
	}
}

func TestFormatProgressPctWidth(t *testing.T) {
	for _, pct := range []int{0, 5, 10, 99, 100} {
		s := formatProgressPct(pct)
		if lipgloss.Width(s) != progressPctCols {
			t.Errorf("formatProgressPct(%d) = %q width %d, want %d", pct, s, lipgloss.Width(s), progressPctCols)
		}
	}
}

func TestPadCeremonyBody(t *testing.T) {
	p := padCeremonyBody("hi")
	if lipgloss.Width(p) != ceremonyBodyCols {
		t.Fatalf("width %d want %d", lipgloss.Width(p), ceremonyBodyCols)
	}
}
