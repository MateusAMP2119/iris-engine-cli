package cli

import (
	"encoding/json"
	"fmt"
	"os"
	"strings"

	"github.com/spf13/cobra"

	"github.com/MateusAMP2119/iris-engine-cli/internal/buildinfo"
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

// The stable act ids of the chaptered tour: every step carries its act in the
// --json envelope, and the interactive sequencer keys its chapter marks and
// openers on them.
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

// quickstartPipelineSteps returns THE PIPELINE act's steps for one catalog
// entry: apply its pipeline folder, run it (the explanation carrying the
// entry's own run note), then the provenance finale on the entry's showcase
// table and pk.
func quickstartPipelineSteps(e catalogEntry) []quickstartStep {
	return []quickstartStep{
		{
			ID:          "apply",
			Explanation: fmt.Sprintf("Register the %s sample: its pipeline, role, grants, and the %s table.", e.ID, e.Showcase.Table),
			Argv:        []string{"iris", "declare", "apply", "pipelines/" + e.ID},
			Act:         tourActPipeline,
		},
		{
			ID:          "run",
			Explanation: fmt.Sprintf("Run it: %s.", e.RunNote),
			Argv:        []string{"iris", "pipeline", "run", e.ID},
			Act:         tourActPipeline,
		},
		{
			ID:          "provenance",
			Explanation: "Ask a row who wrote it: the authoring run, the layer stack, the hashes.",
			Argv:        []string{"iris", "data", "provenance", e.Showcase.Table, e.Showcase.PK},
			Act:         tourActPipeline,
		},
	}
}

// quickstartActsFor returns the canonical chaptered step table of the guided
// first session for one catalog entry: THE ENGINE (install, start -d, info)
// then THE PIPELINE (apply, run, provenance on the entry's showcase). Every
// rendering -- the interactive tour, the plain act-headed guide, and the --json
// envelope -- shares this one table. It is built fresh per call (no mutable
// package state); openers are wired by the tour (tourActs), so the guide
// renderings stay pure data.
func quickstartActsFor(e catalogEntry) []tourAct {
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
			steps: quickstartPipelineSteps(e),
		},
	}
}

// quickstartActs returns the act table for the catalog's default entry. It is the
// default-entry shorthand behind quickstartSteps; the live surfaces select their
// entry first -- the shop's interactive pick, an explicit --pipeline, or the
// default under --yes -- and call quickstartActsFor with it. A malformed embedded
// catalog surfaces as the error.
func quickstartActs() ([]tourAct, error) {
	cat, err := loadCatalog()
	if err != nil {
		return nil, err
	}
	return quickstartActsFor(cat.defaultEntry()), nil
}

// quickstartSteps flattens the default-entry act table into the ordered step
// list every rendering shares.
func quickstartSteps() ([]quickstartStep, error) {
	acts, err := quickstartActs()
	if err != nil {
		return nil, err
	}
	var steps []quickstartStep
	for _, act := range acts {
		steps = append(steps, act.steps...)
	}
	return steps, nil
}

// quickstartCmd builds `iris quickstart`: the third root verb beside the
// lifecycle pair, the installer's continuation -- the guided tour of the first
// session. It is daemonless: the tour runs before any engine exists (it
// bootstraps one). Interactivity requires stdin AND stdout to both be
// interactive terminals with --json off; --yes runs the whole tour unattended
// (piped or not) with the invoking directory as the workspace; any other
// invocation renders the plain act-headed guide -- or, under --json, the
// step-list data envelope -- executing nothing and exiting 0.
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
	c.Flags().String("pipeline", "", "pick this catalog pipeline explicitly (skips the shop browse; the plain guide and --json list the catalog)")
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
			return a.usage("iris quickstart tours this machine and provisions a local engine, so --host is refused; drop --host and run the tour locally (a local --socket stays accepted), or point this workspace at a remote engine with iris engine connect <host>")
		}
		cat, err := loadCatalog()
		if err != nil {
			return &fault{code: exitOpFailed, codeStr: "quickstart_catalog",
				message: fmt.Sprintf("quickstart: load the embedded pipeline catalog: %v", err)}
		}
		// --pipeline picks explicitly in every rendering; an unknown id is a
		// usage error naming the available ids. --yes without --pipeline takes
		// the default, entry 1.
		selected := cat.defaultEntry()
		explicit := false
		if id, ok := changedString(cmd, "pipeline"); ok && id != "" {
			e, eerr := cat.entryByID(id)
			if eerr != nil {
				return a.usage(fmt.Sprintf("iris quickstart --pipeline: %v", eerr))
			}
			selected, explicit = e, true
		}
		if jsonMode, _ := cmd.Flags().GetBool("json"); jsonMode {
			return a.renderQuickstartJSON(cat, selected)
		}
		yes, _ := cmd.Flags().GetBool("yes")
		fromInstaller, _ := cmd.Flags().GetBool("from-installer")
		if yes || (a.stdoutTTY() && a.stdinTTY()) {
			return a.runQuickstartTour(cmd, yes, fromInstaller, cat, selected, explicit)
		}
		return a.renderQuickstartGuide(cat, selected)
	}
}

// quickstartWelcome paints the standalone tour's opening: the two acts by
// name (THE PIPELINE's line parameterized by an explicit --pipeline pick),
// how consent works, and how the tour ends. The installer's continuation
// (--from-installer) skips it -- install.sh's banner was the welcome.
func (a *app) quickstartWelcome(p painter, selected catalogEntry, explicit bool) {
	pipelineLine := "pick a starter from the pipeline catalog, run it, ask a row who wrote it"
	if explicit {
		pipelineLine = fmt.Sprintf("register the %s pipeline, run it, ask a row who wrote it", selected.ID)
	}
	if p.enabled {
		// The ceremony surface opens the clack rail; the plain rendering below
		// stays byte-stable for every non-terminal consumer.
		clackIntro(a.out, p, "Welcome to iris — the guided first session ("+buildinfo.Version+")")
		fmt.Fprintf(a.out, "%s  The tour runs the real first session in two acts:\n", p.dim(railBar))
		fmt.Fprintf(a.out, "%s    %s — provision the engine and start it\n", p.dim(railBar), p.cyan("THE ENGINE"))
		fmt.Fprintf(a.out, "%s    %s — %s\n", p.dim(railBar), p.magenta("THE PIPELINE"), pipelineLine)
		fmt.Fprintf(a.out, "%s\n", p.dim(railBar))
		fmt.Fprintf(a.out, "%s  Every step is the real command; it ends with the engine left running.\n", p.dim(railBar))
		fmt.Fprintf(a.out, "%s\n", p.dim(railBar))
		clackOutro(a.out, p, "tour continues below")
		return
	}
	fmt.Fprintln(a.out, p.cyan("Welcome to iris — the guided first session."))
	fmt.Fprintln(a.out)
	fmt.Fprintln(a.out, "The tour runs the real first session in two acts:")
	fmt.Fprintf(a.out, "  %s — provision the engine and start it\n", p.cyan("THE ENGINE"))
	fmt.Fprintf(a.out, "  %s — %s\n", p.magenta("THE PIPELINE"), pipelineLine)
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

// renderQuickstartGuide writes the plain copy-paste guide: the catalog block
// and the selected entry's canonical steps under plain-text act headings,
// numbered through, as byte-stable plain text (pinned by a golden file), zero
// ANSI, executing nothing.
func (a *app) renderQuickstartGuide(cat *pipelineCatalog, selected catalogEntry) error {
	var b strings.Builder
	b.WriteString("iris quickstart — the guided first session\n")
	b.WriteString("\n")
	b.WriteString("This is the plain guide: the tour's steps as numbered copy-paste commands,\n")
	b.WriteString("executing nothing. Run `iris quickstart` in an interactive terminal for the\n")
	b.WriteString("guided version: two acts, one question opening each, the steps then running\n")
	b.WriteString("for real — materializing your pick from the embedded pipeline catalog into\n")
	b.WriteString("the workspace for you.\n")
	b.WriteString("\n")
	b.WriteString("The pipeline catalog (pick with --pipeline <id>; entry 1 is the default):\n")
	b.WriteString("\n")
	for i, e := range cat.Entries {
		fmt.Fprintf(&b, "  %d. %s — %s\n", i+1, e.ID, e.Pitch)
	}
	b.WriteString("\n")
	fmt.Fprintf(&b, "The steps below use %s.\n", selected.ID)
	b.WriteString("\n")
	n := 0
	for _, act := range quickstartActsFor(selected) {
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

// quickstartCatalogPayload is the --json envelope's additive catalog object:
// the default and selected entry ids beside every entry's browsable metadata.
type quickstartCatalogPayload struct {
	Default  string                          `json:"default"`
	Selected string                          `json:"selected"`
	Entries  []quickstartCatalogEntryPayload `json:"entries"`
}

// quickstartCatalogEntryPayload is one catalog entry in the --json envelope.
type quickstartCatalogEntryPayload struct {
	ID       string          `json:"id"`
	Name     string          `json:"name"`
	Pitch    string          `json:"pitch"`
	Showcase catalogShowcase `json:"showcase"`
}

// quickstartGuide is the --json payload of `iris quickstart`: the selected
// entry's ordered step list, each step carrying its act, plus the additive
// catalog object.
type quickstartGuide struct {
	Steps   []quickstartStep         `json:"steps"`
	Catalog quickstartCatalogPayload `json:"catalog"`
}

// renderQuickstartJSON emits the guide as the one data envelope on stdout.
func (a *app) renderQuickstartJSON(cat *pipelineCatalog, selected catalogEntry) error {
	var steps []quickstartStep
	for _, act := range quickstartActsFor(selected) {
		steps = append(steps, act.steps...)
	}
	payload := quickstartCatalogPayload{Default: cat.defaultEntry().ID, Selected: selected.ID}
	for _, e := range cat.Entries {
		payload.Entries = append(payload.Entries, quickstartCatalogEntryPayload{
			ID: e.ID, Name: e.Name, Pitch: e.Pitch, Showcase: e.Showcase,
		})
	}
	return json.NewEncoder(a.out).Encode(dataEnvelope{Data: quickstartGuide{Steps: steps, Catalog: payload}})
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
