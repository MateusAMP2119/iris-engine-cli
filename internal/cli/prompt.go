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
func confirmWithHuh(question string, out io.Writer) (bool, error) {
	if !stdinLooksLikeTTY() {
		return false, errNotATerminal
	}
	var ok bool
	form := huh.NewForm(
		huh.NewGroup(
			huh.NewConfirm().
				Title(question).
				Affirmative("Yes").
				Negative("No").
				Value(&ok),
		),
	).WithTheme(huh.ThemeCharm())
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