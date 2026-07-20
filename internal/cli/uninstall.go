package cli

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"math/rand/v2"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"unicode/utf8"

	"github.com/charmbracelet/lipgloss"
	"github.com/spf13/cobra"

	"github.com/MateusAMP2119/iris-lakehouse/internal/buildinfo"
	"github.com/MateusAMP2119/iris-lakehouse/internal/config"
	"github.com/MateusAMP2119/iris-lakehouse/internal/daemon"
)

// errNotATerminal reports that stdin cannot host the interactive y/N confirmation.
var errNotATerminal = errors.New("stdin is not a terminal")

// The fixed step names and count of the staged uninstall sequence.
const (
	stepStopEngine  = "stop_engine"
	stepEngineState = "remove_engine_state"
	stepBinary      = "remove_binary"
	uninstallSteps  = 3
)

// uninstallStepColumn is kept as an alias of ceremonyBodyCols so step [✓]
// marks share the progress-bar mark column's right edge (see progress.go).
const uninstallStepColumn = ceremonyBodyCols

// uninstallStep is one step's outcome in the --json envelope.
type uninstallStep struct {
	Step    int      `json:"step"`
	Name    string   `json:"name"`
	Status  string   `json:"status"`
	Removed []string `json:"removed,omitempty"`
}

// uninstallCmdResult is the --json data envelope of `iris uninstall`: final outcome, version, binary path, per-step statuses.
type uninstallCmdResult struct {
	Status  string          `json:"status"`
	Version string          `json:"version,omitempty"`
	Path    string          `json:"path,omitempty"`
	Steps   []uninstallStep `json:"steps,omitempty"`
}

// uninstallCmd builds `iris uninstall`: the staged complete uninstall (stop engine, remove engine state, remove binary), gated by --yes/--force.
func (a *app) uninstallCmd() *cobra.Command {
	c := &cobra.Command{
		Use:   "uninstall",
		Short: "Completely uninstall iris: stop the engine, remove engine state, remove the binary",
		Args:  cobra.NoArgs,
		RunE:  a.uninstallSelf(),
	}
	addConfirmFlags(c)
	return daemonless(c)
}

// uninstallSelf runs the staged sequence: a decline aborts the rest clean (exit 0), a failure exits 4 naming the step, success closes with a random farewell quote.
func (a *app) uninstallSelf() runE {
	return func(cmd *cobra.Command, _ []string) error {
		yes, _ := cmd.Flags().GetBool("yes")
		force, _ := cmd.Flags().GetBool("force")
		jsonMode, _ := cmd.Flags().GetBool("json")
		p := a.newPainter(jsonMode)
		settings := a.resolveTarget(cmd)
		ctx := cmd.Context()
		if ctx == nil {
			ctx = context.Background()
		}

		// The binary path feeds step 3's prompt and every outcome envelope.
		resolve := a.executablePath
		if resolve == nil {
			resolve = resolveSelfPath
		}
		path, err := resolve()
		if err != nil {
			return &fault{code: exitOpFailed, codeStr: "uninstall_failed", message: fmt.Sprintf("iris uninstall: %v", err)}
		}

		var steps []uninstallStep
		// Transcript for post-run scrollback (TTY only; skipped under --json).
		log := newCeremonyLog(a.out)
		say := func(format string, args ...any) {
			if !jsonMode {
				log.printf(format, args...)
			}
		}
		done := func(text string) {
			if !jsonMode {
				mark := ceremonyCheckMark(p.green("✓"))
				log.line(formatCeremonyLine(text, mark))
			}
		}
		review := func() {
			if !jsonMode {
				maybeReviewCeremony(a.out, log.content())
			}
		}

		say("")
		if p.enabled {
			a.writeUninstallHeaderBox(p, buildinfo.Version, log)
		} else {
			say("[IRIS UNINSTALL %s]", buildinfo.Version)
		}
		say("")
		say("  🔧 Starting complete uninstallation sequence...")
		say("")

		// Step 1/3: stop a recorded detached daemon; nothing recorded and nothing reachable passes clean.
		say("  %s", p.cyan("[1/3] Stopping Iris Engine"))
		say("  • Checking for running processes...")
		stopped := false
		if pid, perr := daemon.ReadPIDFile(settings); perr == nil {
			sctx, cancel := context.WithTimeout(ctx, stopGraceTimeout)
			serr := daemon.StopDaemon(sctx, settings, pid)
			cancel()
			if serr != nil {
				a.logger.Error("uninstall: engine stop failed", "err", serr)
				return &fault{
					code:    exitOpFailed,
					codeStr: "stop_failed",
					message: fmt.Sprintf("iris uninstall: step 1/%d (stop engine) failed: %v", uninstallSteps, serr),
				}
			}
			stopped = true
		}
		switch {
		case a.probeDaemon(ctx, settings) == nil:
			// Still reachable means no pid to signal (foreground daemon, or not the recorded one); only --force proceeds past it.
			if !force {
				return &fault{
					code:    exitOpFailed,
					codeStr: "daemon_reachable",
					message: fmt.Sprintf(`iris uninstall: step 1/%d (stop engine) failed: a running iris daemon is reachable but was not started detached, so it cannot be stopped here; stop it where it runs (Ctrl-C, or "iris engine stop"), or re-run with --force to leave it running`, uninstallSteps),
				}
			}
			steps = append(steps, uninstallStep{Step: 1, Name: stepStopEngine, Status: "left_running"})
			done("Daemon left running (--force).")
		case stopped:
			steps = append(steps, uninstallStep{Step: 1, Name: stepStopEngine, Status: "stopped"})
			done("Iris engine stopped successfully.")
		default:
			steps = append(steps, uninstallStep{Step: 1, Name: stepStopEngine, Status: "nothing_to_stop"})
			done("No running engine; nothing to stop.")
		}
		say("")

		// Step 2/3: remove the on-disk engine state (the `iris engine uninstall` set); absent state skips without a prompt.
		say("  %s", p.cyan("[2/3] Removing Engine State"))
		if !daemon.EngineArtifactsPresent(settings) {
			steps = append(steps, uninstallStep{Step: 2, Name: stepEngineState, Status: "nothing_to_remove"})
			done("No engine state on disk; nothing to remove.")
		} else {
			where := ""
			if settings.Socket != "" {
				where = " under " + filepath.Dir(settings.Socket)
			}
			ok, cerr := a.uninstallConsent(fmt.Sprintf("Remove engine state%s?", where), yes, force)
			if cerr != nil {
				return cerr
			}
			if !ok {
				steps = append(steps,
					uninstallStep{Step: 2, Name: stepEngineState, Status: "declined"},
					uninstallStep{Step: 3, Name: stepBinary, Status: "skipped"})
				err := a.uninstallAborted(p, jsonMode, path, steps)
				review()
				return err
			}
			removed, rerr := daemon.RemoveEngineArtifacts(settings)
			if rerr != nil {
				a.logger.Error("uninstall: engine state removal failed", "err", rerr)
				return &fault{
					code:    exitOpFailed,
					codeStr: "uninstall_failed",
					message: fmt.Sprintf("iris uninstall: step 2/%d (remove engine state) failed: %v", uninstallSteps, rerr),
				}
			}
			if !jsonMode && p.enabled {
				a.uninstallProgressBar(p, "• Removing engine state")
				// Bar already drew the final line; record without reprinting.
				// (runProgressBar also appends to $IRIS_CEREMONY_LOG when set.)
				log.note(progressFinalLine("• Removing engine state"))
			}
			steps = append(steps, uninstallStep{Step: 2, Name: stepEngineState, Status: "removed", Removed: removed})
			done("Engine state removed.")
		}
		say("")

		// Step 3/3: remove the running binary itself.
		say("  %s", p.cyan("[3/3] Uninstalling Iris CLI"))
		ok, cerr := a.uninstallConsent(fmt.Sprintf("Uninstall cli %s from %s?", buildinfo.Version, path), yes, force)
		if cerr != nil {
			return cerr
		}
		if !ok {
			steps = append(steps, uninstallStep{Step: 3, Name: stepBinary, Status: "declined"})
			err := a.uninstallAborted(p, jsonMode, path, steps)
			review()
			return err
		}
		if err := os.Remove(path); err != nil {
			if errors.Is(err, fs.ErrPermission) {
				return &fault{
					code:    exitOpFailed,
					codeStr: "permission_denied",
					message: fmt.Sprintf("iris uninstall: step 3/%d (remove binary) failed: cannot remove %s: permission denied; re-run with sudo", uninstallSteps, path),
				}
			}
			return &fault{
				code:    exitOpFailed,
				codeStr: "uninstall_failed",
				message: fmt.Sprintf("iris uninstall: step 3/%d (remove binary) failed: remove %s: %v", uninstallSteps, path, err),
			}
		}
		steps = append(steps, uninstallStep{Step: 3, Name: stepBinary, Status: "removed", Removed: []string{path}})
		if err := removeInstallerSymlink(path); err != nil && !jsonMode {
			say("  %s %s", p.dim("!"), err.Error())
		}
		removeShellPathEntries()
		// Wipe the engine home (workspace, empty bin/, iris.toml, any leftover
		// leaves) so "Traces erased" means nothing remains under ~/.iris.
		if home := engineHomeDir(settings); home != "" {
			if err := removeEngineHome(home); err != nil && !jsonMode {
				say("  %s could not remove engine home %s: %v", p.dim("!"), home, err)
			}
		}
		if !jsonMode && p.enabled {
			a.uninstallProgressBar(p, "• Removing binary and traces")
			log.note(progressFinalLine("• Removing binary and traces"))
		}
		done("Binary removed")
		done("Traces erased")

		if jsonMode {
			return json.NewEncoder(a.out).Encode(dataEnvelope{Data: uninstallCmdResult{
				Status: "uninstalled", Version: buildinfo.Version, Path: path, Steps: steps,
			}})
		}
		say("")
		a.writeFarewellQuote(p, log)
		review()
		return nil
	}
}

// uninstallConsent gates one step: --yes/--force pass, otherwise one y/N through the confirm seam; no terminal maps to the standard consent-required refusal.
func (a *app) uninstallConsent(question string, yes, force bool) (bool, error) {
	if yes || force {
		return true, nil
	}
	confirmFn := a.confirm
	if confirmFn == nil {
		confirmFn = a.terminalConfirm
	}
	ok, err := confirmFn(question, false)
	if err != nil {
		return false, &fault{
			code:    exitOpFailed,
			codeStr: "confirmation_required",
			message: `iris uninstall is destructive; re-run with --yes to confirm, or run it in a terminal to confirm interactively`,
		}
	}
	return ok, nil
}

// uninstallStepDone prints one completed-step line with the [✓] mark
// right-aligned in the shared ceremony mark column (same right edge as bars).
func (a *app) uninstallStepDone(p painter, text string) {
	mark := ceremonyCheckMark(p.green("✓"))
	// Plain width for padding must ignore ANSI in the mark; pad the text only.
	line := formatCeremonyLine(text, mark)
	fmt.Fprintln(a.out, line)
	appendCeremonyLogFile(line)
}

// uninstallAborted reports a declined step: remaining steps skipped, exit clean (0), the outcome says what was and was not removed.
func (a *app) uninstallAborted(p painter, jsonMode bool, path string, steps []uninstallStep) error {
	if jsonMode {
		return json.NewEncoder(a.out).Encode(dataEnvelope{Data: uninstallCmdResult{
			Status: "aborted", Version: buildinfo.Version, Path: path, Steps: steps,
		}})
	}
	engineRemoved := false
	for _, s := range steps {
		if s.Name == stepEngineState && s.Status == "removed" {
			engineRemoved = true
		}
	}
	fmt.Fprintln(a.out)
	if engineRemoved {
		fmt.Fprintf(a.out, "%s Engine state removed; the iris binary stays at %s.\n", p.green("  Aborted."), path)
		return nil
	}
	fmt.Fprintf(a.out, "%s Nothing removed. The iris binary stays at %s.\n", p.green("  Aborted."), path)
	return nil
}

// uninstallProgressBar animates a removal step via Bubble Tea + bubbles/progress
// so the bar looks the same on every platform (no raw ANSI \r loops). Piped and
// --json runs draw nothing.
func (a *app) uninstallProgressBar(p painter, prefix string) {
	if !p.enabled {
		return
	}
	runProgressBar(a.out, prefix)
}

// uninstallHeaderBox draws the cyan header box on a terminal, version in magenta; borders sized on the unstyled interior so escapes never skew alignment.
func (a *app) uninstallHeaderBox(p painter, version string) {
	a.writeUninstallHeaderBox(p, version, nil)
}

// writeUninstallHeaderBox is uninstallHeaderBox with an optional ceremonyLog.
// When log is nil, lines go to a.out and the shared ceremony log file only.
func (a *app) writeUninstallHeaderBox(p painter, version string, log *ceremonyLog) {
	const leftPad, rightPad = "   ", "  "
	plainInner := leftPad + "IRIS UNINSTALL " + version + rightPad
	rule := strings.Repeat("═", utf8.RuneCountInString(plainInner))
	styledInner := leftPad + "IRIS UNINSTALL " + p.magenta(version) + rightPad
	bar := p.cyan("║")

	top := p.cyan("  ╔" + rule + "╗")
	mid := fmt.Sprintf("  %s%s%s", bar, styledInner, bar)
	bot := p.cyan("  ╚" + rule + "╝")
	if log != nil {
		log.line(top)
		log.line(mid)
		log.line(bot)
		return
	}
	fmt.Fprintln(a.out, top)
	fmt.Fprintln(a.out, mid)
	fmt.Fprintln(a.out, bot)
	appendCeremonyLogFile(top)
	appendCeremonyLogFile(mid)
	appendCeremonyLogFile(bot)
}

// farewellQuote is one entry of the farewell pool.
type farewellQuote struct {
	author string
	text   string
}

// farewellQuotes is the built-in pool the closing quote is drawn from at random.
var farewellQuotes = []farewellQuote{
	{"Heraclitus", "The only constant in life is change."},
	{"Marcus Aurelius", "Everything that happens is either endurable or not. If it is endurable, endure it."},
	{"Lao Tzu", "When you realize nothing is lacking, the whole world belongs to you."},
	{"Nietzsche", "One must still have chaos in oneself to be able to give birth to a dancing star."},
	{"Epictetus", "It's not what happens to you, but how you react to it that matters."},
	{"Socrates (via Plato)", "The unexamined life is not worth living."},
	{"Seneca", "Every new beginning comes from some other beginning's end."},
}

// farewellQuote prints one random quote wrapped to the ceremony line width, with
// the attribution right-aligned to that same edge (flush with [✓] / 100%).
// Terminal and plain runs alike (--json never reaches it).
func (a *app) farewellQuote(p painter) {
	a.writeFarewellQuote(p, nil)
}

// writeFarewellQuote is farewellQuote with an optional ceremonyLog for scrollback.
func (a *app) writeFarewellQuote(p painter, log *ceremonyLog) {
	q := farewellQuotes[rand.IntN(len(farewellQuotes))] //nolint:gosec // G404: cosmetic quote pick, not security-sensitive.
	lines := formatFarewell(q)
	emit := func(s string) {
		if log != nil {
			log.line(s)
			return
		}
		fmt.Fprintln(a.out, s)
		appendCeremonyLogFile(s)
	}
	for i, line := range lines {
		if i == len(lines)-1 {
			// Author line: plain leading pad, dimmed attribution.
			trim := strings.TrimLeft(line, " ")
			pad := lipgloss.Width(line) - lipgloss.Width(trim)
			if pad < 0 {
				pad = 0
			}
			emit(strings.Repeat(" ", pad) + p.dim(trim))
			// Blank row after the attribution so the next prompt has breathing room.
			emit("")
			continue
		}
		emit(line)
	}
}

// formatFarewell wraps the quote to the ceremony grid and right-aligns the
// author on the final line so it shares the mark column's right edge.
func formatFarewell(q farewellQuote) []string {
	quoted := fmt.Sprintf("%q", q.text)
	// ASCII hyphen-minus avoids ambiguous em-dash display width across terminals.
	attr := "- " + q.author
	indentW := lipgloss.Width(ceremonyIndent)
	edge := ceremonyLineWidth()
	inner := edge - indentW
	if inner < 8 {
		inner = 8
	}
	var lines []string
	for _, part := range wrapDisplay(quoted, inner) {
		lines = append(lines, ceremonyIndent+part)
	}
	attrW := lipgloss.Width(attr)
	pad := edge - attrW
	if pad < indentW {
		pad = indentW
	}
	lines = append(lines, strings.Repeat(" ", pad)+attr)
	return lines
}

// wrapDisplay hard-wraps s to at most width display cells on word boundaries
// (falls back to character cuts when a single word is wider than width).
func wrapDisplay(s string, width int) []string {
	if width < 1 {
		return []string{s}
	}
	if lipgloss.Width(s) <= width {
		return []string{s}
	}
	words := strings.Fields(s)
	if len(words) == 0 {
		return nil
	}
	var lines []string
	var cur string
	for _, w := range words {
		if cur == "" {
			if lipgloss.Width(w) <= width {
				cur = w
				continue
			}
			// Hard-split an overlong token.
			for lipgloss.Width(w) > width {
				cut := cutPrefix(w, width)
				lines = append(lines, cut)
				w = strings.TrimPrefix(w, cut)
			}
			cur = w
			continue
		}
		trial := cur + " " + w
		if lipgloss.Width(trial) <= width {
			cur = trial
			continue
		}
		lines = append(lines, cur)
		cur = w
	}
	if cur != "" {
		lines = append(lines, cur)
	}
	return lines
}

// cutPrefix returns the longest prefix of s whose display width is <= width.
func cutPrefix(s string, width int) string {
	if width < 1 {
		return ""
	}
	var b strings.Builder
	for _, r := range s {
		next := b.String() + string(r)
		if lipgloss.Width(next) > width {
			break
		}
		b.WriteRune(r)
	}
	if b.Len() == 0 {
		// Always consume at least one rune so we make progress.
		for _, r := range s {
			return string(r)
		}
	}
	return b.String()
}

// terminalConfirm prompts via huh when a TTY is available; no terminal returns
// errNotATerminal so the caller refuses instead of blocking. Tests inject
// a.confirm to script answers without a real terminal.
func (a *app) terminalConfirm(question string, _ bool) (bool, error) {
	out := a.errOut
	if out == nil {
		out = os.Stderr
	}
	return confirmWithHuh(question, out)
}

// readSingleKey reads one raw keypress from stdin via stty; ok=false means raw mode could not be entered.
func readSingleKey() (byte, bool) {
	saved, err := sttyOutput("-g")
	if err != nil {
		return 0, false
	}
	if _, err := sttyOutput("-icanon", "-echo", "min", "1", "time", "0"); err != nil {
		return 0, false
	}
	defer func() { _, _ = sttyOutput(saved) }()
	buf := make([]byte, 1)
	if n, err := os.Stdin.Read(buf); err != nil || n == 0 {
		return 0, true // EOF counts as answered: decline
	}
	return buf[0], true
}

// sttyOutput runs stty against the process stdin and returns its trimmed stdout.
func sttyOutput(args ...string) (string, error) {
	cmd := exec.Command("stty", args...)
	cmd.Stdin = os.Stdin
	out, err := cmd.Output()
	return strings.TrimSpace(string(out)), err
}

// installerPATHLinks are the PATH shims install.sh may create. Prefer
// ~/.local/bin (user-writable); /usr/local/bin is only used when that dir is
// already writable without root. Legacy sudo-created /usr/local/bin links may
// remain if this process cannot unlink them.
func installerPATHLinks() []string {
	var links []string
	if home, err := os.UserHomeDir(); err == nil && home != "" {
		links = append(links, filepath.Join(home, ".local", "bin", "iris"))
	}
	links = append(links, "/usr/local/bin/iris")
	return links
}

// removeInstallerSymlink drops PATH shims that point at the removed binary.
// Tries a plain remove first, then passwordless `sudo -n rm` for a root-owned
// /usr/local/bin shim (same-shell hash compatibility from install.sh). Returns a
// warning only when both fail.
func removeInstallerSymlink(target string) error {
	var left []string
	for _, link := range installerPATHLinks() {
		if _, err := os.Lstat(link); err != nil {
			continue
		}
		if !shouldRemoveInstallerShim(link, target) {
			continue
		}
		if err := os.Remove(link); err == nil {
			continue
		}
		if err := removePathSudoN(link); err == nil {
			continue
		}
		if _, err := os.Lstat(link); err == nil {
			left = append(left, link)
		}
	}
	if len(left) == 0 {
		return nil
	}
	return fmt.Errorf("left PATH link(s) in place: %s", strings.Join(left, "; "))
}

// shouldRemoveInstallerShim reports whether a well-known PATH entry is ours.
// Handles symlinks, hard links, and residual hard links after the primary binary
// path was already unlinked.
func shouldRemoveInstallerShim(link, target string) bool {
	if dest, err := os.Readlink(link); err == nil {
		return symlinkPointsTo(dest, target, filepath.Dir(link))
	}
	fi, err := os.Lstat(link)
	if err != nil {
		return false
	}
	if ti, err := os.Lstat(target); err == nil {
		return os.SameFile(fi, ti)
	}
	// Target already removed: residual hardlink at a well-known installer path.
	return true
}

// removePathSudoN runs `sudo -n rm -f path` (no password prompt). Fails fast when
// sudo is absent or requires a password — callers then surface a dim warning.
func removePathSudoN(path string) error {
	if _, err := exec.LookPath("sudo"); err != nil {
		return err
	}
	cmd := exec.Command("sudo", "-n", "rm", "-f", path)
	return cmd.Run()
}

// symlinkPointsTo reports whether linkDest (the symlink's raw target) refers to
// the same path as want. linkDir is the directory containing the symlink, used
// to resolve relative destinations.
func symlinkPointsTo(linkDest, want, linkDir string) bool {
	if linkDest == want {
		return true
	}
	cleanWant := filepath.Clean(want)
	if filepath.Clean(linkDest) == cleanWant {
		return true
	}
	if !filepath.IsAbs(linkDest) {
		if linkDir == "" {
			linkDir = "/usr/local/bin"
		}
		linkDest = filepath.Join(linkDir, linkDest)
	}
	if filepath.Clean(linkDest) == cleanWant {
		return true
	}
	// EvalSymlinks on want may have resolved through other links already.
	if resolved, err := filepath.EvalSymlinks(linkDest); err == nil && filepath.Clean(resolved) == cleanWant {
		return true
	}
	return false
}

// engineHomeDir is the engine home directory for settings (parent of the control
// socket; ~/.iris by default). Empty when no socket is configured.
func engineHomeDir(s config.Settings) string {
	if s.Socket == "" {
		return ""
	}
	return filepath.Dir(s.Socket)
}

// removeEngineHome deletes the engine home tree (workspace, bin/, leftover
// config). Refuses obviously unsafe paths (empty, root, the user's home).
func removeEngineHome(home string) error {
	home = filepath.Clean(home)
	if home == "" || home == "." || home == "/" {
		return nil
	}
	if userHome, err := os.UserHomeDir(); err == nil {
		if home == filepath.Clean(userHome) {
			return nil
		}
	}
	return os.RemoveAll(home)
}

// removeShellPathEntries strips the installer's "# iris" PATH block from shell rc files, best-effort.
func removeShellPathEntries() {
	home, err := os.UserHomeDir()
	if err != nil {
		return
	}
	for _, rc := range []string{".zshrc", ".bashrc"} {
		p := filepath.Join(home, rc)
		info, err := os.Stat(p)
		if err != nil {
			continue
		}
		b, err := os.ReadFile(p) //nolint:gosec // G304: fixed rc names under the user's own home.
		if err != nil {
			continue
		}
		var kept []string
		changed := false
		for _, line := range strings.Split(string(b), "\n") {
			t := strings.TrimSpace(line)
			if t == "# iris" || (strings.Contains(t, ".iris/bin") && strings.Contains(t, "PATH")) {
				changed = true
				continue
			}
			kept = append(kept, line)
		}
		if changed {
			_ = os.WriteFile(p, []byte(strings.Join(kept, "\n")), info.Mode().Perm()) //nolint:gosec // G703: fixed rc names under the user's own home.
		}
	}
}

// resolveSelfPath resolves the running binary's real path through its symlinks so removal hits the actual file.
func resolveSelfPath() (string, error) {
	exe, err := os.Executable()
	if err != nil {
		return "", fmt.Errorf("locate the running iris binary: %w", err)
	}
	resolved, err := filepath.EvalSymlinks(exe)
	if err != nil {
		return "", fmt.Errorf("resolve the iris binary path %s: %w", exe, err)
	}
	return resolved, nil
}
