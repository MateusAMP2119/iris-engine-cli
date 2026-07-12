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
	"unicode/utf8"

	"github.com/spf13/cobra"

	"github.com/MateusAMP2119/iris-engine-cli/internal/config"
	"github.com/MateusAMP2119/iris-engine-cli/internal/daemon"
)

// promptKind names the flavor of a quickstart-tour question, so an injected
// tourPrompt (and the production terminal prompt) can tell what is being
// asked.
type promptKind int

const (
	// promptAct is an act-gate consent: one affirmative opens the act and its
	// steps run straight through (specification section 8, quickstart surface).
	promptAct promptKind = iota
)

// promptAnswer is the operator's answer to one tour question.
type promptAnswer int

const (
	// answerProceed opens the act.
	answerProceed promptAnswer = iota
	// answerQuit stops the tour: a clean abort, exit 0 with a resume hint.
	answerQuit
)

// tourPromptFunc is the signature of the tourPrompt seam.
type tourPromptFunc = func(question string, kind promptKind) (promptAnswer, error)

// tourInputFunc is the signature of the tourInput seam: one line read whose
// prompt carries a visible default. The caller applies def to an empty answer;
// the seam only reads.
type tourInputFunc = func(prompt, def string) (string, error)

// errTourAborted is the internal signal for a clean tour abort: a decline,
// EOF, or interrupt at an act's opening question. The sequencer maps it to
// tourAbort (exit 0, resume hint); it never escapes runQuickstartTour.
var errTourAborted = errors.New("quickstart: tour aborted")

// The tour's pinned prompt copy. The act gate defaults to proceed on an empty
// answer (Y/n); anything unrecognized reads as quit, so a typo never runs a
// real command.
const (
	// tourActGateQuestion opens an act that has no opening question of its own
	// (THE PIPELINE, until the catalog shop replaces its interior): one
	// affirmative and the act's steps run straight through.
	tourActGateQuestion = "Open this act? (Y/n)"
	// tourPipelineIntro is THE PIPELINE act's one-line intro above its gate.
	tourPipelineIntro = "Register the hello_iris sample, run it, and ask a row who wrote it."
	// tourDefaultWorkspace is the workspace question's visible default anywhere
	// the invoking directory is not already a workspace: never the invoking
	// directory itself, which under `curl | sh` is arbitrary.
	tourDefaultWorkspace = "~/iris"
)

// chapterRuleWidth is the chapter rule's total column count, matching the
// 48-column uninstall.sh confirmation box.
const chapterRuleWidth = 48

// tourSession carries one tour invocation's resolved seams and context into
// the act openers.
type tourSession struct {
	ctx    context.Context
	p      painter
	yes    bool
	prompt tourPromptFunc
	input  tourInputFunc
}

// tourIO owns the tour's terminal dialogue: ONE shared reader over the process
// stdin serves both the Y/n act gates and the line reads, so a line buffered
// ahead is never dropped between questions. Questions go to errOut (a prompt
// is dialogue, never command output).
type tourIO struct {
	errOut io.Writer
	reader *bufio.Reader
}

// newTourIO builds the production tour dialogue over the process stdin.
func newTourIO(errOut io.Writer) *tourIO {
	return &tourIO{errOut: errOut, reader: bufio.NewReader(os.Stdin)}
}

// ask asks one Y/n act-gate question. EOF -- a closed stdin -- answers quit,
// the clean-abort path; only a real read fault surfaces as an error.
func (t *tourIO) ask(question string, _ promptKind) (promptAnswer, error) {
	fmt.Fprintf(t.errOut, "%s ", question)
	line, err := t.reader.ReadString('\n')
	if err != nil && !errors.Is(err, io.EOF) {
		return answerQuit, fmt.Errorf("quickstart: read prompt answer: %w", err)
	}
	if line == "" && err != nil {
		return answerQuit, nil
	}
	switch strings.ToLower(strings.TrimSpace(line)) {
	case "", "y", "yes":
		return answerProceed, nil
	default:
		// n, no, q, or anything unrecognized: never run a real command on a typo.
		return answerQuit, nil
	}
}

// readLine asks one line question (the prompt carries its visible default) and
// returns the raw answer. A closed stdin with nothing read returns io.EOF, the
// clean-abort path; only a real read fault surfaces as a wrapped error.
func (t *tourIO) readLine(prompt, _ string) (string, error) {
	fmt.Fprintf(t.errOut, "%s ", prompt)
	line, err := t.reader.ReadString('\n')
	if err != nil && !errors.Is(err, io.EOF) {
		return "", fmt.Errorf("quickstart: read prompt answer: %w", err)
	}
	if line == "" && err != nil {
		return "", io.EOF
	}
	return strings.TrimRight(line, "\r\n"), nil
}

// tourActs returns the runnable act table: the canonical quickstartActs with
// THE ENGINE's opener wired to the workspace prompt (the act's opening
// question, doubling as its consent).
func (a *app) tourActs() []tourAct {
	acts := quickstartActs()
	for i := range acts {
		if acts[i].id == tourActEngine {
			acts[i].opener = a.openEngineWorkspace
		}
	}
	return acts
}

// chapterMark renders one chapter's mark. On the ceremony surface it is the
// light rule-and-title device -- `  ── THE ENGINE ─────…`, the uninstall.sh
// box family at lighter weight, one 48-column rule in the act's palette color.
// On a plain surface the act is still named (a bare title line) but the rule
// artwork never reaches a non-terminal consumer.
func chapterMark(p painter, color func(string) string, title string) string {
	if !p.enabled {
		return title
	}
	lead := "── " + title + " "
	pad := chapterRuleWidth - utf8.RuneCountInString(lead)
	if pad < 0 {
		pad = 0
	}
	return "  " + color(lead+strings.Repeat("─", pad))
}

// actColor picks an act's bright palette color along the shared rainbow: cyan
// for THE ENGINE, magenta for THE PIPELINE -- and yellow, THE CLI's color
// (install.sh's act), for anything else.
func actColor(p painter, id string) func(string) string {
	switch id {
	case tourActEngine:
		return p.cyan
	case tourActPipeline:
		return p.magenta
	default:
		return p.yellow
	}
}

// runQuickstartTour is the chaptered guided tour of the first session
// (specification section 8): after the welcome (skipped for the installer's
// continuation, whose banner was the welcome) it walks the acts -- chapter
// mark, one consent (THE ENGINE's workspace question, or the act gate), then
// the act's steps straight through the in-process runner. A reachable daemon
// on the workspace socket announces install/start as already done and skips
// them. Declines, EOF, and interrupts abort clean (exit 0, resume hint); the
// first failing step surfaces its own error and exit category; yes runs
// everything unattended in the invoking directory.
func (a *app) runQuickstartTour(cmd *cobra.Command, yes, fromInstaller bool) error {
	base := cmd.Context()
	if base == nil {
		base = context.Background()
	}
	// The Ctrl-C path: cancellation makes the open question (or the gap between
	// steps) read as a clean abort. A signal during a step also cancels that
	// step's in-process command through the shared context.
	ctx, stop := signal.NotifyContext(base, os.Interrupt, syscall.SIGTERM)
	defer stop()

	p := a.newPainter(false)
	tio := newTourIO(a.errOut)
	prompt := a.tourPrompt
	if prompt == nil {
		prompt = tio.ask
	}
	input := a.tourInput
	if input == nil {
		input = tio.readLine
	}
	run := a.runStep
	if run == nil {
		run = a.runTourChild
	}
	s := &tourSession{ctx: ctx, p: p, yes: yes, prompt: prompt, input: input}

	if !fromInstaller {
		a.quickstartWelcome(p)
	}

	acts := a.tourActs()
	total := 0
	for _, act := range acts {
		total += len(act.steps)
	}

	k := 0 // the global step count, for the failure message's step m/n
	for _, act := range acts {
		if ctx.Err() != nil {
			return a.tourAbort()
		}
		fmt.Fprintln(a.out)
		fmt.Fprintln(a.out, chapterMark(p, actColor(p, act.id), act.title))

		// One consent opens the act: its own opening question when it has one
		// (THE ENGINE's workspace prompt), the generic act gate otherwise. --yes
		// answers everything.
		switch {
		case act.opener != nil:
			if err := act.opener(s); err != nil {
				if errors.Is(err, errTourAborted) {
					return a.tourAbort()
				}
				return err
			}
		case !yes:
			fmt.Fprintln(a.out, tourPipelineIntro)
			ans, perr := askTour(ctx, prompt, tourActGateQuestion, promptAct)
			if perr != nil || ans != answerProceed || ctx.Err() != nil {
				a.reportPromptFault(perr)
				return a.tourAbort()
			}
		}

		steps := act.steps
		if act.id == tourActEngine {
			// The tour only ever targets the local workspace engine it provisions:
			// an ambient host (IRIS_HOST or an iris.toml host -- the flag is refused
			// outright) is announced once and ignored, both for this probe and
			// inside every child step (the child apps resolve with forceLocalTarget
			// set). Resolved after the workspace question so the socket default is
			// the tour workspace's.
			settings := a.resolveTarget(cmd)
			if settings.Host != "" {
				fmt.Fprintln(a.out, p.dim(fmt.Sprintf(
					"Ignoring the configured remote host %s — the tour only targets the local workspace engine.", settings.Host)))
				settings.Host = ""
			}
			// Adaptive skip: every step is idempotent, so a daemon already answering
			// on the workspace socket means install and start are done -- announce
			// under the ENGINE chapter and skip, never ask anything extra.
			if a.probeDaemon(ctx, settings) == nil {
				fmt.Fprintf(a.out, "An engine is already running on this workspace's socket — %s and %s are already done; skipping ahead.\n",
					strings.Join(steps[0].Argv, " "), strings.Join(steps[1].Argv, " "))
				k += 2
				steps = steps[2:]
			} else if settings.Managed() && daemon.IsManagedInstalled(settings) {
				fmt.Fprintln(a.out, "The managed Postgres is already installed; the install step only verifies it (every step is idempotent).")
			}
		}

		for _, step := range steps {
			if ctx.Err() != nil {
				return a.tourAbort()
			}
			k++
			a.renderTourStep(p, step)
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
				return a.tourStepFailed(step, k, total, code)
			}
		}
	}

	a.tourWrapUp(p)
	return nil
}

// renderTourStep frames one step: its explanation, then the literal command in
// the update staged grammar (`→ <command> …`) on the ceremony surface, or a
// plain `$ <command>` line anywhere else -- the real command output follows.
func (a *app) renderTourStep(p painter, step quickstartStep) {
	cmd := strings.Join(step.Argv, " ")
	fmt.Fprintln(a.out)
	fmt.Fprintf(a.out, "  %s\n", step.Explanation)
	if p.enabled {
		fmt.Fprintf(a.out, "  %s %s %s\n", p.cyan("→"), p.green(cmd), p.dim("…"))
	} else {
		fmt.Fprintf(a.out, "  $ %s\n", cmd)
	}
}

// openEngineWorkspace is THE ENGINE act's opener: the workspace question
// (specification section 8, quickstart surface). Interactive, it reads one
// line with a visible default -- `~/iris`, or the invoking directory when that
// is already a workspace (.iris/ or pipelines/ present) -- expands `~` to the
// operator's home, creates the directory (mkdir -p) and enters it, so every
// subsequent step operates on cwd exactly like any command. The empty answer
// accepts the default AND consents to the act; `q`, EOF, and an interrupt
// abort clean (errTourAborted). Under --yes it never prompts: the invoking
// directory is the workspace, unchanged. A real filesystem fault is a
// quickstart_workspace fault, exit 4.
func (a *app) openEngineWorkspace(s *tourSession) error {
	wd, err := os.Getwd()
	if err != nil {
		return &fault{code: exitOpFailed, codeStr: "quickstart_workspace",
			message: fmt.Sprintf("quickstart: resolve the current directory: %v", err)}
	}
	if s.yes {
		a.announceWorkspace(s.p, wd)
		return nil
	}

	def := tourDefaultWorkspace
	if isWorkspaceDir(wd) {
		def = wd
	}
	line, perr := askTourLine(s.ctx, s.input, "Engine workspace ["+def+"]:", def)
	if perr != nil || s.ctx.Err() != nil {
		a.reportPromptFault(perr)
		return errTourAborted
	}
	answer := strings.TrimSpace(line)
	if answer == "" {
		answer = def
	}
	if strings.EqualFold(answer, "q") {
		return errTourAborted
	}

	dir, err := expandUserPath(answer)
	if err != nil {
		return err
	}
	abs, err := filepath.Abs(dir)
	if err != nil {
		return &fault{code: exitOpFailed, codeStr: "quickstart_workspace",
			message: fmt.Sprintf("quickstart: resolve workspace path %s: %v", dir, err)}
	}
	// MkdirAll: re-running the tour adopts an existing workspace rather than
	// failing; 0755 because a workspace is traversable project source, not a
	// private artifact.
	if err := os.MkdirAll(abs, 0o755); err != nil {
		return &fault{code: exitOpFailed, codeStr: "quickstart_workspace",
			message: fmt.Sprintf("quickstart: create workspace %s: %v", abs, err)}
	}
	if err := os.Chdir(abs); err != nil {
		return &fault{code: exitOpFailed, codeStr: "quickstart_workspace",
			message: fmt.Sprintf("quickstart: enter workspace %s: %v", abs, err)}
	}
	a.announceWorkspace(s.p, abs)
	return nil
}

// announceWorkspace confirms the resolved workspace in the staged grammar
// (`✓ workspace <abs>`), green on the ceremony surface and plain otherwise --
// the one line every subsequent step's cwd is anchored to.
func (a *app) announceWorkspace(p painter, dir string) {
	fmt.Fprintf(a.out, "  %s\n", p.green("✓ workspace "+dir))
}

// expandUserPath expands a leading ~ to the operator's home directory. Only
// the bare `~` and `~/...` forms are supported: `~user` would need a passwd
// lookup os.UserHomeDir cannot do, so it is refused with a clear fault rather
// than silently treated as a relative directory name.
func expandUserPath(path string) (string, error) {
	if path == "~" || strings.HasPrefix(path, "~/") {
		home, err := os.UserHomeDir()
		if err != nil {
			return "", &fault{code: exitOpFailed, codeStr: "quickstart_workspace",
				message: fmt.Sprintf("quickstart: resolve your home directory for %s: %v", path, err)}
		}
		return filepath.Join(home, strings.TrimPrefix(path, "~")), nil
	}
	if strings.HasPrefix(path, "~") {
		return "", &fault{code: exitOpFailed, codeStr: "quickstart_workspace",
			message: fmt.Sprintf("quickstart: %s: ~user paths are not supported; use an absolute path or ~/<dir>", path)}
	}
	return path, nil
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

// askTour asks one act-gate question through prompt while honoring ctx: a
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

// askTourLine reads one line answer through input while honoring ctx, the
// line-read sibling of askTour: a cancellation (Ctrl-C) wins over a pending
// read -- the caller sees it via ctx.Err() -- and the abandoned goroutine read
// never outlives the tour by more than the process itself.
func askTourLine(ctx context.Context, input tourInputFunc, prompt, def string) (string, error) {
	type outcome struct {
		line string
		err  error
	}
	ch := make(chan outcome, 1)
	go func() {
		line, err := input(prompt, def)
		ch <- outcome{line: line, err: err}
	}()
	select {
	case <-ctx.Done():
		return "", nil
	case o := <-ch:
		return o.line, o.err
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
// carried across, so a harnessed parent stays harnessed through its steps. The
// child resolves with forceLocalTarget set: an ambient IRIS_HOST or iris.toml
// host never reaches a step -- the tour tours the local workspace engine only.
func (a *app) runTourChild(ctx context.Context, args []string) int {
	child := newAppWithLogger(a.out, a.errOut, a.logger)
	child.forceLocalTarget = true
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

// reportPromptFault surfaces a real prompt read fault on errOut before the
// tour aborts: the abort stays clean (exit 0), but the fault is never
// swallowed. EOF stays silent -- a closed stdin is the ordinary decline, not a
// fault (the production dialogue already maps a no-input EOF to it).
func (a *app) reportPromptFault(perr error) {
	if perr != nil && !errors.Is(perr, io.EOF) {
		fmt.Fprintf(a.errOut, "iris: %v\n", perr)
	}
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
