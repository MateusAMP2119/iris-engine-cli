package cli

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/charmbracelet/huh"
	"github.com/charmbracelet/lipgloss"
)

func TestCeremonyMarkColumnAlignsChecksAndBars(t *testing.T) {
	checkLine := formatCeremonyLine("Engine state removed.", ceremonyCheckMark("✓"))
	// Synthetic progress mark with the same display width as a real bar+pct.
	barMark := strings.Repeat("=", progressBarCols) + " " + formatProgressPct(100)
	barLine := formatCeremonyLine("Removing engine state", barMark)
	// Longer progress label that still fits the body column.
	barLine2 := formatCeremonyLine("Removing binary and traces", barMark)

	wantLineEnd := ceremonyLineWidth()

	if got := lipgloss.Width(checkLine); got != wantLineEnd {
		t.Errorf("check line width = %d, want %d\nline=%q", got, wantLineEnd, checkLine)
	}
	if got := lipgloss.Width(barLine); got != wantLineEnd {
		t.Errorf("bar line width = %d, want %d\nline=%q", got, wantLineEnd, barLine)
	}
	if got := lipgloss.Width(barLine2); got != wantLineEnd {
		t.Errorf("long-label bar line width = %d, want %d\nline=%q", got, wantLineEnd, barLine2)
	}

	// [✓] is right-aligned: its ']' shares the line's right edge with "100%".
	checkMarkStart := strings.Index(checkLine, "[")
	if checkMarkStart < 0 {
		t.Fatal("check line missing [")
	}
	if got := lipgloss.Width(checkLine[:checkMarkStart]); got >= wantLineEnd {
		t.Errorf("check mark starts at %d, want before line end %d", got, wantLineEnd)
	}
	// Right edges match across check and progress lines.
	if lipgloss.Width(checkLine) != lipgloss.Width(barLine) || lipgloss.Width(barLine) != lipgloss.Width(barLine2) {
		t.Errorf("right edges differ: check=%d bar=%d bar2=%d",
			lipgloss.Width(checkLine), lipgloss.Width(barLine), lipgloss.Width(barLine2))
	}
}

func TestCeremonyLineOverflowKeepsMinGap(t *testing.T) {
	// Wider than body+mark combined; mark should still appear after one space.
	long := strings.Repeat("x", ceremonyBodyCols+ceremonyMarkCols)
	line := formatCeremonyLine(long, ceremonyCheckMark("✓"))
	if !strings.Contains(line, long+" "+ceremonyCheckMark("✓")) {
		t.Fatalf("overflow line should keep a one-space gap, got %q", line)
	}
}

// TestCeremonyConfirmNoAlignsWithCheck proves the uninstall yes/no confirm places
// the right edge of the No button on the same column as the ceremony mark edge.
func TestCeremonyConfirmNoAlignsWithCheck(t *testing.T) {
	checkLine := formatCeremonyLine("Iris engine stopped successfully.", ceremonyCheckMark("✓"))
	markEnd := lipgloss.Width(checkLine) - 1 // inclusive display column of ']'

	var ok bool
	c := newCeremonyConfirm("Remove engine state under /home/tiger/.iris?", &ok)
	c.Focus()
	_ = c.WithTheme(ceremonyConfirmTheme())
	_ = c.WithWidth(ceremonyConfirmWidth())

	lines := strings.Split(c.View(), "\n")
	if len(lines) == 0 {
		t.Fatal("confirm view empty")
	}
	last := stripANSI(lines[len(lines)-1])
	noAt := strings.Index(last, "No")
	if noAt < 0 {
		t.Fatalf("confirm buttons missing No: %q", last)
	}
	// Button is pad(2)+"No"+pad(2); inclusive end of the No box.
	noStart := lipgloss.Width(last[:noAt])
	boxEnd := noStart + lipgloss.Width("No") + 2 - 1
	if boxEnd != markEnd {
		t.Errorf("No box end column = %d, want %d (mark right edge)\ncheck: %s\nbtns:  %s",
			boxEnd, markEnd, checkLine, last)
	}
}

func TestPadCeremonyMarkRightAligns(t *testing.T) {
	mark := ceremonyCheckMark("✓")
	padded := padCeremonyMark(mark)
	if lipgloss.Width(padded) != ceremonyMarkCols {
		t.Fatalf("padded width %d, want %d", lipgloss.Width(padded), ceremonyMarkCols)
	}
	if !strings.HasSuffix(padded, mark) {
		t.Fatalf("padded %q should end with %q", padded, mark)
	}
}

// TestCeremonySetupFormWidthMatchesGrid proves install setup menus use the same
// frame width as ceremony check lines (indent + bullet + body + mark).
func TestCeremonySetupFormWidthMatchesGrid(t *testing.T) {
	checkLine := formatCeremonyLine("Catalog configured", ceremonyCheckMark("✓"))
	want := lipgloss.Width(checkLine)
	if got := ceremonyConfirmWidth(); got != want {
		t.Fatalf("ceremonyConfirmWidth() = %d, check line width = %d", got, want)
	}
	// Form is built with that width; ThemeCharm content is frame-inset.
	_ = newCeremonySetupForm(huh.NewGroup(huh.NewSelect[string]().Options(huh.NewOption("a", "a"))))
	// Full-width mark is unchanged.
	full := strings.Repeat("x", ceremonyMarkCols)
	if got := padCeremonyMark(full); got != full {
		t.Fatalf("full-width mark should be unchanged, got %q", got)
	}
	if padCeremonyMark("") != "" {
		t.Fatal("empty mark should stay empty")
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

func TestFormatFarewellAlignsAuthorToCeremonyEdge(t *testing.T) {
	edge := ceremonyLineWidth()
	for _, q := range farewellQuotes {
		lines := formatFarewell(q)
		if len(lines) < 2 {
			t.Fatalf("quote %q: want quote line(s) + author, got %v", q.author, lines)
		}
		author := lines[len(lines)-1]
		if got := lipgloss.Width(author); got != edge {
			t.Errorf("author line width for %s = %d, want ceremony edge %d\n%q", q.author, got, edge, author)
		}
		for i, line := range lines[:len(lines)-1] {
			if got := lipgloss.Width(line); got > edge {
				t.Errorf("quote line %d for %s wider than edge: %d > %d\n%q", i, q.author, got, edge, line)
			}
		}
	}
}

func TestRemoveEngineHomeWipesWorkspaceAndBin(t *testing.T) {
	home := t.TempDir()
	for _, sub := range []string{"bin", "workspace", "objects"} {
		if err := os.MkdirAll(filepath.Join(home, sub), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(home, "bin", "iris"), []byte("x"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := removeEngineHome(home); err != nil {
		t.Fatalf("removeEngineHome: %v", err)
	}
	if _, err := os.Stat(home); !os.IsNotExist(err) {
		t.Fatalf("engine home still present: %v", err)
	}
}

func TestRemoveEngineHomeRefusesUserHome(t *testing.T) {
	userHome, err := os.UserHomeDir()
	if err != nil {
		t.Skip(err)
	}
	// Must not delete the user's home; a no-op is success.
	if err := removeEngineHome(userHome); err != nil {
		t.Fatalf("refusing user home should be a no-op, got %v", err)
	}
	if _, err := os.Stat(userHome); err != nil {
		t.Fatalf("user home was disturbed: %v", err)
	}
}
