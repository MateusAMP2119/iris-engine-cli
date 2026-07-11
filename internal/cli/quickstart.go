package cli

import (
	"encoding/json"
	"fmt"
	"os"
	"strings"

	"github.com/spf13/cobra"
)

// quickstartStep is one canonical tour step: a stable id, a one-line
// explanation, and the argv the step runs. argv[0] is the literal "iris" in the
// guide renderings; the interactive sequencer (the tour orchestration task)
// resolves it to the tour's own binary, never a PATH lookup, so every step runs
// the real command implementation.
type quickstartStep struct {
	ID          string   `json:"id"`
	Explanation string   `json:"explanation"`
	Argv        []string `json:"argv"`
}

// quickstartSteps returns the canonical ordered step table of the guided first
// session (specification section 8, quickstart surface): install, start -d,
// info, apply, run, provenance. Every rendering -- the interactive tour, the
// plain numbered guide, and the --json envelope -- shares this one table. It is
// built fresh per call (no mutable package state).
func quickstartSteps() []quickstartStep {
	return []quickstartStep{
		{
			ID:          "install",
			Explanation: "Bootstrap the engine: place the managed Postgres, create the meta and data databases, set up the control socket.",
			Argv:        []string{"iris", "engine", "install"},
		},
		{
			ID:          "start",
			Explanation: "Start the engine daemon in the background; it stays running after the tour.",
			Argv:        []string{"iris", "engine", "start", "-d"},
		},
		{
			ID:          "info",
			Explanation: "Read the engine readout: versions, socket, Postgres mode, role, uptime.",
			Argv:        []string{"iris", "engine", "info"},
		},
		{
			ID:          "apply",
			Explanation: "Register the hello_iris sample: its pipeline, role, grants, and the demo.colors table.",
			Argv:        []string{"iris", "declare", "apply", "pipelines/hello_iris"},
		},
		{
			ID:          "run",
			Explanation: "Run it: the script upserts the seven rainbow colors into demo.colors; re-running layers a second provenance stamp on the same rows.",
			Argv:        []string{"iris", "pipeline", "run", "hello_iris"},
		},
		{
			ID:          "provenance",
			Explanation: "Ask a row who wrote it: the authoring run, the layer stack, the hashes.",
			Argv:        []string{"iris", "data", "provenance", "demo.colors", "green"},
		},
	}
}

// quickstartCmd builds `iris quickstart`: the third root verb beside the
// lifecycle pair (specification section 8), the installer's handoff -- the
// guided tour of the first session. It is daemonless: the tour runs before any
// engine exists (it bootstraps one). Interactivity requires stdin AND stdout to
// both be interactive terminals with --json off; any other invocation renders
// the plain numbered copy-paste guide -- or, under --json, the step-list data
// envelope -- executing nothing and exiting 0.
func (a *app) quickstartCmd() *cobra.Command {
	c := &cobra.Command{
		Use:   "quickstart",
		Short: "Take the guided tour of the first session (explains, confirms, then really runs it)",
		Args:  cobra.NoArgs,
		RunE:  a.runQuickstart(),
	}
	return daemonless(c)
}

// runQuickstart is the handler for `iris quickstart`: it resolves the
// interactivity gate and dispatches to one of the three renderings. Color on
// the interactive path follows the ceremony rule via the shared painter gate
// (interactive stdout, NO_COLOR unset, --json off): NO_COLOR strips paint but
// never interactivity.
func (a *app) runQuickstart() runE {
	return func(cmd *cobra.Command, _ []string) error {
		if jsonMode, _ := cmd.Flags().GetBool("json"); jsonMode {
			return a.renderQuickstartJSON()
		}
		if a.stdoutTTY() && a.stdinTTY() {
			return a.quickstartInteractive(a.newPainter(false))
		}
		return a.renderQuickstartGuide()
	}
}

// quickstartInteractive is the interactive tour surface. In this task it
// renders the welcome and returns; the step sequencer (explain, confirm, run
// through the tour's own binary, adaptive skip, --yes) lands with the tour
// orchestration task and slots in after the welcome. The gates around it -- the
// TTY pair and the ceremony color rule -- are final.
func (a *app) quickstartInteractive(p painter) error {
	a.quickstartWelcome(p)
	return nil
}

// quickstartWelcome paints the tour's opening: what the tour does and how it
// treats the operator's workspace. It is the one piece of the interactive
// surface this task ships.
func (a *app) quickstartWelcome(p painter) {
	fmt.Fprintln(a.out, p.cyan("Welcome to iris — the guided first session."))
	fmt.Fprintln(a.out)
	fmt.Fprintln(a.out, "The tour explains each step, asks before it runs, and really runs it:")
	for i, s := range quickstartSteps() {
		fmt.Fprintf(a.out, "  %d. %s\n", i+1, p.green(strings.Join(s.Argv, " ")))
	}
	fmt.Fprintln(a.out)
	fmt.Fprintln(a.out, "It ends with the engine left running and a cheat-sheet of what you used.")
}

// renderQuickstartGuide writes the plain numbered copy-paste guide: the same
// canonical steps as byte-stable plain text (pinned by a golden file), zero
// ANSI, executing nothing.
func (a *app) renderQuickstartGuide() error {
	var b strings.Builder
	b.WriteString("iris quickstart — the guided first session\n")
	b.WriteString("\n")
	b.WriteString("This is the plain guide: the tour's steps as numbered copy-paste commands,\n")
	b.WriteString("executing nothing. Run `iris quickstart` in an interactive terminal for the\n")
	b.WriteString("guided version, which explains each step, asks before running it, and\n")
	b.WriteString("writes the embedded hello_iris sample (pipelines/hello_iris/ and\n")
	b.WriteString("schemas/demo/colors/) into the workspace for you.\n")
	b.WriteString("\n")
	for i, s := range quickstartSteps() {
		fmt.Fprintf(&b, "  %d. %s\n", i+1, s.Explanation)
		fmt.Fprintf(&b, "     %s\n", strings.Join(s.Argv, " "))
		b.WriteString("\n")
	}
	b.WriteString("Every step is idempotent and safe to re-run; stop the engine later with\n")
	b.WriteString("`iris engine stop`.\n")
	_, err := fmt.Fprint(a.out, b.String())
	return err
}

// quickstartGuide is the --json payload of `iris quickstart`: the ordered step
// list of the tour, executing nothing.
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
