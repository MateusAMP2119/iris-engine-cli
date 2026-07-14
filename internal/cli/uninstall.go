package cli

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"unicode/utf8"

	"github.com/spf13/cobra"

	"github.com/MateusAMP2119/iris-lakehouse/internal/buildinfo"
)

// errNotATerminal reports that the interactive confirmation cannot run because
// stdin is not a terminal. The handler maps it to the standard consent-required
// refusal rather than blocking on a pipe.
var errNotATerminal = errors.New("stdin is not a terminal")

// uninstallCmdResult is the machine-readable payload of `iris uninstall`, the
// --json data envelope: the outcome (uninstalled or aborted), the running
// version, and the executable path it acted on. It carries no secret.
type uninstallCmdResult struct {
	Status  string `json:"status"`
	Version string `json:"version,omitempty"`
	Path    string `json:"path,omitempty"`
}

// uninstallCmd builds `iris uninstall`: one of the two root lifecycle verbs
// (beside `iris update`), the self-removal of the installed iris binary. It is
// daemonless and carries the destructive --yes/--force gate; it is distinct
// from `iris engine uninstall`, which tears down engine state.
func (a *app) uninstallCmd() *cobra.Command {
	c := &cobra.Command{
		Use:   "uninstall",
		Short: "Remove the installed iris binary itself (daemonless self-removal)",
		Args:  cobra.NoArgs,
		RunE:  a.uninstallSelf(),
	}
	addConfirmFlags(c)
	return daemonless(c)
}

// uninstallSelf is the handler for `iris uninstall`: the daemonless
// self-removal of the running iris executable. It refuses while a daemon is
// reachable (guiding to stop and tear down the engine first) unless --force
// overrides the probe; it then enforces the dev-loop consent gate
// (--yes/--force, or an interactive y/N prompt showing the version and path,
// aborting cleanly on decline and refusing with the standard consent-required
// error when no terminal is present); and finally resolves the running
// executable through its symlinks and removes it. A permission failure carries
// sudo / curl-uninstaller guidance. Every failure is operation-failed (exit 4);
// a clean abort exits 0 without touching a file; success prints the goodbye
// lines (or the one --json data envelope) and exits 0.
func (a *app) uninstallSelf() runE {
	return func(cmd *cobra.Command, _ []string) error {
		yes, _ := cmd.Flags().GetBool("yes")
		force, _ := cmd.Flags().GetBool("force")
		jsonMode, _ := cmd.Flags().GetBool("json")
		p := a.newPainter(jsonMode)

		// 1. Refuse while a daemon is reachable (same target resolution and probe
		// every command uses), unless --force overrides the probe.
		if !force {
			settings := a.resolveTarget(cmd)
			if err := a.probeDaemon(cmd.Context(), settings); err == nil {
				return &fault{
					code:    exitOpFailed,
					codeStr: "daemon_reachable",
					message: `a running iris daemon is reachable; stop and tear down the engine first with "iris engine stop && iris engine uninstall", or re-run with --force`,
				}
			}
		}

		// 2. Resolve the running executable's real path (needed for the prompt and
		// the removal).
		resolve := a.executablePath
		if resolve == nil {
			resolve = resolveSelfPath
		}
		path, err := resolve()
		if err != nil {
			return &fault{code: exitOpFailed, codeStr: "uninstall_failed", message: fmt.Sprintf("iris uninstall: %v", err)}
		}

		// 3. Consent gate (dev-loop y/N flavor).
		if !yes && !force {
			// tty ceremony: a cyan confirmation box before the y/N prompt, mirroring
			// the curl uninstaller. Plain (piped) runs draw nothing.
			if p.enabled {
				a.uninstallBox(p, buildinfo.Version, path)
			}
			confirmFn := a.confirm
			if confirmFn == nil {
				confirmFn = a.terminalConfirm
			}
			ok, cerr := confirmFn(fmt.Sprintf("%s from %s", buildinfo.Version, path), false)
			if cerr != nil {
				return &fault{
					code:    exitOpFailed,
					codeStr: "confirmation_required",
					message: `iris uninstall is destructive; re-run with --yes to confirm, or run it in a terminal to confirm interactively`,
				}
			}
			if !ok {
				if jsonMode {
					return json.NewEncoder(a.out).Encode(dataEnvelope{Data: uninstallCmdResult{
						Status: "aborted", Version: buildinfo.Version, Path: path,
					}})
				}
				if p.enabled {
					fmt.Fprintln(a.out, p.green("  Aborted. Nothing removed."))
				} else {
					fmt.Fprintln(a.out, "Aborted. Nothing removed.")
				}
				return nil
			}
		}

		// 4. Remove the binary.
		if err := os.Remove(path); err != nil {
			if errors.Is(err, fs.ErrPermission) {
				return &fault{
					code:    exitOpFailed,
					codeStr: "permission_denied",
					message: fmt.Sprintf("iris uninstall: cannot remove %s: permission denied; re-run with sudo, or use the curl uninstaller", path),
				}
			}
			return &fault{code: exitOpFailed, codeStr: "uninstall_failed", message: fmt.Sprintf("iris uninstall: remove %s: %v", path, err)}
		}

		// 5. Success. The removal is reported by the human/JSON output below; no log
		// line is emitted, so the console is not double-reported.
		if jsonMode {
			return json.NewEncoder(a.out).Encode(dataEnvelope{Data: uninstallCmdResult{
				Status: "uninstalled", Version: buildinfo.Version, Path: path,
			}})
		}
		if p.enabled {
			// tty ceremony: the removal line, then a rainbow farewell mirroring the
			// curl uninstaller's "Goodbye" gradient.
			fmt.Fprintf(a.out, "  Uninstalled %s.\n", path)
			fmt.Fprintf(a.out, "  %s from iris.\n", p.rainbow("Goodbye"))
			return nil
		}
		fmt.Fprintf(a.out, "Uninstalled %s.\n", path)
		fmt.Fprintln(a.out, "Goodbye from iris.")
		return nil
	}
}

// uninstallBox draws the cyan confirmation box shown before the y/N prompt on a
// terminal, mirroring uninstall.sh: the version in magenta, the removal path
// plain. The rules and the right border are sized to the content line's display
// width -- measured on the UNSTYLED interior so ANSI escapes never count -- so the
// box stays aligned for any path length. It writes to stdout; the prompt itself
// follows on stderr.
func (a *app) uninstallBox(p painter, version, path string) {
	const leftPad, rightPad = "   ", "  "
	// Build the plain interior first and measure it; the styled interior below
	// renders the same visible glyphs (magenta wraps version without widening it),
	// so both share this width and the right border aligns.
	plainInner := leftPad + "Uninstall " + version + " from " + path + "?" + rightPad
	rule := strings.Repeat("─", utf8.RuneCountInString(plainInner))
	styledInner := leftPad + "Uninstall " + p.magenta(version) + " from " + path + "?" + rightPad
	bar := p.cyan("│")

	fmt.Fprintln(a.out, p.cyan("  ┌"+rule+"┐"))
	fmt.Fprintf(a.out, "  %s%s%s\n", bar, styledInner, bar)
	fmt.Fprintln(a.out, p.cyan("  └"+rule+"┘"))
}

// terminalConfirm is the production interactive confirmation for `iris uninstall`:
// it prompts on stderr and reads a y/N answer from stdin, returning true only for
// y/yes. When stdin is not a terminal it returns errNotATerminal so the caller
// raises the standard consent-required refusal instead of blocking on a pipe. The
// isTeardown flavor is unused: uninstall takes the dev-loop y/N form.
func (a *app) terminalConfirm(subject string, _ bool) (bool, error) {
	stat, err := os.Stdin.Stat()
	if err != nil || stat.Mode()&os.ModeCharDevice == 0 {
		return false, errNotATerminal
	}
	fmt.Fprintf(a.errOut, "Uninstall %s? (y/N): ", subject)
	line, rerr := bufio.NewReader(os.Stdin).ReadString('\n')
	if rerr != nil && line == "" {
		return false, nil // EOF with no answer is a decline, not an error.
	}
	ans := strings.ToLower(strings.TrimSpace(line))
	return ans == "y" || ans == "yes", nil
}

// resolveSelfPath resolves the running iris binary's real on-disk path, following
// the executable through its symlinks (os.Executable then filepath.EvalSymlinks)
// so `iris uninstall` removes the actual file rather than a symlink into it.
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
