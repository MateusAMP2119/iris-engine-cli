package cli

import (
	"errors"
	"io"
	"os"
	"time"

	"github.com/charmbracelet/bubbles/spinner"
	"github.com/charmbracelet/huh"
	"github.com/charmbracelet/lipgloss"
)

// confirmWithHuh runs a yes/no confirm via charmbracelet/huh. Used by
// uninstall (and other destructive flows) when stdin/stdout are real TTYs.
// Returns errNotATerminal when interactive UI cannot attach so callers map to
// the standard confirmation_required refusal.
//
// Buttons are right-aligned to the ceremony mark column so No ends under [✓].
func confirmWithHuh(question string, out io.Writer) (bool, error) {
	if !stdinLooksLikeTTY() {
		return false, errNotATerminal
	}
	var ok bool
	confirm := newCeremonyConfirm(question, &ok)
	form := huh.NewForm(huh.NewGroup(confirm)).
		WithTheme(ceremonyConfirmTheme()).
		WithWidth(ceremonyConfirmWidth())
	if out != nil {
		form = form.WithOutput(out)
	}
	if err := form.Run(); err != nil {
		if errors.Is(err, huh.ErrUserAborted) {
			return false, nil
		}
		return false, err
	}
	return ok, nil
}

// ceremonyConfirmTheme is ThemeCharm with title width and button margins tuned
// so right-aligned Yes/No land on the ceremony [✓] column (see progress.go).
func ceremonyConfirmTheme() *huh.Theme {
	theme := huh.ThemeCharm()
	width := ceremonyConfirmWidth()
	frame := theme.Focused.Base.GetHorizontalFrameSize()
	titleCols := width - frame
	if titleCols < 1 {
		titleCols = width
	}
	// Stretch the title so Confirm.View's button row uses the full content
	// width (otherwise right-align only spans the title text).
	theme.Focused.Title = theme.Focused.Title.Width(titleCols)
	theme.Blurred.Title = theme.Blurred.Title.Width(titleCols)
	// Drop trailing button margin so the No box is flush with the form edge
	// (default MarginRight(1) leaves a gap after No past the mark).
	focusBtn := theme.Focused.FocusedButton.MarginRight(0)
	blurBtn := theme.Focused.BlurredButton.MarginRight(0)
	theme.Focused.FocusedButton = focusBtn
	theme.Focused.BlurredButton = blurBtn
	theme.Blurred.FocusedButton = focusBtn
	theme.Blurred.BlurredButton = blurBtn
	return theme
}

// newCeremonyConfirm builds the yes/no field used by confirmWithHuh. Theme and
// width are applied by the form (or by tests that call View directly).
func newCeremonyConfirm(question string, value *bool) *huh.Confirm {
	return huh.NewConfirm().
		Title(question).
		Affirmative("Yes").
		Negative("No").
		WithButtonAlignment(lipgloss.Right).
		Value(value)
}

func stdinLooksLikeTTY() bool {
	stat, err := os.Stdin.Stat()
	if err != nil {
		return false
	}
	return stat.Mode()&os.ModeCharDevice != 0
}

// engineSetupChoice is the post-install menu selection.
type engineSetupChoice int

const (
	setupLocal engineSetupChoice = iota + 1
	setupRemote
	setupSkip
)

// selectEngineSetup runs the installer's engine-setup menu through huh when a
// TTY is available. preselect (from IRIS_ENGINE_SETUP) short-circuits the form.
func selectEngineSetup(preselect string, out io.Writer) (engineSetupChoice, error) {
	switch preselect {
	case "local":
		return setupLocal, nil
	case "remote":
		return setupRemote, nil
	case "skip":
		return setupSkip, nil
	}
	if !stdinLooksLikeTTY() {
		return setupSkip, nil
	}
	var choice string
	form := huh.NewForm(
		huh.NewGroup(
			huh.NewSelect[string]().
				Title("Engine setup").
				Description("How should Iris reach a data plane?").
				Options(
					huh.NewOption("Local mode — install + start engine on this machine", "local"),
					huh.NewOption("Remote mode — connect to an existing engine", "remote"),
					huh.NewOption("Skip for now", "skip"),
				).
				Value(&choice),
		),
	).WithTheme(huh.ThemeCharm())
	if out != nil {
		form = form.WithOutput(out)
	}
	if err := form.Run(); err != nil {
		if errors.Is(err, huh.ErrUserAborted) {
			return setupSkip, nil
		}
		return setupSkip, err
	}
	switch choice {
	case "local":
		return setupLocal, nil
	case "remote":
		return setupRemote, nil
	default:
		return setupSkip, nil
	}
}

// promptRemoteEndpoint asks for host and optional PAT via huh.
func promptRemoteEndpoint(out io.Writer) (host, token string, err error) {
	form := huh.NewForm(
		huh.NewGroup(
			huh.NewInput().
				Title("Remote Iris endpoint (host:port)").
				Value(&host),
			huh.NewInput().
				Title("PAT token (optional)").
				EchoMode(huh.EchoModePassword).
				Value(&token),
		),
	).WithTheme(huh.ThemeCharm())
	if out != nil {
		form = form.WithOutput(out)
	}
	if runErr := form.Run(); runErr != nil {
		return "", "", runErr
	}
	return host, token, nil
}

// lipgloss painter helpers for uninstall ceremony (alongside the existing ANSI painter).
func lipglossHeader(title string, color bool) string {
	if !color {
		return title
	}
	return lipgloss.NewStyle().Foreground(lipgloss.Color("14")).Bold(true).Render(title)
}

func lipglossOK(s string, color bool) string {
	if !color {
		return s
	}
	return lipgloss.NewStyle().Foreground(lipgloss.Color("10")).Bold(true).Render(s)
}

// newSetupSpinner returns a bubbles spinner for long setup steps.
func newSetupSpinner() spinner.Model {
	s := spinner.New()
	s.Spinner = spinner.Dot
	s.Style = lipgloss.NewStyle().Foreground(lipgloss.Color("14"))
	return s
}

// spinOnce advances a spinner model once (used by tests and short waits).
func spinOnce(s spinner.Model) spinner.Model {
	s, _ = s.Update(spinner.Tick())
	_ = time.Millisecond
	return s
}