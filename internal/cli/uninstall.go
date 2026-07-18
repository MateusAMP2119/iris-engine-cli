package cli

import (
	"bufio"
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
	"time"
	"unicode/utf8"

	"github.com/spf13/cobra"

	"github.com/MateusAMP2119/iris-lakehouse/internal/buildinfo"
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

// uninstallStepColumn is the column the step lines' [✓] marks align at.
const uninstallStepColumn = 52

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
		say := func(format string, args ...any) {
			if !jsonMode {
				fmt.Fprintf(a.out, format+"\n", args...)
			}
		}
		done := func(text string) {
			if !jsonMode {
				a.uninstallStepDone(p, text)
			}
		}

		say("")
		if p.enabled {
			a.uninstallHeaderBox(p, buildinfo.Version)
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
				return a.uninstallAborted(p, jsonMode, path, steps)
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
			if !jsonMode {
				a.uninstallProgressBar(p, "• Removing engine state...")
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
			return a.uninstallAborted(p, jsonMode, path, steps)
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
		removeInstallerSymlink(path)
		removeShellPathEntries()
		if !jsonMode {
			a.uninstallProgressBar(p, "🧹 Removing binary and traces...")
		}
		done("Binary removed")
		done("Traces erased")

		if jsonMode {
			return json.NewEncoder(a.out).Encode(dataEnvelope{Data: uninstallCmdResult{
				Status: "uninstalled", Version: buildinfo.Version, Path: path, Steps: steps,
			}})
		}
		say("")
		a.farewellQuote(p)
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

// uninstallStepDone prints one completed-step line, the [✓] mark aligned at a fixed column (green on a terminal).
func (a *app) uninstallStepDone(p painter, text string) {
	pad := uninstallStepColumn - utf8.RuneCountInString(text)
	if pad < 1 {
		pad = 1
	}
	fmt.Fprintf(a.out, "  • %s%s[%s]\n", text, strings.Repeat(" ", pad), p.green("✓"))
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

// uninstallProgressBar animates a removal step's bar in place on a terminal, prefix leading the bar; plain glyphs match the installer's engine bar exactly. Piped runs draw nothing.
func (a *app) uninstallProgressBar(p painter, prefix string) {
	if !p.enabled {
		return
	}
	const cells = 10
	for i := 0; i <= cells; i++ {
		bar := strings.Repeat("█", i) + strings.Repeat("░", cells-i)
		fmt.Fprintf(a.out, "\r\033[2K  %s [%s] %d%%", prefix, bar, i*100/cells)
		time.Sleep(25 * time.Millisecond)
	}
	fmt.Fprintln(a.out)
}

// uninstallHeaderBox draws the cyan header box on a terminal, version in magenta; borders sized on the unstyled interior so escapes never skew alignment.
func (a *app) uninstallHeaderBox(p painter, version string) {
	const leftPad, rightPad = "   ", "  "
	plainInner := leftPad + "IRIS UNINSTALL " + version + rightPad
	rule := strings.Repeat("═", utf8.RuneCountInString(plainInner))
	styledInner := leftPad + "IRIS UNINSTALL " + p.magenta(version) + rightPad
	bar := p.cyan("║")

	fmt.Fprintln(a.out, p.cyan("  ╔"+rule+"╗"))
	fmt.Fprintf(a.out, "  %s%s%s\n", bar, styledInner, bar)
	fmt.Fprintln(a.out, p.cyan("  ╚"+rule+"╝"))
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

// farewellQuote prints one random quote, the attribution right-aligned under the quote's end; terminal and plain runs alike (--json never reaches it).
func (a *app) farewellQuote(p painter) {
	q := farewellQuotes[rand.IntN(len(farewellQuotes))] //nolint:gosec // G404: cosmetic quote pick, not security-sensitive.
	quoted := fmt.Sprintf("%q", q.text)
	attr := "— " + q.author
	pad := 3 + utf8.RuneCountInString(quoted) - utf8.RuneCountInString(attr)
	if pad < 3 {
		pad = 3
	}
	fmt.Fprintf(a.out, "   %s\n", quoted)
	fmt.Fprintf(a.out, "%s%s\n", strings.Repeat(" ", pad), p.dim(attr))
}

// terminalConfirm prompts the step's question on stderr and reads one y/N keypress from stdin (no Enter); no terminal returns errNotATerminal so the caller refuses instead of blocking.
func (a *app) terminalConfirm(question string, _ bool) (bool, error) {
	stat, err := os.Stdin.Stat()
	if err != nil || stat.Mode()&os.ModeCharDevice == 0 {
		return false, errNotATerminal
	}
	fmt.Fprintf(a.errOut, "  %s (y/N): ", question)
	if ans, ok := readSingleKey(); ok {
		yes := ans == 'y' || ans == 'Y'
		shown := "n"
		if yes {
			shown = "y"
		}
		fmt.Fprintf(a.errOut, "%s\n", shown)
		return yes, nil
	}
	// stty unavailable: fall back to a line read
	line, rerr := bufio.NewReader(os.Stdin).ReadString('\n')
	if rerr != nil && line == "" {
		return false, nil // EOF with no answer is a decline, not an error.
	}
	ans := strings.ToLower(strings.TrimSpace(line))
	return ans == "y" || ans == "yes", nil
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

// removeInstallerSymlink drops /usr/local/bin/iris when it links to the removed binary, best-effort.
func removeInstallerSymlink(target string) {
	const link = "/usr/local/bin/iris"
	if dest, err := os.Readlink(link); err == nil && dest == target {
		_ = os.Remove(link)
	}
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
