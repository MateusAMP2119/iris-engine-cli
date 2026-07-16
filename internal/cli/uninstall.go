package cli

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"math/rand/v2"
	"os"
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

// uninstallSelf runs the staged sequence: a decline aborts the rest clean (exit 0), a failure exits 4 naming the step, success closes with the FAREWELL! banner and a random quote.
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
		say("  %s", p.cyan(fmt.Sprintf("[IRIS UNINSTALL v%s]", buildinfo.Version)))
		say("")
		say("  Starting complete uninstallation sequence...")
		say("")

		// Step 1/3: stop a recorded detached daemon; nothing recorded and nothing reachable passes clean.
		say("  Step 1/3 — %s", p.cyan("Stopping Iris Engine"))
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
		say("  Step 2/3 — %s", p.cyan("Removing Engine State"))
		if !daemon.EngineArtifactsPresent(settings) {
			steps = append(steps, uninstallStep{Step: 2, Name: stepEngineState, Status: "nothing_to_remove"})
			done("No engine state on disk; nothing to remove.")
		} else {
			where := ""
			if settings.Socket != "" {
				where = " under " + filepath.Dir(settings.Socket)
			}
			ok, cerr := a.uninstallConsent(fmt.Sprintf("Remove engine state%s (managed Postgres tree, object store, logs, socket, service unit)?", where), yes, force)
			if cerr != nil {
				return cerr
			}
			if !ok {
				steps = append(steps,
					uninstallStep{Step: 2, Name: stepEngineState, Status: "declined"},
					uninstallStep{Step: 3, Name: stepBinary, Status: "skipped"})
				return a.uninstallAborted(p, jsonMode, path, steps)
			}
			say("  • Removing engine state...")
			removed, rerr := daemon.RemoveEngineArtifacts(settings)
			if rerr != nil {
				a.logger.Error("uninstall: engine state removal failed", "err", rerr)
				return &fault{
					code:    exitOpFailed,
					codeStr: "uninstall_failed",
					message: fmt.Sprintf("iris uninstall: step 2/%d (remove engine state) failed: %v", uninstallSteps, rerr),
				}
			}
			steps = append(steps, uninstallStep{Step: 2, Name: stepEngineState, Status: "removed", Removed: removed})
			done("Engine state removed.")
		}
		say("")

		// Step 3/3: remove the running binary itself.
		say("  Step 3/3 — %s", p.cyan("Uninstalling Iris CLI"))
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
					message: fmt.Sprintf("iris uninstall: step 3/%d (remove binary) failed: cannot remove %s: permission denied; re-run with sudo, or use the curl uninstaller", uninstallSteps, path),
				}
			}
			return &fault{
				code:    exitOpFailed,
				codeStr: "uninstall_failed",
				message: fmt.Sprintf("iris uninstall: step 3/%d (remove binary) failed: remove %s: %v", uninstallSteps, path, err),
			}
		}
		steps = append(steps, uninstallStep{Step: 3, Name: stepBinary, Status: "removed", Removed: []string{path}})
		if !jsonMode {
			a.uninstallProgressBar(p)
		}
		done("Binary removed")
		done("Traces erased")

		if jsonMode {
			return json.NewEncoder(a.out).Encode(dataEnvelope{Data: uninstallCmdResult{
				Status: "uninstalled", Version: buildinfo.Version, Path: path, Steps: steps,
			}})
		}
		say("")
		if p.enabled {
			farewellBanner(a.out, p)
			say("")
		}
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

// uninstallProgressBar animates step 3's bar in place on a terminal; piped runs draw nothing.
func (a *app) uninstallProgressBar(p painter) {
	if !p.enabled {
		return
	}
	const cells = 10
	for i := 0; i <= cells; i++ {
		bar := strings.Repeat("█", i) + strings.Repeat("░", cells-i)
		fmt.Fprintf(a.out, "\r\033[2K  Uninstalling... [%s] %d%%", p.cyan(bar), i*100/cells)
		time.Sleep(25 * time.Millisecond)
	}
	fmt.Fprintln(a.out)
}

// farewellBannerRows is the block-art FAREWELL! mark, the installer banner's letterform family.
var farewellBannerRows = []string{
	"   ███████╗ █████╗ ██████╗ ███████╗██╗    ██╗███████╗██╗     ██╗     ██╗",
	"   ██╔════╝██╔══██╗██╔══██╗██╔════╝██║    ██║██╔════╝██║     ██║     ██║",
	"   █████╗  ███████║██████╔╝█████╗  ██║ █╗ ██║█████╗  ██║     ██║     ██║",
	"   ██╔══╝  ██╔══██║██╔══██╗██╔══╝  ██║███╗██║██╔══╝  ██║     ██║     ╚═╝",
	"   ██║     ██║  ██║██║  ██║███████╗╚███╔███╔╝███████╗███████╗███████╗██╗",
	"   ╚═╝     ╚═╝  ╚═╝╚═╝  ╚═╝╚══════╝ ╚══╝╚══╝ ╚══════╝╚══════╝╚══════╝╚═╝",
}

// farewellBanner paints the FAREWELL! mark one rainbow color per row; callers gate it on an enabled painter.
func farewellBanner(w io.Writer, _ painter) {
	for i, row := range farewellBannerRows {
		fmt.Fprintf(w, "%s%s%s\n", rainbowPalette[i%len(rainbowPalette)], row, ansiReset)
	}
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

// farewellQuote prints one random quote with attribution; terminal and plain runs alike (--json never reaches it).
func (a *app) farewellQuote(p painter) {
	q := farewellQuotes[rand.IntN(len(farewellQuotes))] //nolint:gosec // G404: cosmetic quote pick, not security-sensitive.
	fmt.Fprintf(a.out, "   %q\n", q.text)
	fmt.Fprintf(a.out, "                  %s\n", p.dim("— "+q.author))
}

// terminalConfirm prompts the step's question on stderr and reads y/N from stdin; no terminal returns errNotATerminal so the caller refuses instead of blocking.
func (a *app) terminalConfirm(question string, _ bool) (bool, error) {
	stat, err := os.Stdin.Stat()
	if err != nil || stat.Mode()&os.ModeCharDevice == 0 {
		return false, errNotATerminal
	}
	fmt.Fprintf(a.errOut, "%s (y/N): ", question)
	line, rerr := bufio.NewReader(os.Stdin).ReadString('\n')
	if rerr != nil && line == "" {
		return false, nil // EOF with no answer is a decline, not an error.
	}
	ans := strings.ToLower(strings.TrimSpace(line))
	return ans == "y" || ans == "yes", nil
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
