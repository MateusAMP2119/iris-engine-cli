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

// Ceremony layout — one grid for step checks, progress bars, and yes/no
// confirms so the trailing mark column lines up everywhere:
//
//   - {body padded to ceremonyBodyCols}{mark right-aligned in ceremonyMarkCols}
//
// mark is either "[✓]" or "{bar} {pct}". Short marks (checks) are right-aligned
// inside the mark column so their right edge matches the progress bar's "100%".
// Body fits the longest progress labels (~26) with a little slack; longer status
// lines overflow and keep a one-space gap before the mark. The confirm form
// width is ceremonyConfirmWidth so a right-aligned No button ends on that edge.
const (
	ceremonyIndent   = "  "
	ceremonyBullet   = "• "
	ceremonyBodyCols = 32
	progressBarCols  = 16
	progressPctCols  = 4 // "  0%" .. "100%"
	// ceremonyMarkCols is bar + space + pct — the full progress trailing mark.
	ceremonyMarkCols = progressBarCols + 1 + progressPctCols
)

// ceremonyConfirmWidth is the huh form width that places a right-aligned No
// button on the same column as the ceremony mark column's right edge (see
// confirmWithHuh). Use lipgloss.Width for multi-byte indent/bullet cells.
// huh's focused Base draws a left border outside Style.Width; stretching the
// title to width−frame and right-aligning the buttons makes the No box end on
// the mark.
func ceremonyConfirmWidth() int {
	return lipgloss.Width(ceremonyIndent) + lipgloss.Width(ceremonyBullet) + ceremonyBodyCols + ceremonyMarkCols
}

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

// ceremonyLineWidth is the display width of a full ceremony line
// (indent + bullet + body + mark column).
func ceremonyLineWidth() int {
	return lipgloss.Width(ceremonyIndent) + lipgloss.Width(ceremonyBullet) + ceremonyBodyCols + ceremonyMarkCols
}

// padCeremonyBody pads left text so left+pad fills ceremonyBodyCols display cells.
func padCeremonyBody(left string) string {
	w := lipgloss.Width(left)
	if w >= ceremonyBodyCols {
		return left
	}
	return left + strings.Repeat(" ", ceremonyBodyCols-w)
}

// padCeremonyMark right-aligns mark inside ceremonyMarkCols so [✓] and
// "{bar} 100%" share one right edge. Wider marks are returned unchanged.
func padCeremonyMark(mark string) string {
	if mark == "" {
		return ""
	}
	w := lipgloss.Width(mark)
	if w >= ceremonyMarkCols {
		return mark
	}
	return strings.Repeat(" ", ceremonyMarkCols-w) + mark
}

// formatCeremonyLine builds "  • {body}{mark}" with body width ceremonyBodyCols
// and mark right-aligned in ceremonyMarkCols. When body text is wider than
// ceremonyBodyCols, a single space separates body and mark (right edge may
// extend past the usual column for that rare long status line).
func formatCeremonyLine(bodyLeft, mark string) string {
	prefix := ceremonyIndent + ceremonyBullet
	if mark == "" {
		return prefix + bodyLeft
	}
	bodyW := lipgloss.Width(bodyLeft)
	markW := lipgloss.Width(mark)
	// Prefer the fixed grid; fall back to a one-space gap when body overflows.
	gap := ceremonyBodyCols + ceremonyMarkCols - bodyW - markW
	if gap < 1 {
		gap = 1
	}
	return prefix + bodyLeft + strings.Repeat(" ", gap) + mark
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

// progressFinalLine is the settled 100% ceremony line for prefix (no reprint).
func progressFinalLine(prefix string) string {
	m := newProgressModel(prefix)
	m.percent = 1
	return formatCeremonyLine(m.label, m.mark(100))
}

// runProgressBar runs a Bubble Tea progress bar to completion on out.
// No-ops when out is not a terminal (json/piped runs stay quiet). On success
// the final line is mirrored to $IRIS_CEREMONY_LOG when set (animation frames
// are not logged).
func runProgressBar(out io.Writer, prefix string) {
	if !writerIsTTY(out) {
		// Still record a plain done line for shared install transcripts.
		appendCeremonyLogFile(progressFinalLine(prefix))
		return
	}
	m := newProgressModel(prefix)
	p := tea.NewProgram(m, tea.WithOutput(out), tea.WithInput(nil))
	if _, err := p.Run(); err != nil {
		line := formatCeremonyLine(m.label, "done")
		fmt.Fprintln(out, line)
		appendCeremonyLogFile(line)
		return
	}
	appendCeremonyLogFile(progressFinalLine(prefix))
}

// workResultMsg is delivered when a runProgressWhile job finishes.
type workResultMsg struct{ err error }

// workPollMsg re-arms the wait/poll loop while the job is still running.
type workPollMsg struct{}

// workProgressModel is a ceremony bar that creeps while real work runs, then
// snaps to 100% when the job returns. Avoids the "bar done, then long silence"
// feel of a cosmetic fill that finishes before install/start begin.
type workProgressModel struct {
	label    string
	bar      progress.Model
	percent  float64
	quitting bool
	err      error
	done     <-chan error
}

func newWorkProgressModel(label string, done <-chan error) workProgressModel {
	bar := progress.New(
		progress.WithDefaultGradient(),
		progress.WithWidth(progressBarCols),
		progress.WithoutPercentage(),
	)
	return workProgressModel{
		label: strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(label), "•")),
		bar:   bar,
		done:  done,
	}
}

func (m workProgressModel) Init() tea.Cmd {
	return tea.Batch(m.bar.Init(), waitWork(m.done))
}

// waitWork returns as soon as the job finishes, or after a short tick so the
// bar can keep creeping while work is still in flight.
func waitWork(done <-chan error) tea.Cmd {
	return func() tea.Msg {
		select {
		case err := <-done:
			return workResultMsg{err: err}
		case <-time.After(80 * time.Millisecond):
			return workPollMsg{}
		}
	}
}

func (m workProgressModel) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		return m, nil
	case workPollMsg:
		// Asymptotic crawl toward ~92% so long jobs never look frozen at 100% early.
		if m.percent < 0.92 {
			// Larger steps early, smaller later: remaining * 0.06 per poll.
			m.percent += (0.92 - m.percent) * 0.06
			if m.percent > 0.92 {
				m.percent = 0.92
			}
		}
		cmd := m.bar.SetPercent(m.percent)
		return m, tea.Batch(cmd, waitWork(m.done))
	case workResultMsg:
		m.err = msg.err
		m.percent = 1
		m.quitting = true
		cmd := m.bar.SetPercent(1)
		return m, tea.Batch(cmd, func() tea.Msg { return progressDone{} })
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

func (m workProgressModel) View() string {
	pct := int(m.percent * 100)
	if pct > 100 {
		pct = 100
	}
	// Reuse progressModel.mark math via a throwaway model with the same bar state.
	pm := progressModel{label: m.label, bar: m.bar, percent: m.percent}
	line := formatCeremonyLine(m.label, pm.mark(pct))
	if m.quitting && m.percent >= 1 {
		return line + "\n"
	}
	return line
}

// runProgressWhile shows a ceremony progress bar while work runs. The bar creeps
// until work returns, then settles at 100%. Non-TTY: runs work with no animation.
// The final settled line is appended to $IRIS_CEREMONY_LOG when set.
func runProgressWhile(out io.Writer, prefix string, work func() error) error {
	if work == nil {
		work = func() error { return nil }
	}
	if !writerIsTTY(out) {
		err := work()
		if err == nil {
			appendCeremonyLogFile(progressFinalLine(prefix))
		}
		return err
	}
	done := make(chan error, 1)
	go func() { done <- work() }()

	m := newWorkProgressModel(prefix, done)
	p := tea.NewProgram(m, tea.WithOutput(out), tea.WithInput(nil))
	final, err := p.Run()
	wm, _ := final.(workProgressModel)
	if err != nil {
		// Program failed to render. Prefer the job error already captured by the
		// model; otherwise wait for the worker (channel still full only if the
		// model never observed the result).
		workErr := wm.err
		if !wm.quitting {
			workErr = <-done
		}
		line := formatCeremonyLine(m.label, "done")
		fmt.Fprintln(out, line)
		appendCeremonyLogFile(line)
		return workErr
	}
	appendCeremonyLogFile(progressFinalLine(prefix))
	return wm.err
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
