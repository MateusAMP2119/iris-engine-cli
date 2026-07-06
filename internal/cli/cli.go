// Package cli builds the Iris command-line surface: the cobra noun-verb command
// tree, the global flags, and the exit-code and --json output contracts of
// specification section 8. It sits at the top of the product import graph
// (nothing imports it), and cmd/iris is a thin entrypoint over Execute.
//
// The command handlers are stubs during epic E01. What is real from day one is
// the contract around those stubs: the categorical exit codes, the single-JSON
// document on stdout under --json, and the strict separation of log output
// (stderr) from command output (stdout). A stub that would reach a running
// daemon reports "no daemon reachable" (exit 3) with guidance to start the
// engine -- the honest current behavior, since no daemon exists yet -- and a
// stub that would act on the local host but is not wired yet reports "not
// implemented" (exit 4).
package cli

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"

	"github.com/spf13/cobra"
)

// The exit codes are the categories of specification section 8. Detail rides the
// message or the --json envelope; the CLI never emits a code outside this set,
// and in particular overrides cobra's default exit 1.
const (
	exitOK           = 0 // success
	exitUsage        = 2 // usage error (bad flags/args, unknown command, bare noun)
	exitNoDaemon     = 3 // no daemon reachable (start guidance)
	exitOpFailed     = 4 // operation failed (includes not-yet-implemented stubs)
	exitDeadLettered = 5 // run dead-lettered
	exitNotLeader    = 6 // not the leader
)

// Execute builds the command tree, runs it against args, and returns the process
// exit code (a specification section 8 category). It writes command output to
// stdout and human-readable errors to stderr, and never calls os.Exit, so it is
// drivable from tests. cmd/iris is a thin wrapper that passes os.Args[1:] and the
// process streams and exits with the returned code.
func Execute(args []string, stdout, stderr io.Writer) int {
	return newApp(stdout, stderr).run(args)
}

// app carries one invocation's output streams, logger, and resolved output mode.
// It is constructed per Execute call and threaded into the command handlers as a
// closure receiver, so the package holds no mutable global state and no init().
type app struct {
	out      io.Writer
	errOut   io.Writer
	logger   *slog.Logger
	jsonMode bool
}

// newApp builds an app whose structured logs go to stderr at info level, keeping
// stdout free for command output. Tests use newAppWithLogger to inject a logger.
func newApp(stdout, stderr io.Writer) *app {
	logger := slog.New(slog.NewTextHandler(stderr, &slog.HandlerOptions{Level: slog.LevelInfo}))
	return newAppWithLogger(stdout, stderr, logger)
}

// newAppWithLogger builds an app with an explicit logger, so a test can assert
// where log output lands relative to command output.
func newAppWithLogger(stdout, stderr io.Writer, logger *slog.Logger) *app {
	return &app{out: stdout, errOut: stderr, logger: logger}
}

// run builds and executes the command tree and maps the outcome to an exit-code
// category. On error it resolves the output mode -- honoring exactly how pflag
// consumed --json -- and renders the error accordingly.
func (a *app) run(args []string) int {
	root := a.newRootCommand()
	root.SetArgs(args)
	root.SetOut(a.out)
	root.SetErr(a.errOut)
	cmd, err := root.ExecuteC()
	if err == nil {
		return exitOK
	}
	a.jsonMode = a.jsonModeAfterError(cmd, err, args)
	return a.renderError(err)
}

// fault is a command outcome carrying a specification section 8 exit-code
// category, a machine code for the --json error envelope, and a human message.
type fault struct {
	code    int
	codeStr string
	message string
}

// Error implements error; the message is the human-readable outcome.
func (f *fault) Error() string { return f.message }

// flagError wraps a cobra/pflag flag-parsing failure so the error path can tell
// it apart from a post-parse error. It matters only for resolving the output
// mode: after a clean parse the parsed --json flag is authoritative (it reflects
// exactly what pflag consumed), but a flag-parse error may have stopped before
// --json was reached, so that path re-resolves the mode from a pflag probe.
type flagError struct{ err error }

// Error implements error with the wrapped pflag message.
func (e *flagError) Error() string { return e.err.Error() }

// Unwrap exposes the wrapped pflag error.
func (e *flagError) Unwrap() error { return e.err }

// noDaemon is the outcome of a stub that must reach a running daemon while none
// is reachable: exit 3, with guidance to start the engine folded into the
// message so it rides both the human output and the --json envelope. It resolves
// the dial target through the configuration precedence (flags > IRIS_* env >
// iris.toml > defaults, specification section 8) so the socket/host it would have
// dialed is real, and logs the diagnostic to stderr (never stdout) at debug
// level, off by default.
func (a *app) noDaemon(cmd *cobra.Command, op string) error {
	target := a.resolveTarget(cmd)
	a.logger.Debug("no iris daemon reachable", "op", op, "socket", target.Socket, "host", target.Host)
	return &fault{
		code:    exitNoDaemon,
		codeStr: "no_daemon",
		message: `no Iris daemon reachable; start the engine with "iris engine start", or target a running daemon with --socket or --host`,
	}
}

// notImplemented is the outcome of a local-lifecycle stub that does not dial a
// daemon and is not wired yet: exit 4 (operation failed).
func (a *app) notImplemented(what string) error {
	return &fault{code: exitOpFailed, codeStr: "not_implemented", message: what + " is not implemented yet"}
}

// usage is a usage-error outcome (exit 2) raised by a handler, distinct from the
// arg/flag errors cobra raises before a handler runs.
func (a *app) usage(msg string) error {
	return &fault{code: exitUsage, codeStr: "usage", message: msg}
}

// errEnvelope is the --json error document: the read-API error envelope shape of
// specification section 7, {"error":{"code":...,"message":...}}.
type errEnvelope struct {
	Error errBody `json:"error"`
}

// errBody is the error object inside errEnvelope.
type errBody struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

// dataEnvelope is the --json success document: the read-API success envelope
// shape of specification section 7, {"data":...}.
type dataEnvelope struct {
	Data any `json:"data"`
}

// cliDescription is the payload of `iris --json` (bare root): a machine-readable
// summary of the command surface, so even the root emits one JSON document under
// --json rather than human help text.
type cliDescription struct {
	Usage string   `json:"usage"`
	Nouns []string `json:"nouns"`
}

// renderError writes an error outcome and returns its exit-code category. A
// fault carries its own code; any other error is one of cobra's own arg, flag, or
// unknown-command errors, all of which are usage errors (exit 2) -- cobra's
// default exit 1 is never surfaced. Under --json the error envelope is the single
// document on stdout; otherwise the message is written to stderr, leaving stdout
// clean.
func (a *app) renderError(err error) int {
	var f *fault
	if !errors.As(err, &f) {
		f = &fault{code: exitUsage, codeStr: "usage", message: err.Error()}
	}
	if a.jsonMode {
		_ = json.NewEncoder(a.out).Encode(errEnvelope{Error: errBody{Code: f.codeStr, Message: f.message}})
	} else {
		fmt.Fprintf(a.errOut, "iris: %s\n", f.message)
	}
	return f.code
}

// jsonModeAfterError resolves whether --json was requested, for rendering an
// error. After a clean parse the parsed flag is authoritative: it reflects
// exactly what pflag consumed, so a --json that a value-taking flag swallowed
// (iris --token --json ...) correctly reads as unset. Only a flag-parse error --
// which may have stopped before --json was reached -- falls back to a probe that
// re-parses against the real command tree.
func (a *app) jsonModeAfterError(cmd *cobra.Command, err error, args []string) bool {
	var fe *flagError
	if !errors.As(err, &fe) && cmd != nil {
		if b, gerr := cmd.Flags().GetBool("json"); gerr == nil {
			return b
		}
	}
	return a.probeJSONMode(args)
}

// probeJSONMode reports whether --json is set by re-resolving it against a fresh
// instance of the real command tree, parse-only. It finds the target command for
// args (cobra Find), then parses that command's flags with unknown flags
// whitelisted so parsing does not stop early. Reusing the real tree makes the
// probe consume every flag -- global and per-command alike -- exactly as the real
// parse would, so a --json swallowed as another flag's value (iris run list
// --after --json ...) is never mistaken for the bool, and the set stays correct
// as the tree grows. It is the fallback for the flag-parse-error path, where
// cobra's own parse did not finish.
func (a *app) probeJSONMode(args []string) bool {
	root := a.newRootCommand()
	target, rest, err := root.Find(args)
	if err != nil || target == nil {
		target, rest = root, args
	}
	target.FParseErrWhitelist.UnknownFlags = true
	target.Flags().SetOutput(io.Discard)
	_ = target.ParseFlags(rest)
	b, _ := target.Flags().GetBool("json")
	return b
}

// describeJSON emits the single JSON document for `iris --json` (bare root): a
// data envelope summarizing the command surface, so the root honors the --json
// contract instead of printing human help to stdout.
func (a *app) describeJSON(root *cobra.Command) error {
	desc := cliDescription{
		Usage: "iris <noun> <verb> [target]",
		Nouns: visibleChildNames(root),
	}
	_ = json.NewEncoder(a.out).Encode(dataEnvelope{Data: desc})
	return nil
}
