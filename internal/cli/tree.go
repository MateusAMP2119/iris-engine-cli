package cli

import (
	"fmt"
	"sort"
	"strings"

	"github.com/spf13/cobra"
)

// runE is the signature of a cobra command handler.
type runE = func(cmd *cobra.Command, args []string) error

// The lifecycle annotation classifies each leaf command as daemonless (runnable
// without a daemon: the fixed roster of specification section 2) or daemon-
// touching (must reach a running daemon, or fail fast with start guidance). It is
// set explicitly at command construction, so later epics (and the traceability
// sweep) read the annotation rather than a string list.
const (
	// lifecycleAnnotation is the cobra Annotations key carrying the classification.
	lifecycleAnnotation = "iris.lifecycle"
	// lifecycleDaemonless marks a command that runs without a daemon.
	lifecycleDaemonless = "daemonless"
	// lifecycleDaemonTouching marks a command that must reach a running daemon.
	lifecycleDaemonTouching = "daemon-touching"
)

// withLifecycle records a leaf command's daemon-lifecycle classification and
// returns it, for chaining at construction.
func withLifecycle(c *cobra.Command, life string) *cobra.Command {
	if c.Annotations == nil {
		c.Annotations = map[string]string{}
	}
	c.Annotations[lifecycleAnnotation] = life
	return c
}

// daemonTouching marks c as a command that must reach a running daemon.
func daemonTouching(c *cobra.Command) *cobra.Command {
	return withLifecycle(c, lifecycleDaemonTouching)
}

// daemonless marks c as a command in the specification section 2 daemonless
// roster.
func daemonless(c *cobra.Command) *cobra.Command {
	return withLifecycle(c, lifecycleDaemonless)
}

// newRootCommand builds the iris command tree: the root, its global persistent
// flags, and the noun-verb subcommands of specification section 8. The tree is
// built fresh per invocation from a constructor (no package globals, no init),
// so it closes over the invocation's app for output and logging.
func (a *app) newRootCommand() *cobra.Command {
	root := &cobra.Command{
		Use:           "iris",
		Short:         "Iris - provenance-first data engine and pipeline orchestrator",
		SilenceErrors: true, // errors are rendered by renderError, not cobra
		SilenceUsage:  true, // usage never pollutes stdout under --json
		Args:          cobra.NoArgs,
		// A bare `iris` prints help (exit 0), but under --json it must still emit a
		// single JSON document rather than human help text on stdout.
		RunE: func(cmd *cobra.Command, _ []string) error {
			if jsonMode, _ := cmd.Flags().GetBool("json"); jsonMode {
				return a.describeJSON(cmd)
			}
			return cmd.Help()
		},
	}
	root.CompletionOptions.DisableDefaultCmd = true
	// Tag flag-parse failures so the error path can tell them from post-parse
	// errors when resolving the output mode.
	root.SetFlagErrorFunc(func(_ *cobra.Command, err error) error { return &flagError{err: err} })

	// Global flags, on every command by inheritance (specification section 8).
	pf := root.PersistentFlags()
	pf.Bool("json", false, "emit a single JSON document on stdout instead of human-readable output")
	pf.String("socket", "", "path to the engine's Unix control socket")
	pf.String("host", "", "address of a remote engine reached over TCP (host:port or http://host:port for plain, https://host:port for TLS)")
	pf.String("token", "", "PAT presented to a remote engine over TCP")

	root.AddCommand(
		a.declareCmd(),
		a.pipelineCmd(),
		a.runCmd(),
		a.dataCmd(),
		a.workloadCmd(),
		a.engineCmd(),
		a.deadletterCmd(),
		a.endpointCmd(),
		a.patCmd(),
	)
	return root
}

// group builds a noun (or sub-noun) node: a command that owns verbs but does no
// work itself. A bare invocation is a usage error (exit 2), consistent with the
// resource-first tree and the spec's bare-declare rule, so no group node ever
// prints human help to stdout under --json.
func (a *app) group(use, short string, children ...*cobra.Command) *cobra.Command {
	c := &cobra.Command{
		Use:   use,
		Short: short,
		Args:  cobra.NoArgs,
		RunE:  a.groupStub(),
	}
	c.AddCommand(children...)
	return c
}

// groupStub is a group node's handler: it names the available subcommands and
// returns a usage error.
func (a *app) groupStub() runE {
	return func(cmd *cobra.Command, _ []string) error {
		return a.usage(fmt.Sprintf("%q needs a subcommand: %s",
			cmd.CommandPath(), strings.Join(visibleChildNames(cmd), ", ")))
	}
}

// daemonStub is the handler of a command that must reach a running daemon: it
// dials the resolved daemon and, with none reachable, reports no-daemon (exit 3)
// with start guidance, never auto-starting one. When the daemon is reachable the
// command is not wired yet, so it reports not-implemented (exit 4); the command's
// real body lands in a later epic.
func (a *app) daemonStub(op string) runE {
	return func(cmd *cobra.Command, _ []string) error { return a.requireDaemon(cmd, op) }
}

// visibleChildNames returns the sorted names of a command's user-facing
// subcommands, skipping hidden and cobra's built-in help/completion commands.
func visibleChildNames(cmd *cobra.Command) []string {
	var out []string
	for _, c := range cmd.Commands() {
		if c.Hidden || c.Name() == "help" || c.Name() == "completion" {
			continue
		}
		out = append(out, c.Name())
	}
	sort.Strings(out)
	return out
}

// addConfirmFlags registers the --yes/--force flags of a destructive command
// (--yes honors soft blocks, --force overrides).
func addConfirmFlags(c *cobra.Command) {
	c.Flags().Bool("yes", false, "proceed past soft blocks without prompting")
	c.Flags().Bool("force", false, "override blocks, including hard ones")
}

// addScopeFlags registers the --pipeline/--all worklist scope of a dead-letter
// replay or drain.
func addScopeFlags(c *cobra.Command) {
	c.Flags().String("pipeline", "", "scope to one pipeline's entries")
	c.Flags().Bool("all", false, "scope to every outstanding entry")
}

// declareCmd builds `iris declare`: apply and destroy one declaration file. A
// bare `iris declare` is a usage error by design (no --all).
func (a *app) declareCmd() *cobra.Command {
	apply := &cobra.Command{
		Use:   "apply <path>",
		Short: "Register and apply one iris-declare.yaml (pipeline or composer)",
		Args:  cobra.ExactArgs(1),
		RunE:  a.declareApply(),
	}
	apply.Flags().Bool("dry-run", false, "report what would change without touching anything")

	destroy := &cobra.Command{
		Use:   "destroy <path>",
		Short: "Tear down one declaration: its pipeline, role, grants, and un-promoted data",
		Args:  cobra.ExactArgs(1),
		RunE:  a.declareDestroy(),
	}
	destroy.Flags().Bool("dry-run", false, "report what would be torn down without touching anything")
	addConfirmFlags(destroy)

	return a.group("declare",
		"Register and tear down the workload graph, one declaration file per invocation",
		daemonTouching(apply), daemonTouching(destroy))
}

// pipelineCmd builds `iris pipeline`: the single-unit lifecycle and reads.
func (a *app) pipelineCmd() *cobra.Command {
	build := &cobra.Command{
		Use: "build <name>", Short: "Build source into the self-contained binary, recording its content hash",
		Args: cobra.ExactArgs(1), RunE: a.pipelineBuild(),
	}
	promote := &cobra.Command{
		Use: "promote <name>", Short: "Mark the pipeline's data permanent (gated on built)",
		Args: cobra.ExactArgs(1), RunE: a.pipelinePromote(),
	}
	run := &cobra.Command{
		Use: "run <name>", Short: "Trigger a manual one-off run",
		Args: cobra.ExactArgs(1), RunE: a.pipelineRun(),
	}
	list := &cobra.Command{
		Use: "list", Short: "List pipelines with a queued or running run",
		Args: cobra.NoArgs, RunE: a.pipelineList(),
	}
	list.Flags().Bool("all", false, "list every pipeline, not only active ones")
	show := &cobra.Command{
		Use: "show <name>", Short: "Show a pipeline's resolved declaration, role, grants, recent runs, and gate ledger",
		Args: cobra.ExactArgs(1), RunE: a.pipelineShow(),
	}

	return a.group("pipeline", "Operate on a single pipeline",
		daemonTouching(build), daemonTouching(promote), daemonTouching(run), daemonTouching(list), daemonTouching(show))
}

// runCmd builds `iris run`: execution-record reads and cancel.
func (a *app) runCmd() *cobra.Command {
	list := &cobra.Command{
		Use: "list", Short: "List run history",
		Args: cobra.NoArgs, RunE: a.daemonStub("run list"),
	}
	list.Flags().Bool("graph", false, "draw lineage rails instead of a flat list")
	list.Flags().Bool("ascii", false, "render the graph with ASCII glyphs")
	list.Flags().String("after", "", "only runs after this run")
	list.Flags().String("before", "", "only runs before this run")

	show := &cobra.Command{
		Use: "show <run>", Short: "Show one run: state, snapshot pin, artifact and declaration hashes",
		Args: cobra.ExactArgs(1), RunE: a.daemonStub("run show"),
	}
	show.Flags().Bool("trace", false, "show ancestry over run_inputs")
	show.Flags().Bool("down", false, "with --trace, walk descendants instead of ancestors")

	logs := &cobra.Command{
		Use: "logs <run>", Short: "Tail a run's captured output",
		Args: cobra.ExactArgs(1), RunE: a.daemonStub("run logs"),
	}
	cancel := &cobra.Command{
		Use: "cancel <run>", Short: "Cancel one running run (kills its process group)",
		Args: cobra.ExactArgs(1), RunE: a.daemonStub("run cancel"),
	}

	return a.group("run", "Inspect and control execution records",
		daemonTouching(list), daemonTouching(show), daemonTouching(logs), daemonTouching(cancel))
}

// dataCmd builds `iris data`: row-level provenance, the single computed read.
func (a *app) dataCmd() *cobra.Command {
	prov := &cobra.Command{
		Use:   "provenance <schema.table> <pk>",
		Short: "Show a row's provenance: author, layer stack, consumed upstreams, hashes",
		Args:  cobra.ExactArgs(2), RunE: a.daemonStub("data provenance"),
	}
	return a.group("data", "Row-level reads", daemonTouching(prov))
}

// workloadCmd builds `iris workload`: the standing wiring panel and the dev
// loop's data scope.
func (a *app) workloadCmd() *cobra.Command {
	show := &cobra.Command{
		Use: "show [pipeline]", Short: "Show the wiring panel: lanes, composer walk, gate state per edge",
		Args: cobra.MaximumNArgs(1), RunE: a.daemonStub("workload show"),
	}
	wipe := &cobra.Command{
		Use: "wipe [pipeline]", Short: "Revert un-promoted disposable data, all of it or one pipeline's",
		Args: cobra.MaximumNArgs(1), RunE: a.daemonStub("workload wipe"),
	}
	addConfirmFlags(wipe)

	return a.group("workload", "The standing wiring and the dev loop's data scope",
		daemonTouching(show), daemonTouching(wipe))
}

// engineCmd builds `iris engine`: the daemon, its state, and its service unit.
// Local-lifecycle verbs (start/install/uninstall/service) do not dial a daemon
// and report not-implemented; the rest reach a running daemon.
func (a *app) engineCmd() *cobra.Command {
	start := &cobra.Command{
		Use: "start", Short: "Run an engine candidate (foreground; -d to detach)",
		Args: cobra.NoArgs, RunE: a.engineStart(),
	}
	// Daemon-scoped flags live only on engine start (specification section 8).
	start.Flags().BoolP("detach", "d", false, "detach and run the engine in the background")
	start.Flags().String("pg-dsn", "", "DSN of an external Postgres to use instead of the managed one")
	start.Flags().String("retain", "", "run-history retention count")
	start.Flags().String("journal-partition-rows", "", "rows per journal partition before sealing")
	start.Flags().String("objects-path", "", "filesystem path for the local object store")
	start.Flags().String("tcp", "", "address to expose the read API and control plane over TCP")
	start.Flags().String("tls-cert", "", "TLS certificate for the TCP listener")
	start.Flags().String("tls-key", "", "TLS key for the TCP listener")

	stop := &cobra.Command{
		Use: "stop", Short: "Stop a detached daemon (graceful SIGTERM)",
		Args: cobra.NoArgs, RunE: a.engineStop(),
	}
	install := &cobra.Command{
		Use: "install", Short: "Download and place the managed Postgres, then create meta and set up the socket",
		Args: cobra.NoArgs, RunE: a.engineInstall(),
	}
	uninstall := &cobra.Command{
		Use: "uninstall", Short: "Full engine teardown (gated)",
		Args: cobra.NoArgs, RunE: a.engineUninstall(),
	}
	addConfirmFlags(uninstall)
	info := &cobra.Command{
		Use: "info", Short: "Show engine and version info, role, listeners, uptime",
		Args: cobra.NoArgs, RunE: a.engineInfo(),
	}
	logs := &cobra.Command{
		Use: "logs", Short: "Tail the daemon log",
		Args: cobra.NoArgs, RunE: a.daemonStub("engine logs"),
	}
	inspect := &cobra.Command{
		Use: "inspect", Short: "Dump the engine-table DDL, read-only",
		Args: cobra.NoArgs, RunE: a.engineInspect(),
	}
	stats := &cobra.Command{
		Use: "stats", Short: "Show rollups: run, lane, and dead-letter counts",
		Args: cobra.NoArgs, RunE: a.engineStats(),
	}

	svcInstall := &cobra.Command{
		Use: "install", Short: "Generate and install the platform service unit (systemd/launchd)",
		Args: cobra.NoArgs, RunE: a.engineServiceInstall(),
	}
	svcInstall.Flags().String("path", "", "write the unit to this path instead of the workspace-local default")
	svcUninstall := &cobra.Command{
		Use: "uninstall", Short: "Remove the installed service unit",
		Args: cobra.NoArgs, RunE: a.engineServiceUninstall(),
	}
	svcUninstall.Flags().String("path", "", "remove the unit at this path instead of the workspace-local default")
	service := a.group("service", "Manage the platform service unit",
		daemonless(svcInstall), daemonless(svcUninstall))

	return a.group("engine", "Manage the daemon, its state, and its service unit",
		daemonless(start), daemonTouching(stop), daemonless(install), daemonless(uninstall),
		daemonTouching(info), daemonTouching(logs), daemonTouching(inspect), daemonTouching(stats), service)
}

// deadletterCmd builds `iris deadletter` (sole alias: dl). replay and drain
// require a scope: <run>, --pipeline <name>, or --all; a bare invocation is a
// usage error.
func (a *app) deadletterCmd() *cobra.Command {
	list := &cobra.Command{
		Use: "list", Short: "List outstanding entries",
		Args: cobra.NoArgs, RunE: a.daemonStub("deadletter list"),
	}
	show := &cobra.Command{
		Use: "show <run>", Short: "Show one entry: reason, error, failed_upstream, blast radius",
		Args: cobra.ExactArgs(1), RunE: a.daemonStub("deadletter show"),
	}
	replay := &cobra.Command{
		Use: "replay [run]", Short: "Replay root causes (auto-walks failed_upstream)",
		Args: cobra.MaximumNArgs(1), RunE: a.deadletterReplay(),
	}
	addScopeFlags(replay)
	drain := &cobra.Command{
		Use: "drain [run]", Short: "Discard entries: worklist only, nothing re-runs (gated)",
		Args: cobra.MaximumNArgs(1), RunE: a.deadletterDrain(),
	}
	addScopeFlags(drain)
	addConfirmFlags(drain)

	c := a.group("deadletter", "The dead-letter worklist",
		daemonTouching(list), daemonTouching(show), daemonTouching(replay), daemonTouching(drain))
	c.Aliases = []string{"dl"} // the one and only alias in the tree
	return c
}

// endpointCmd builds `iris endpoint`: declared read surfaces with their own
// lifecycle, apart from declare apply.
func (a *app) endpointCmd() *cobra.Command {
	apply := &cobra.Command{
		Use: "apply [name]", Short: "Publish endpoints/ (or one): validate, compile, atomic",
		Args: cobra.MaximumNArgs(1), RunE: a.daemonStub("endpoint apply"),
	}
	remove := &cobra.Command{
		Use: "remove <name>", Short: "Retire a read surface (shape only, no data touched)",
		Args: cobra.ExactArgs(1), RunE: a.daemonStub("endpoint remove"),
	}
	list := &cobra.Command{
		Use: "list", Short: "List declared endpoints",
		Args: cobra.NoArgs, RunE: a.daemonStub("endpoint list"),
	}
	show := &cobra.Command{
		Use: "show <name>", Short: "Show resolved config, compiled shape, source fields",
		Args: cobra.ExactArgs(1), RunE: a.daemonStub("endpoint show"),
	}

	return a.group("endpoint", "Declared read surfaces (own lifecycle)",
		daemonTouching(apply), daemonTouching(remove), daemonTouching(list), daemonTouching(show))
}

// patCmd builds `iris pat`: PAT lifecycle with scopes {control, read, data}.
func (a *app) patCmd() *cobra.Command {
	create := &cobra.Command{
		Use: "create", Short: "Mint a new PAT",
		Args: cobra.NoArgs, RunE: a.patCreate(),
	}
	create.Flags().StringSlice("scope", nil, "PAT scope (repeatable): any non-empty subset of {control, read, data}")
	create.Flags().String("label", "", "human label recorded for the PAT")
	create.Flags().StringSlice("read", nil, "data-PAT read grant (repeatable): schema.table.field, or bare schema.table for all fields declared at mint")
	create.Flags().StringSlice("endpoint", nil, "data-PAT read grant (repeatable): expand an endpoint's source fields")
	list := &cobra.Command{
		Use: "list", Short: "List PATs",
		Args: cobra.NoArgs, RunE: a.daemonStub("pat list"),
	}
	revoke := &cobra.Command{
		Use: "revoke <pat>", Short: "Revoke a PAT",
		Args: cobra.ExactArgs(1), RunE: a.daemonStub("pat revoke"),
	}

	return a.group("pat", "Manage PATs with scopes {control, read, data}",
		daemonTouching(create), daemonTouching(list), daemonTouching(revoke))
}
