package cli

import (
	"fmt"
	"os"
	"os/exec"
	"strings"

	"github.com/spf13/cobra"
)

// setupCmd builds `iris setup`: the post-install engine configuration menu
// (local install+start, remote connect, or skip). install.sh invokes this so
// the interactive flow uses huh/viper-backed config instead of shell prompts.
func (a *app) setupCmd() *cobra.Command {
	c := &cobra.Command{
		Use:   "setup",
		Short: "Post-install engine setup (local, remote, or skip)",
		Args:  cobra.NoArgs,
		RunE:  a.setupEngine(),
	}
	c.Flags().String("mode", "", "local|remote|skip (non-interactive; also IRIS_ENGINE_SETUP)")
	return daemonless(c)
}

func (a *app) setupEngine() runE {
	return func(cmd *cobra.Command, _ []string) error {
		jsonMode, _ := cmd.Flags().GetBool("json")
		if jsonMode {
			return a.usage("iris setup is interactive; do not pass --json")
		}
		mode, _ := cmd.Flags().GetString("mode")
		if mode == "" {
			mode = os.Getenv("IRIS_ENGINE_SETUP")
		}
		mode = strings.ToLower(strings.TrimSpace(mode))

		choice, err := selectEngineSetup(mode, a.out)
		if err != nil {
			return &fault{code: exitOpFailed, codeStr: "setup_failed", message: fmt.Sprintf("iris setup: %v", err)}
		}

		switch choice {
		case setupLocal:
			fmt.Fprintln(a.out, "  • Selected: Local mode")
			fmt.Fprintln(a.out, "  🚀 Starting Iris Engine...")
			runProgressBar(a.out, "• Setting up engine")
			if err := a.runSelf(cmd, "engine", "install"); err != nil {
				return err
			}
			if err := a.runSelf(cmd, "engine", "start", "-d"); err != nil {
				return err
			}
			fmt.Fprintln(a.out, "  ✓ Engine started")
			return nil
		case setupRemote:
			fmt.Fprintln(a.out, "  • Selected: Remote mode")
			host, token, perr := promptRemoteEndpoint(a.out)
			if perr != nil {
				return &fault{code: exitOpFailed, codeStr: "setup_failed", message: fmt.Sprintf("iris setup: %v", perr)}
			}
			host = strings.TrimSpace(host)
			if host == "" {
				fmt.Fprintln(a.out, "  • No endpoint given. Run 'iris engine connect <host>' when ready.")
				return nil
			}
			args := []string{"engine", "connect", host}
			if strings.TrimSpace(token) != "" {
				args = append(args, "--token", token)
			}
			return a.runSelf(cmd, args...)
		default:
			fmt.Fprintln(a.out, "  • Selected: Skip for now")
			fmt.Fprintln(a.out, "  • Engine not configured. Run 'iris engine install && iris engine start -d' (local)")
			fmt.Fprintln(a.out, "    or 'iris engine connect <host>' (remote) when ready.")
			return nil
		}
	}
}

// runSelf re-invokes the current iris binary with args (same argv0), inheriting
// stdio so nested commands render normally.
func (a *app) runSelf(_ *cobra.Command, args ...string) error {
	exe, err := os.Executable()
	if err != nil {
		return &fault{code: exitOpFailed, codeStr: "setup_failed", message: fmt.Sprintf("iris setup: resolve self: %v", err)}
	}
	c := exec.Command(exe, args...)
	c.Stdout = a.out
	c.Stderr = a.errOut
	c.Stdin = os.Stdin
	if err := c.Run(); err != nil {
		return &fault{code: exitOpFailed, codeStr: "setup_failed", message: fmt.Sprintf("iris %s: %v", strings.Join(args, " "), err)}
	}
	return nil
}
