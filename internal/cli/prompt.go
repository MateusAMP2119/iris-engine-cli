package cli

import (
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	"github.com/charmbracelet/bubbles/spinner"
	"github.com/charmbracelet/huh"
	"github.com/charmbracelet/lipgloss"
)

// confirmWithHuh runs a yes/no confirm via charmbracelet/huh. Used by
// uninstall (and other destructive flows) when a keyboard is available
// (stdin TTY, or /dev/tty under curl|bash). Returns errNotATerminal when
// interactive UI cannot attach so callers map to confirmation_required.
//
// Buttons are right-aligned to the ceremony mark column so No ends under [✓].
func confirmWithHuh(question string, out io.Writer) (bool, error) {
	in, closer := ceremonyReviewInput()
	if in == nil {
		return false, errNotATerminal
	}
	if closer != nil {
		defer closer.Close()
	}
	var ok bool
	confirm := newCeremonyConfirm(question, &ok)
	form := huh.NewForm(huh.NewGroup(confirm)).
		WithTheme(ceremonyConfirmTheme()).
		WithWidth(ceremonyConfirmWidth()).
		WithInput(in)
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

// ceremonySetupTheme pins title and description wrap to the install ceremony
// frame so setup selects/inputs share the same right edge as progress [✓].
func ceremonySetupTheme() *huh.Theme {
	theme := huh.ThemeCharm()
	width := ceremonyConfirmWidth()
	frame := theme.Focused.Base.GetHorizontalFrameSize()
	contentCols := width - frame
	if contentCols < 1 {
		contentCols = width
	}
	theme.Focused.Title = theme.Focused.Title.Width(contentCols)
	theme.Blurred.Title = theme.Blurred.Title.Width(contentCols)
	theme.Focused.Description = theme.Focused.Description.Width(contentCols)
	theme.Blurred.Description = theme.Blurred.Description.Width(contentCols)
	theme.Focused.SelectSelector = theme.Focused.SelectSelector.Width(contentCols)
	theme.Blurred.SelectSelector = theme.Blurred.SelectSelector.Width(contentCols)
	theme.Focused.Option = theme.Focused.Option.Width(contentCols)
	theme.Blurred.Option = theme.Blurred.Option.Width(contentCols)
	theme.Focused.TextInput.Placeholder = theme.Focused.TextInput.Placeholder.Width(contentCols)
	theme.Blurred.TextInput.Placeholder = theme.Blurred.TextInput.Placeholder.Width(contentCols)
	return theme
}

// newCeremonySetupForm builds a huh form sized to the install ceremony grid
// (indent + bullet + body + mark), matching progress/check lines.
func newCeremonySetupForm(groups ...*huh.Group) *huh.Form {
	return huh.NewForm(groups...).
		WithTheme(ceremonySetupTheme()).
		WithWidth(ceremonyConfirmWidth())
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
// keyboard is available. stdin may be a pipe (curl|bash); input falls back to
// /dev/tty so the menu still works. preselect (IRIS_ENGINE_SETUP) short-circuits.
func selectEngineSetup(preselect string, out io.Writer) (engineSetupChoice, error) {
	switch preselect {
	case "local":
		return setupLocal, nil
	case "remote":
		return setupRemote, nil
	case "skip":
		return setupSkip, nil
	}
	in, closer := ceremonyReviewInput()
	if in == nil {
		// Headless: no stdin TTY and no /dev/tty (CI, non-interactive shells).
		return setupSkip, nil
	}
	if closer != nil {
		defer closer.Close()
	}
	var choice string
	form := newCeremonySetupForm(
		huh.NewGroup(
			huh.NewSelect[string]().
				Title("Engine setup").
				Description("Local install on this machine, or connect to an existing engine.").
				Options(
					huh.NewOption("Local mode: install + start engine on this machine", "local"),
					huh.NewOption("Remote mode: connect to an existing engine", "remote"),
					huh.NewOption("Skip for now", "skip"),
				).
				Value(&choice),
		),
	).WithInput(in)
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
	in, closer := ceremonyReviewInput()
	if in == nil {
		return "", "", errNotATerminal
	}
	if closer != nil {
		defer closer.Close()
	}
	form := newCeremonySetupForm(
		huh.NewGroup(
			huh.NewInput().
				Title("Remote Iris endpoint (host:port)").
				Value(&host),
			huh.NewInput().
				Title("PAT token (optional)").
				EchoMode(huh.EchoModePassword).
				Value(&token),
		),
	).WithInput(in)
	if out != nil {
		form = form.WithOutput(out)
	}
	if runErr := form.Run(); runErr != nil {
		return "", "", runErr
	}
	return host, token, nil
}

// catalogSetupChoice is the post-install catalog menu selection.
type catalogSetupChoice int

const (
	catalogSetupPublic catalogSetupChoice = iota + 1
	catalogSetupCustom
	catalogSetupSkip
)

// selectCatalogSetup runs the installer's catalog menu through huh when a
// keyboard is available. preselect (IRIS_SETUP_CATALOGS / --catalogs) short-
// circuits: "public", "skip", or one-or-more comma-separated index URLs.
// Headless with no preselect skips (same posture as the engine menu).
func selectCatalogSetup(preselect string, out io.Writer) (catalogSetupChoice, []string, error) {
	preselect = strings.TrimSpace(preselect)
	if preselect != "" {
		switch strings.ToLower(preselect) {
		case "public":
			return catalogSetupPublic, nil, nil
		case "skip":
			return catalogSetupSkip, nil, nil
		}
		urls, err := parseCatalogSetupURLs(preselect)
		if err != nil {
			return 0, nil, err
		}
		return catalogSetupCustom, urls, nil
	}
	in, closer := ceremonyReviewInput()
	if in == nil {
		return catalogSetupSkip, nil, nil
	}
	if closer != nil {
		defer closer.Close()
	}
	var choice string
	form := newCeremonySetupForm(
		huh.NewGroup(
			huh.NewSelect[string]().
				Title("Pipeline catalog").
				Description("Collection of pre-built pipelines. Install by name after the engine is up. Custom catalogs can be added later.").
				Options(
					huh.NewOption("Public catalog", "public"),
					huh.NewOption("Custom catalog", "custom"),
					huh.NewOption("Skip for now", "skip"),
				).
				Value(&choice),
		),
	).WithInput(in)
	if out != nil {
		form = form.WithOutput(out)
	}
	if err := form.Run(); err != nil {
		if errors.Is(err, huh.ErrUserAborted) {
			return catalogSetupSkip, nil, nil
		}
		return catalogSetupSkip, nil, err
	}
	switch choice {
	case "public":
		return catalogSetupPublic, nil, nil
	case "custom":
		url, err := promptCatalogURL(out)
		if err != nil {
			return 0, nil, err
		}
		urls, err := parseCatalogSetupURLs(url)
		if err != nil {
			return 0, nil, err
		}
		return catalogSetupCustom, urls, nil
	default:
		return catalogSetupSkip, nil, nil
	}
}

// promptCatalogURL asks for one catalog index URL via huh.
func promptCatalogURL(out io.Writer) (string, error) {
	in, closer := ceremonyReviewInput()
	if in == nil {
		return "", errNotATerminal
	}
	if closer != nil {
		defer closer.Close()
	}
	var raw string
	form := newCeremonySetupForm(
		huh.NewGroup(
			huh.NewInput().
				Title("Catalog URL").
				Description("HTTPS URL of a catalog.json listing installable pipelines.").
				Placeholder("https://example.com/catalog.json").
				Value(&raw),
		),
	).WithInput(in)
	if out != nil {
		form = form.WithOutput(out)
	}
	if err := form.Run(); err != nil {
		return "", err
	}
	return strings.TrimSpace(raw), nil
}

// parseCatalogSetupURLs splits a comma-separated list of catalog index URLs and
// requires each to be an absolute http(s) URL.
func parseCatalogSetupURLs(raw string) ([]string, error) {
	parts := strings.Split(raw, ",")
	var urls []string
	for _, p := range parts {
		p = strings.TrimSpace(p)
		if p == "" {
			continue
		}
		if !strings.HasPrefix(p, "http://") && !strings.HasPrefix(p, "https://") {
			return nil, fmt.Errorf("catalog URL must be http(s): %q", p)
		}
		urls = append(urls, p)
	}
	if len(urls) == 0 {
		return nil, fmt.Errorf("catalog setup needs at least one index URL")
	}
	return urls, nil
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
