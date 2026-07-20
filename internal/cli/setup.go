package cli

import (
	"bytes"
	"context"
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

		p := a.newPainter(false)
		log := newCeremonyLog(a.out)
		done := func(label string) {
			mark := ceremonyCheckMark(p.green("✓"))
			log.line(formatCeremonyLine(label, mark))
		}

		switch choice {
		case setupLocal:
			log.line("  • Selected: Local mode")
			// Real work under live bars: install can take a while (Postgres
			// materialize / adopt). A cosmetic fill-to-100% before the work
			// made the terminal look frozen; these bars track the jobs.
			if err := runProgressWhile(a.out, "• Installing engine", func() error {
				return a.runSelfQuiet(cmd, "engine", "install")
			}); err != nil {
				return err
			}
			// Reinstall / repeated setup: a reachable daemon already owns the
			// engine — skip start -d so we do not spawn a second candidate.
			settings := a.resolveTarget(cmd)
			ctx := cmd.Context()
			if ctx == nil {
				ctx = context.Background()
			}
			if a.probeDaemon(ctx, settings) == nil {
				done("Engine already running")
			} else if err := runProgressWhile(a.out, "• Starting engine", func() error {
				return a.runSelfQuiet(cmd, "engine", "start", "-d")
			}); err != nil {
				return err
			} else {
				done("Engine started")
			}
			maybeReviewCeremony(a.out, log.content())
			return nil
		case setupRemote:
			log.line("  • Selected: Remote mode")
			host, token, perr := promptRemoteEndpoint(a.out)
			if perr != nil {
				return &fault{code: exitOpFailed, codeStr: "setup_failed", message: fmt.Sprintf("iris setup: %v", perr)}
			}
			host = strings.TrimSpace(host)
			if host == "" {
				log.line("  • No endpoint given. Remote mode: 'iris engine connect <host>'.")
				maybeReviewCeremony(a.out, log.content())
				return nil
			}
			args := []string{"engine", "connect", host}
			if strings.TrimSpace(token) != "" {
				args = append(args, "--token", token)
			}
			return a.runSelf(cmd, args...)
		default:
			log.line("  • Selected: Skip for now")
			log.line("  • No engine configured. Local mode: 'iris engine install && iris engine start -d';")
			log.line("    remote mode: 'iris engine connect <host>'.")
			maybeReviewCeremony(a.out, log.content())
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

// runSelfQuiet is runSelf with stdout/stderr captured so nested lifecycle
// commands don't break the install ceremony grid. On failure the captured
// streams are replayed, then the error is returned.
func (a *app) runSelfQuiet(_ *cobra.Command, args ...string) error {
	exe, err := os.Executable()
	if err != nil {
		return &fault{code: exitOpFailed, codeStr: "setup_failed", message: fmt.Sprintf("iris setup: resolve self: %v", err)}
	}
	var out, errb bytes.Buffer
	c := exec.Command(exe, args...)
	c.Stdout = &out
	c.Stderr = &errb
	c.Stdin = os.Stdin
	if err := c.Run(); err != nil {
		if a.out != nil {
			fmt.Fprint(a.out, out.String())
		}
		if a.errOut != nil {
			fmt.Fprint(a.errOut, errb.String())
		}
		return &fault{code: exitOpFailed, codeStr: "setup_failed", message: fmt.Sprintf("iris %s: %v", strings.Join(args, " "), err)}
	}
	return nil
}
