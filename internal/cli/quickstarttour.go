package cli

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"

	"github.com/spf13/cobra"

	"github.com/MateusAMP2119/iris-engine-cli/internal/config"
	"github.com/MateusAMP2119/iris-engine-cli/internal/daemon"
)

// promptKind names the flavor of a quickstart-tour question, so an injected
// tourPrompt (and the production terminal prompt) can tell a command
// confirmation from the workspace question.
type promptKind int

const (
	// promptCommand is the per-step `Run it? (Y/n)` confirmation preceding each
	// tour command; the empty answer defaults to proceed.
	promptCommand promptKind = iota
	// promptWorkspace is the tour's workspace question: use the current workspace,
	// or create ./iris-quickstart-demo and work there.
	promptWorkspace
)

// promptAnswer is the operator's answer to one tour question.
type promptAnswer int

const (
	// answerProceed runs the step (or accepts the workspace proposal).
	answerProceed promptAnswer = iota
	// answerSkip skips this one step without running it and continues the tour.
	// The workspace question treats it as a decline. The production terminal
	// prompt returns it for "s"/"skip"; it is mainly a seam answer for tests.
	answerSkip
	// answerQuit stops the tour: a clean abort, exit 0 with a resume hint.
	answerQuit
)

// tourPromptFunc is the signature of the tourPrompt seam.
type tourPromptFunc = func(question string, kind promptKind) (promptAnswer, error)

// quickstartDemoDir is the workspace directory the tour offers to create when
// the current directory is not a workspace (specification section 8).
const quickstartDemoDir = "iris-quickstart-demo"

// The tour's prompt questions. Command steps default to proceed on an empty
// answer (Y/n); anything unrecognized reads as quit, so a typo never runs a
// real command.
const (
	tourCommandQuestion   = "Run it? (Y/n)"
	tourWorkspaceQuestion = "Run the tour in this workspace? (Y/n)"
	tourCreateQuestion    = "Create ./" + quickstartDemoDir + " and work there? (Y/n)"
)

// runQuickstartTour is the guided tour of the first session (specification
// section 8): after the welcome it resolves the workspace, adaptively skips
// install/start when the workspace daemon already answers, and then walks the
// canonical steps -- explain, show the literal command, confirm, execute for
// real through the in-process runner. Declines, EOF, and interrupts abort clean
// (exit 0, resume hint); a failing step surfaces its own error and exit
// category; yes runs everything unattended.
func (a *app) runQuickstartTour(cmd *cobra.Command, yes bool) error {
	base := cmd.Context()
	if base == nil {
		base = context.Background()
	}
	// The Ctrl-C path: cancellation makes the open prompt (or the gap between
	// steps) read as a clean abort. A signal during a step also cancels that
	// step's in-process command through the shared context.
	ctx, stop := signal.NotifyContext(base, os.Interrupt, syscall.SIGTERM)
	defer stop()

	p := a.newPainter(false)
	prompt := a.tourPrompt
	if prompt == nil {
		prompt = newTerminalTourPrompt(a.errOut)
	}
	run := a.runStep
	if run == nil {
		run = a.runTourChild
	}

	a.quickstartWelcome(p)
	fmt.Fprintln(a.out)

	ok, err := a.tourWorkspace(ctx, prompt, yes)
	if err != nil {
		return err
	}
	if !ok {
		return a.tourAbort()
	}

	// Adaptive skip: every step is idempotent, so a daemon already answering on
	// the workspace socket means install and start are done -- announce and skip,
	// never prompt for them. The probe is local by construction (the tour refuses
	// --host and ignores any ambient host): the tour only ever adopts the local
	// workspace engine it would otherwise create. Resolved after the workspace
	// step so the socket default is the tour workspace's.
	steps := quickstartSteps()
	settings := a.resolveTarget(cmd)
	settings.Host = ""
	first := 0
	if a.probeDaemon(ctx, settings) == nil {
		fmt.Fprintf(a.out, "An engine is already running on this workspace's socket — steps 1 and 2 (%s; %s) are already done; skipping ahead.\n",
			strings.Join(steps[0].Argv, " "), strings.Join(steps[1].Argv, " "))
		first = 2
	} else if settings.Managed() && daemon.IsManagedInstalled(settings) {
		fmt.Fprintln(a.out, "The managed Postgres is already installed; step 1 only verifies it (every step is idempotent).")
	}

	for i := first; i < len(steps); i++ {
		step := steps[i]
		if ctx.Err() != nil {
			return a.tourAbort()
		}
		fmt.Fprintln(a.out)
		fmt.Fprintf(a.out, "Step %d/%d — %s\n", i+1, len(steps), step.Explanation)
		fmt.Fprintf(a.out, "  %s\n", p.green("$ "+strings.Join(step.Argv, " ")))
		if !yes {
			ans, perr := askTour(ctx, prompt, tourCommandQuestion, promptCommand)
			switch {
			case perr != nil || ans == answerQuit || ctx.Err() != nil:
				return a.tourAbort()
			case ans == answerSkip:
				fmt.Fprintln(a.out, "Skipped.")
				continue
			}
		}
		if step.ID == "apply" {
			if err := a.tourMaterializeSample(); err != nil {
				return err
			}
		}
		code := run(ctx, tourStepArgv(cmd, step))
		if ctx.Err() != nil {
			return a.tourAbort()
		}
		if code != exitOK {
			return a.tourStepFailed(step, i+1, len(steps), code)
		}
	}

	a.tourWrapUp(p)
	return nil
}

// tourWorkspace resolves the tour's workspace: a directory that already looks
// like a workspace (.iris/ or pipelines/ present) is used with the operator's
// consent; anywhere else the tour offers to create ./iris-quickstart-demo and
// work there (mkdir + chdir, so every subsequent step operates on cwd exactly
// like any command). It reports false for a clean abort (decline, EOF,
// interrupt) and an error only for a real filesystem fault.
func (a *app) tourWorkspace(ctx context.Context, prompt tourPromptFunc, yes bool) (bool, error) {
	wd, err := os.Getwd()
	if err != nil {
		return false, &fault{code: exitOpFailed, codeStr: "quickstart_workspace",
			message: fmt.Sprintf("quickstart: resolve the current directory: %v", err)}
	}
	if isWorkspaceDir(wd) {
		fmt.Fprintf(a.out, "This directory is already a workspace: %s\n", wd)
		if yes {
			return true, nil
		}
		ans, perr := askTour(ctx, prompt, tourWorkspaceQuestion, promptWorkspace)
		return perr == nil && ans == answerProceed, nil
	}

	fmt.Fprintln(a.out, "This directory is not an iris workspace yet.")
	if !yes {
		ans, perr := askTour(ctx, prompt, tourCreateQuestion, promptWorkspace)
		if perr != nil || ans != answerProceed {
			return false, nil
		}
	}
	// MkdirAll: re-running the tour from the same parent adopts the existing demo
	// directory rather than failing; 0755 because a workspace is traversable
	// project source, not a private artifact.
	if err := os.MkdirAll(quickstartDemoDir, 0o755); err != nil {
		return false, &fault{code: exitOpFailed, codeStr: "quickstart_workspace",
			message: fmt.Sprintf("quickstart: create ./%s: %v", quickstartDemoDir, err)}
	}
	if err := os.Chdir(quickstartDemoDir); err != nil {
		return false, &fault{code: exitOpFailed, codeStr: "quickstart_workspace",
			message: fmt.Sprintf("quickstart: enter ./%s: %v", quickstartDemoDir, err)}
	}
	fmt.Fprintf(a.out, "Working in ./%s.\n", quickstartDemoDir)
	return true, nil
}

// isWorkspaceDir reports whether dir already looks like an iris workspace: a
// .iris/ engine directory or a pipelines/ source tree.
func isWorkspaceDir(dir string) bool {
	for _, marker := range []string{config.DirName, "pipelines"} {
		if st, err := os.Stat(filepath.Join(dir, marker)); err == nil && st.IsDir() {
			return true
		}
	}
	return false
}

// askTour asks one tour question through prompt while honoring ctx: a
// cancellation (Ctrl-C) wins over a pending read and reads as quit. The prompt
// runs in a goroutine because the production prompt blocks on the process
// stdin, which has no cancellable read; after a cancellation the abandoned
// read never outlives the tour by more than the process itself.
func askTour(ctx context.Context, prompt tourPromptFunc, question string, kind promptKind) (promptAnswer, error) {
	type outcome struct {
		ans promptAnswer
		err error
	}
	ch := make(chan outcome, 1)
	go func() {
		ans, err := prompt(question, kind)
		ch <- outcome{ans: ans, err: err}
	}()
	select {
	case <-ctx.Done():
		return answerQuit, nil
	case o := <-ch:
		return o.ans, o.err
	}
}

// newTerminalTourPrompt builds the production tour prompt: the question goes to
// errOut (a prompt is dialogue, never command output) and one line is read from
// the process stdin. A single reader serves the whole tour, so a line buffered
// ahead is never dropped between prompts. EOF -- a closed stdin -- answers
// quit, the clean-abort path; only a real read fault surfaces as an error.
func newTerminalTourPrompt(errOut io.Writer) tourPromptFunc {
	reader := bufio.NewReader(os.Stdin)
	return func(question string, _ promptKind) (promptAnswer, error) {
		fmt.Fprintf(errOut, "%s ", question)
		line, err := reader.ReadString('\n')
		if err != nil && !errors.Is(err, io.EOF) {
			return answerQuit, fmt.Errorf("quickstart: read prompt answer: %w", err)
		}
		if line == "" && err != nil {
			return answerQuit, nil
		}
		switch strings.ToLower(strings.TrimSpace(line)) {
		case "", "y", "yes":
			return answerProceed, nil
		case "s", "skip":
			return answerSkip, nil
		default:
			// n, no, q, or anything unrecognized: never run a real command on a typo.
			return answerQuit, nil
		}
	}
}

// tourStepArgv is the argv a step hands the runner: the canonical table row
// with the literal "iris" argv[0] stripped -- the runner IS iris, in process --
// plus the tour's own explicit --socket, so every step targets the same local
// engine the tour is touring.
func tourStepArgv(cmd *cobra.Command, step quickstartStep) []string {
	argv := append([]string(nil), step.Argv[1:]...)
	if v, ok := changedString(cmd, "socket"); ok {
		argv = append(argv, "--socket="+v)
	}
	return argv
}

// runTourChild is the production runStep: a fresh in-process child app over the
// tour's own streams runs the real command implementation -- same code path,
// same exit categories, never a PATH lookup -- and renders its own error, so
// the tour receives only the categorical exit code. Every injectable seam is
// carried across, so a harnessed parent stays harnessed through its steps.
func (a *app) runTourChild(ctx context.Context, args []string) int {
	child := newAppWithLogger(a.out, a.errOut, a.logger)
	child.newKeyReader = a.newKeyReader
	child.daemonTLSConfig = a.daemonTLSConfig
	child.applyWarnings = a.applyWarnings
	child.runUpdate = a.runUpdate
	child.confirm = a.confirm
	child.executablePath = a.executablePath
	child.isTTY = a.isTTY
	child.stdinIsTTY = a.stdinIsTTY
	return child.runContext(ctx, args)
}

// tourMaterializeSample writes the embedded hello_iris sample into the tour
// workspace right before the apply step, announcing what it wrote; present
// files are kept (a differing one with the materializer's warning on stderr),
// never clobbered. A filesystem fault is a real failure: the sample is what the
// next step applies.
func (a *app) tourMaterializeSample() error {
	written, err := materializeQuickstartSample(".", a.errOut)
	if err != nil {
		return &fault{code: exitOpFailed, codeStr: "quickstart_sample",
			message: fmt.Sprintf("quickstart: materialize the hello_iris sample: %v", err)}
	}
	if len(written) == 0 {
		fmt.Fprintln(a.out, "The hello_iris sample is already in the workspace; keeping it.")
		return nil
	}
	fmt.Fprintln(a.out, "Materialized the embedded hello_iris sample:")
	for _, rel := range written {
		fmt.Fprintf(a.out, "  wrote %s\n", rel)
	}
	return nil
}

// tourAbort ends the tour cleanly -- a decline, EOF, or interrupt is a choice,
// never a failure: exit 0, nothing half-broken (every completed step is a real,
// idempotent command), and a resume hint.
func (a *app) tourAbort() error {
	fmt.Fprintln(a.out)
	fmt.Fprintln(a.out, "Tour stopped — nothing to undo; every completed step is a real, idempotent command.")
	fmt.Fprintln(a.out, "Resume any time: iris quickstart")
	return nil
}

// tourStepFailed surfaces a failing step: its own error is already rendered by
// the step itself, so the tour adds only the resume hint -- and, for a
// dead-lettered run (exit 5), the dead-letter lesson -- and exits with the
// step's own category.
func (a *app) tourStepFailed(step quickstartStep, k, total, code int) error {
	if code == exitDeadLettered {
		fmt.Fprintln(a.errOut, "The run dead-lettered — the failure worklist in person: iris deadletter show hello_iris explains it, and iris deadletter replay hello_iris re-runs it once fixed.")
	}
	return &fault{
		code:    code,
		codeStr: "quickstart_step_failed",
		message: fmt.Sprintf("quickstart stopped at step %d/%d (%s); fix the issue above and resume any time: iris quickstart",
			k, total, strings.Join(step.Argv, " ")),
	}
}

// tourWrapUp closes a completed tour: the engine-left-running note, the
// cheat-sheet of what the session used, the cleanup block, a PATH note when the
// binary's directory is not exported, and the rainbow sign-off (plain under
// NO_COLOR or a pipe, like every ceremony).
func (a *app) tourWrapUp(p painter) {
	fmt.Fprintln(a.out)
	fmt.Fprintln(a.out, "That's the tour — the engine is still running and stays up after this terminal closes.")
	fmt.Fprintln(a.out)
	fmt.Fprintln(a.out, "What you used (the cheat-sheet):")
	fmt.Fprintln(a.out, "  iris engine install | start -d | info | stop     the engine lifecycle")
	fmt.Fprintln(a.out, "  iris declare apply <path>                        register a declaration")
	fmt.Fprintln(a.out, "  iris pipeline run <name>                         trigger a manual run")
	fmt.Fprintln(a.out, "  iris data provenance <schema.table> <pk>         ask a row who wrote it")
	fmt.Fprintln(a.out, "  iris run list                                    run history (--graph for rails)")
	fmt.Fprintln(a.out, "  iris deadletter list                             the failure worklist")
	fmt.Fprintln(a.out)
	fmt.Fprintln(a.out, "Clean up when you are done with the demo:")
	fmt.Fprintln(a.out, "  iris engine stop && iris engine uninstall        stop the engine, drop its state")
	fmt.Fprintln(a.out, "  iris uninstall                                   remove the iris binary itself")
	if dir, off := a.executableDirOffPATH(); off {
		fmt.Fprintln(a.out)
		fmt.Fprintf(a.out, "Note: %s is not on your PATH; add it to call iris from anywhere.\n", dir)
	}
	fmt.Fprintln(a.out)
	fmt.Fprintln(a.out, p.rainbow("Enjoy iris."))
}

// executableDirOffPATH resolves the running binary's directory and reports it
// when absent from PATH -- the installer-handoff case, where ~/.local/bin may
// not be exported yet. Any resolution failure reports nothing: the note is
// advisory.
func (a *app) executableDirOffPATH() (string, bool) {
	resolve := a.executablePath
	if resolve == nil {
		resolve = os.Executable
	}
	exe, err := resolve()
	if err != nil {
		return "", false
	}
	dir := filepath.Dir(exe)
	for _, entry := range filepath.SplitList(os.Getenv("PATH")) {
		if entry != "" && filepath.Clean(entry) == dir {
			return "", false
		}
	}
	return dir, true
}
