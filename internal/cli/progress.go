package cli

import (
	"fmt"
	"io"
	"os"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/charmbracelet/bubbles/progress"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
)

// Ceremony layout — one grid for step checks and progress bars so the trailing
// mark column lines up everywhere:
//
//	  • {body padded to ceremonyBodyCols}{mark}
//
// mark is either "[✓]" or "{bar} {pct}". Body width matches the historical
// uninstallStepColumn (52).
const (
	ceremonyIndent   = "  "
	ceremonyBullet   = "• "
	ceremonyBodyCols = 52
	progressBarCols  = 24
	progressPctCols  = 4 // "  0%" .. "100%"
)

// progressTick advances the bar on a fixed cadence (platform-independent).
type progressTick time.Time

// progressModel is a short-lived Bubble Tea program: one labeled bar that
// fills 0→100% then quits. Used by uninstall and setup so ceremony looks the
// same on every platform — no raw ANSI \r loops.
type progressModel struct {
	label    string // without bullet; padded into the body column with the bar
	bar      progress.Model
	percent  float64
	quitting bool
	step     float64
}

type progressDone struct{}

func newProgressModel(label string) progressModel {
	bar := progress.New(
		progress.WithDefaultGradient(),
		progress.WithWidth(progressBarCols),
		progress.WithoutPercentage(),
	)
	return progressModel{
		label: strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(label), "•")),
		bar:   bar,
		step:  0.08,
	}
}

func formatProgressPct(pct int) string {
	if pct < 0 {
		pct = 0
	}
	if pct > 100 {
		pct = 100
	}
	// width progressPctCols including '%'
	return fmt.Sprintf("%*d%%", progressPctCols-1, pct)
}

// ceremonyMark is the trailing status after the shared body column: a check or a bar+pct.
func ceremonyCheckMark(check string) string {
	return "[" + check + "]"
}

func (m progressModel) mark(pct int) string {
	barView := m.bar.View()
	if pct >= 100 {
		barView = m.bar.ViewAs(1)
	}
	return barView + " " + formatProgressPct(pct)
}

// padCeremonyBody pads left text so left+pad fills ceremonyBodyCols display cells.
func padCeremonyBody(left string) string {
	w := lipgloss.Width(left)
	if w >= ceremonyBodyCols {
		return left
	}
	return left + strings.Repeat(" ", ceremonyBodyCols-w)
}

// formatCeremonyLine builds "  • {body}{mark}" with body width ceremonyBodyCols.
// When mark is wider than a simple [✓], it still starts at the same column as checks.
func formatCeremonyLine(bodyLeft, mark string) string {
	return ceremonyIndent + ceremonyBullet + padCeremonyBody(bodyLeft) + mark
}

func (m progressModel) Init() tea.Cmd {
	return tea.Batch(m.bar.Init(), tickProgress())
}

func tickProgress() tea.Cmd {
	return tea.Tick(40*time.Millisecond, func(t time.Time) tea.Msg {
		return progressTick(t)
	})
}

func (m progressModel) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		return m, nil
	case progressTick:
		m.percent += m.step
		if m.percent >= 1 {
			m.percent = 1
			m.quitting = true
			cmd := m.bar.SetPercent(1)
			return m, tea.Batch(cmd, func() tea.Msg { return progressDone{} })
		}
		cmd := m.bar.SetPercent(m.percent)
		return m, tea.Batch(cmd, tickProgress())
	case progress.FrameMsg:
		var cmd tea.Cmd
		var prog tea.Model
		prog, cmd = m.bar.Update(msg)
		m.bar = prog.(progress.Model)
		return m, cmd
	case progressDone:
		return m, tea.Quit
	case tea.KeyMsg:
		return m, nil
	}
	return m, nil
}

func (m progressModel) View() string {
	pct := int(m.percent * 100)
	if pct > 100 {
		pct = 100
	}
	line := formatCeremonyLine(m.label, m.mark(pct))
	if m.quitting && m.percent >= 1 {
		return line + "\n"
	}
	return line
}

// runProgressBar runs a Bubble Tea progress bar to completion on out.
// No-ops when out is not a terminal (json/piped runs stay quiet).
func runProgressBar(out io.Writer, prefix string) {
	if !writerIsTTY(out) {
		return
	}
	m := newProgressModel(prefix)
	p := tea.NewProgram(m, tea.WithOutput(out), tea.WithInput(nil))
	if _, err := p.Run(); err != nil {
		fmt.Fprint(out, formatCeremonyLine(m.label, "done")+"\n")
	}
}

func writerIsTTY(out io.Writer) bool {
	f, ok := out.(*os.File)
	if !ok {
		return false
	}
	stat, err := f.Stat()
	if err != nil {
		return false
	}
	return stat.Mode()&os.ModeCharDevice != 0
}

// keep utf8 available for callers that share this file's layout helpers
var _ = utf8.RuneCountInString
