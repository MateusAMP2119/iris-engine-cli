package cli

import (
	"encoding/json"
	"fmt"
	"os"
	"strings"

	"github.com/spf13/cobra"
)

// quickstartStep is one canonical tour step: a stable id, a one-line
// explanation, the argv the step runs, and the act it belongs to. argv[0] is
// the literal "iris" in the guide renderings; the interactive sequencer
// resolves it to the tour's own binary, never a PATH lookup, so every step
// runs the real command implementation.
type quickstartStep struct {
	ID          string   `json:"id"`
	Explanation string   `json:"explanation"`
	Argv        []string `json:"argv"`
	Act         string   `json:"act"`
}

// The stable act ids of the chaptered tour (specification section 8): every
// step carries its act in the --json envelope, and the interactive sequencer
// keys its chapter marks and openers on them.
const (
	tourActEngine   = "engine"
	tourActPipeline = "pipeline"
)

// tourAct is one chapter of the guided tour: a stable id, the chapter-mark
// title, the steps that run straight through once the act is opened, and an
// optional opener -- the act's own opening question (THE ENGINE's workspace
// prompt), which doubles as its consent. An act without an opener is gated by
// the generic act-gate question instead.
type tourAct struct {
	id     string
	title  string
	steps  []quickstartStep
	opener func(*tourSession) error
}

// quickstartActs returns the canonical chaptered step table of the guided
// first session (specification section 8, quickstart surface): THE ENGINE
// (install, start -d, info) then THE PIPELINE (apply, run, provenance --
// hardwired to the embedded hello_iris sample until the pipeline catalog
// replaces this act's interior). Every rendering -- the interactive tour, the
// plain act-headed guide, and the --json envelope -- shares this one table. It
// is built fresh per call (no mutable package state); openers are wired by the
// tour (tourActs), so the guide renderings stay pure data.
func quickstartActs() []tourAct {
	return []tourAct{
		{
			id:    tourActEngine,
			title: "THE ENGINE",
			steps: []quickstartStep{
				{
					ID:          "install",
					Explanation: "Bootstrap the engine: place the managed Postgres, create the meta and data databases, set up the control socket.",
					Argv:        []string{"iris", "engine", "install"},
					Act:         tourActEngine,
				},
				{
					ID:          "start",
					Explanation: "Start the engine daemon in the background; it stays running after the tour.",
					Argv:        []string{"iris", "engine", "start", "-d"},
					Act:         tourActEngine,
				},
				{
					ID:          "info",
					Explanation: "Read the engine readout: versions, socket, Postgres mode, role, uptime.",
					Argv:        []string{"iris", "engine", "info"},
					Act:         tourActEngine,
				},
			},
		},
		{
			id:    tourActPipeline,
			title: "THE PIPELINE",
			steps: []quickstartStep{
				{
					ID:          "apply",
					Explanation: "Register the hello_iris sample: its pipeline, role, grants, and the demo.colors table.",
					Argv:        []string{"iris", "declare", "apply", "pipelines/hello_iris"},
					Act:         tourActPipeline,
				},
				{
					ID:          "run",
					Explanation: "Run it: the script upserts the seven rainbow colors into demo.colors; re-running layers a second provenance stamp on the same rows.",
					Argv:        []string{"iris", "pipeline", "run", "hello_iris"},
					Act:         tourActPipeline,
				},
				{
					ID:          "provenance",
					Explanation: "Ask a row who wrote it: the authoring run, the layer stack, the hashes.",
					Argv:        []string{"iris", "data", "provenance", "demo.colors", "green"},
					Act:         tourActPipeline,
				},
			},
		},
	}
}

// quickstartSteps flattens the act table into the ordered step list every
// rendering shares.
func quickstartSteps() []quickstartStep {
	var steps []quickstartStep
	for _, act := range quickstartActs() {
		steps = append(steps, act.steps...)
	}
	return steps
}

// quickstartCmd builds `iris quickstart`: the third root verb beside the
// lifecycle pair (specification section 8), the installer's continuation --
// the guided tour of the first session. It is daemonless: the tour runs before
// any engine exists (it bootstraps one). Interactivity requires stdin AND
// stdout to both be interactive terminals with --json off; --yes runs the
// whole tour unattended (piped or not) with the invoking directory as the
// workspace; any other invocation renders the plain act-headed guide -- or,
// under --json, the step-list data envelope -- executing nothing and exiting 0.
func (a *app) quickstartCmd() *cobra.Command {
	c := &cobra.Command{
		Use:   "quickstart",
		Short: "Take the guided tour of the first session (explains, confirms, then really runs it)",
		Args:  cobra.NoArgs,
		RunE:  a.runQuickstart(),
	}
	// A plain bool, not addConfirmFlags: the tour has no --force tier; --yes only
	// answers its own prompts.
	c.Flags().Bool("yes", false, "run every tour step unattended, without prompting (cwd is the workspace)")
	c.Flags().Bool("from-installer", false, "installer continuation: open directly on the engine act (install.sh's banner was the welcome, its prompt the consent)")
	return daemonless(c)
}

// runQuickstart is the handler for `iris quickstart`: it refuses --host (the
// tour provisions a local engine; --socket stays accepted), then resolves the
// gate and dispatches. --json always renders the step-list envelope, executing
// nothing, even with --yes or --from-installer -- the latter combination is
// install.sh's version probe and must exit 0. --yes or an interactive terminal
// pair runs the real tour; anything else gets the plain guide. Color on the
// tour follows the ceremony rule via the shared painter gate (interactive
// stdout, NO_COLOR unset, --json off): NO_COLOR strips paint but never
// interactivity.
func (a *app) runQuickstart() runE {
	return func(cmd *cobra.Command, _ []string) error {
		if v, ok := changedString(cmd, "host"); ok && v != "" {
			return a.usage("iris quickstart tours this machine and provisions a local engine, so --host is refused; drop --host and run the tour locally (a local --socket stays accepted)")
		}
		if jsonMode, _ := cmd.Flags().GetBool("json"); jsonMode {
			return a.renderQuickstartJSON()
		}
		yes, _ := cmd.Flags().GetBool("yes")
		fromInstaller, _ := cmd.Flags().GetBool("from-installer")
		if yes || (a.stdoutTTY() && a.stdinTTY()) {
			return a.runQuickstartTour(cmd, yes, fromInstaller)
		}
		return a.renderQuickstartGuide()
	}
}

// quickstartWelcome paints the standalone tour's opening: the two acts by
// name, how consent works, and how the tour ends. The installer's continuation
// (--from-installer) skips it -- install.sh's banner was the welcome.
func (a *app) quickstartWelcome(p painter) {
	fmt.Fprintln(a.out, p.cyan("Welcome to iris — the guided first session."))
	fmt.Fprintln(a.out)
	fmt.Fprintln(a.out, "The tour runs the real first session in two acts:")
	fmt.Fprintf(a.out, "  %s — provision the engine and start it\n", p.cyan("THE ENGINE"))
	fmt.Fprintf(a.out, "  %s — register the sample pipeline, run it, ask a row who wrote it\n", p.magenta("THE PIPELINE"))
	fmt.Fprintln(a.out)
	fmt.Fprintln(a.out, "One question opens each act; its steps then run straight through, for real.")
	fmt.Fprintln(a.out, "It ends with the engine left running and a cheat-sheet of what you used.")
}

// actGuideHeading is the plain guide's heading for one act: the same chapters
// as the interactive ceremony, restructured as plain text.
func actGuideHeading(id string) string {
	switch id {
	case tourActEngine:
		return "The engine — provision it and start it:"
	case tourActPipeline:
		return "The pipeline — register, run, ask:"
	default:
		return ""
	}
}

// renderQuickstartGuide writes the plain copy-paste guide: the same canonical
// steps under plain-text act headings, numbered through, as byte-stable plain
// text (pinned by a golden file), zero ANSI, executing nothing.
func (a *app) renderQuickstartGuide() error {
	var b strings.Builder
	b.WriteString("iris quickstart — the guided first session\n")
	b.WriteString("\n")
	b.WriteString("This is the plain guide: the tour's steps as numbered copy-paste commands,\n")
	b.WriteString("executing nothing. Run `iris quickstart` in an interactive terminal for the\n")
	b.WriteString("guided version: two acts, one question opening each, the steps then running\n")
	b.WriteString("for real — writing the embedded hello_iris sample (pipelines/hello_iris/\n")
	b.WriteString("and schemas/demo/colors/) into the workspace for you.\n")
	b.WriteString("\n")
	n := 0
	for _, act := range quickstartActs() {
		b.WriteString(actGuideHeading(act.id) + "\n")
		b.WriteString("\n")
		for _, s := range act.steps {
			n++
			fmt.Fprintf(&b, "  %d. %s\n", n, s.Explanation)
			fmt.Fprintf(&b, "     %s\n", strings.Join(s.Argv, " "))
			b.WriteString("\n")
		}
	}
	b.WriteString("Every step is idempotent and safe to re-run; stop the engine later with\n")
	b.WriteString("`iris engine stop`.\n")
	_, err := fmt.Fprint(a.out, b.String())
	return err
}

// quickstartGuide is the --json payload of `iris quickstart`: the ordered step
// list of the tour, each step carrying its act, executing nothing.
type quickstartGuide struct {
	Steps []quickstartStep `json:"steps"`
}

// renderQuickstartJSON emits the guide as the one data envelope on stdout.
func (a *app) renderQuickstartJSON() error {
	return json.NewEncoder(a.out).Encode(dataEnvelope{Data: quickstartGuide{Steps: quickstartSteps()}})
}

// stdoutTTY resolves the stdout half of the interactivity gate through the
// injectable isTTY seam, falling back to the real stdout char-device stat.
func (a *app) stdoutTTY() bool {
	if a.isTTY != nil {
		return a.isTTY()
	}
	return a.stdoutIsTerminal()
}

// stdinTTY resolves the stdin half of the interactivity gate through the
// injectable stdinIsTTY seam, falling back to the real stdin char-device stat.
func (a *app) stdinTTY() bool {
	if a.stdinIsTTY != nil {
		return a.stdinIsTTY()
	}
	return stdinIsTerminal()
}

// stdinIsTerminal reports whether the process stdin is an interactive terminal:
// a char-device stat, the same detection terminalConfirm uses before prompting.
func stdinIsTerminal() bool {
	stat, err := os.Stdin.Stat()
	return err == nil && stat.Mode()&os.ModeCharDevice != 0
}
