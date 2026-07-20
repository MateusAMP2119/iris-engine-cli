package cli

import (
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/charmbracelet/bubbles/viewport"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
	"golang.org/x/term"
)

// ceremony review chrome: one footer row; content uses the rest of the screen.
const ceremonyReviewFooter = "  ↑/k ↓/j  pgup/b pgdn/f  g/G top/end  q quit"

// reviewModel is a full-screen (alt-buffer) pager over a ceremony transcript.
type reviewModel struct {
	content string
	ready   bool
	vp      viewport.Model
	width   int
	height  int
}

func (m reviewModel) Init() tea.Cmd { return nil }

func (m reviewModel) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		m.width = msg.Width
		m.height = msg.Height
		headerH := 0
		footerH := 1
		if !m.ready {
			m.vp = viewport.New(msg.Width, maxInt(1, msg.Height-headerH-footerH))
			m.vp.YPosition = headerH
			m.vp.SetContent(m.content)
			// Start at the top so the user can scroll through from the beginning.
			m.vp.GotoTop()
			m.ready = true
		} else {
			m.vp.Width = msg.Width
			m.vp.Height = maxInt(1, msg.Height-headerH-footerH)
		}
		return m, nil
	case tea.KeyMsg:
		switch msg.String() {
		case "q", "esc", "ctrl+c":
			return m, tea.Quit
		case "g", "home":
			m.vp.GotoTop()
			return m, nil
		case "G", "end":
			m.vp.GotoBottom()
			return m, nil
		}
	}
	var cmd tea.Cmd
	m.vp, cmd = m.vp.Update(msg)
	return m, cmd
}

func (m reviewModel) View() string {
	if !m.ready {
		return "\n  Loading ceremony scrollback…"
	}
	footer := lipgloss.NewStyle().
		Foreground(lipgloss.Color("8")).
		Render(m.footerLine())
	return m.vp.View() + "\n" + footer
}

func (m reviewModel) footerLine() string {
	pct := int(m.vp.ScrollPercent() * 100)
	info := fmt.Sprintf("  %d%%", pct)
	help := ceremonyReviewFooter
	// Prefer help; shrink if the terminal is narrow.
	pad := maxInt(1, m.width-lipgloss.Width(help)-lipgloss.Width(info))
	line := help + strings.Repeat(" ", pad) + info
	if lipgloss.Width(line) > m.width && m.width > 0 {
		return truncateDisplay(help, m.width)
	}
	return line
}

func truncateDisplay(s string, width int) string {
	if width <= 0 || lipgloss.Width(s) <= width {
		return s
	}
	// Walk runes conservatively; lipgloss.Width handles ANSI-free help text.
	var b strings.Builder
	w := 0
	for _, r := range s {
		rw := lipgloss.Width(string(r))
		if w+rw > width-1 {
			b.WriteString("…")
			break
		}
		b.WriteRune(r)
		w += rw
	}
	return b.String()
}

func maxInt(a, b int) int {
	if a > b {
		return a
	}
	return b
}

// runCeremonyReview opens an alt-screen pager over content. No-ops when out is
// not a TTY or content is empty. Callers that already streamed the ceremony to
// the primary screen keep that scrollback; the pager is a temporary overlay.
//
// Input prefers os.Stdin when it is a TTY; otherwise it falls back to /dev/tty
// so `curl … | bash` installs can still scroll, quit, and answer setup prompts.
func runCeremonyReview(out io.Writer, content string) {
	content = strings.TrimRight(content, "\n")
	if content == "" || !writerIsTTY(out) {
		return
	}
	in, closer := ceremonyReviewInput()
	if in == nil {
		return
	}
	if closer != nil {
		defer closer.Close()
	}
	m := reviewModel{content: content}
	opts := []tea.ProgramOption{
		tea.WithOutput(out),
		tea.WithInput(in),
		tea.WithAltScreen(),
		tea.WithMouseCellMotion(), // wheel scroll in the pager
	}
	p := tea.NewProgram(m, opts...)
	if _, err := p.Run(); err != nil {
		// Review is best-effort; never fail the ceremony over a pager glitch.
		fmt.Fprintln(out, "  (ceremony scrollback unavailable)")
	}
}

// ceremonyReviewInput returns a keyboard reader for interactive UI: real stdin
// when it is a TTY, else /dev/tty. Used by the ceremony pager and by setup /
// confirm forms under `curl … | bash` (stdin is the script pipe). closer is
// non-nil only when /dev/tty was opened.
func ceremonyReviewInput() (in io.Reader, closer io.Closer) {
	if stdinLooksLikeTTY() {
		return os.Stdin, nil
	}
	tty, err := os.OpenFile("/dev/tty", os.O_RDONLY, 0)
	if err != nil {
		return nil, nil
	}
	return tty, tty
}

// maybeReviewCeremony opens the pager when the transcript is taller than the
// terminal (otherwise there is nothing useful to scroll). Honors
// IRIS_NO_CEREMONY_REVIEW and parent-owned $IRIS_CEREMONY_LOG.
func maybeReviewCeremony(out io.Writer, content string) {
	if ceremonyReviewDisabled() {
		return
	}
	content = strings.TrimRight(content, "\n")
	if content == "" || !writerIsTTY(out) {
		return
	}
	if in, closer := ceremonyReviewInput(); in == nil {
		return
	} else if closer != nil {
		_ = closer.Close()
	}
	rows := termRows(out)
	nLines := strings.Count(content, "\n") + 1
	// Leave one row for the footer in the comparison so "exactly fills" still skips.
	if nLines <= rows-1 {
		return
	}
	runCeremonyReview(out, content)
}

// termRows reports the terminal height for out, or 24 when unknown.
func termRows(out io.Writer) int {
	f, ok := out.(*os.File)
	if !ok {
		return 24
	}
	_, rows, err := term.GetSize(int(f.Fd()))
	if err != nil || rows <= 0 {
		return 24
	}
	return rows
}
