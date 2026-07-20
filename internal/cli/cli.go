// Package cli builds the Iris command-line surface: the cobra noun-verb command
// tree, the global flags, and the exit-code and --json output contracts. It sits
// at the top of the product import graph (nothing imports it), and cmd/iris is a
// thin entrypoint over Execute. Interactive full-screen views live in
// internal/tui; this package owns command wiring only.
//
// Nearly every command handler is real: it resolves its target, dials the daemon
// over the unix socket or TCP, and maps the reply onto the contract that framed
// the surface from the first day -- the categorical exit codes, the single-JSON
// document on stdout under --json, and the strict separation of log output
// (stderr) from command output (stdout). A handful of read verbs are still
// unwired stubs (run show, engine logs, deadletter list, endpoint
// list/show/remove, pat list/revoke): they carry the daemonStub handler, which
// reports "no daemon reachable" (exit 3) with guidance to start the engine when
// nothing answers, and "not implemented" (exit 4) once a daemon does.
package cli

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"strings"

	"github.com/spf13/cobra"

	"github.com/MateusAMP2119/iris-lakehouse/internal/declare"
	"github.com/MateusAMP2119/iris-lakehouse/internal/tui"
	"github.com/MateusAMP2119/iris-lakehouse/internal/update"
)

// The exit codes are the categories the CLI reports outcomes with. Detail rides the
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
// exit code (one of the exit-code categories above). It writes command output to
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
	// daemonTLSConfig overrides the TLS client config the daemon-reachability probe
	// uses for an https:// host. It is nil in production (standard verification
	// against the system trust store) and injected by tests to trust a self-signed
	// test CA. A remote-control epic can promote it to a real --tls-ca flag.
	daemonTLSConfig *tls.Config
	// applyWarnings computes the advisory warnings `iris declare apply` surfaces for
	// a parsed declaration -- cross-mode reads and the like. It is still nil in
	// production: the data-mode facts it needs live in meta, which only the daemon
	// reads, and the daemon's /apply route returns no such warnings (the cross-mode
	// check runs on `iris pipeline promote` instead, in internal/dispatch), so a
	// production apply computes no local warnings and proceeds unchanged. Tests
	// inject it to drive the --json warning surface, proving the warning structure
	// rides the envelope.
	applyWarnings func(*declare.Declaration) []declare.Warning
	// warnings are the advisory warnings a handler computed for the current
	// invocation. They accompany the terminal outcome (they never replace it): the
	// terminal --json envelope carries them as a warnings array, and human output
	// prints them to stderr. Set per invocation, so the app holds no global state.
	warnings []declare.Warning
	// runUpdate performs the `iris update` self-replace, returning the
	// outcome for the running version; snapshot selects the rolling development
	// channel instead of the latest stable release. It is nil in production (the
	// handler falls back to update.New().Run); tests inject a fake to drive the
	// exit-code and output surface without network or filesystem I/O.
	runUpdate func(ctx context.Context, current string, snapshot bool) (update.Result, error)
	// confirm is the confirmation seam for the destructive ops' interactive prompts
	// (typed-name for teardowns, y/N for dev-loop ops). When non-nil it is consulted
	// when neither --yes nor --force was supplied. The name is the target of the
	// operation (pipeline name, or "engine" for uninstall); isTeardown chooses the
	// prompt style. It is nil in production: `iris uninstall` then falls back to
	// terminalConfirm (a real y/N read off the terminal), while the ops gated through
	// confirmOrFlags -- declare destroy, deadletter drain, workload wipe, engine
	// uninstall -- refuse without --yes/--force rather than prompt. Tests inject it to
	// simulate TTY answers without a real terminal.
	confirm func(name string, isTeardown bool) (bool, error)
	// executablePath resolves the running iris binary's real on-disk path (through
	// its symlinks), the file `iris uninstall` removes. It is nil in production
	// (the handler falls back to resolveSelfPath: os.Executable then
	// filepath.EvalSymlinks); tests inject it to point at a throwaway file so the
	// removal never touches the test binary.
	executablePath func() (string, error)
	// isTTY reports whether the command's stdout is an interactive terminal, the
	// gate for the lifecycle-command terminal ceremony (the colored update journey
	// and uninstall farewell). It is nil in production (the painter falls back to
	// stdoutIsTerminal, an os.Stdout char-device stat honored only when a.out is the
	// real stdout); tests inject it to force styled or plain rendering without a real
	// terminal.
	isTTY func() bool
	// stdinIsTTY reports whether the command's stdin is an interactive terminal.
	// It is nil in production (callers fall back to stdinIsTerminal, an os.Stdin
	// char-device stat, the same check terminalConfirm uses); tests inject it to
	// drive either rendering without a real terminal.
	stdinIsTTY func() bool
	// connectInput reads one line answer to an `iris engine connect` question
	// (the host, when neither the argument nor the configuration supplied one).
	// It is nil in production (the handler falls back to a plain stdin read);
	// tests inject it to script the answer without a real terminal.
	connectInput func(prompt string) (string, error)
	// connectSecret reads one hidden line answer to `iris engine connect`'s PAT
	// question. It is nil in production (the handler falls back to a no-echo
	// terminal read); tests inject it to script the answer without a real
	// terminal.
	connectSecret func(prompt string) (string, error)
	// psLive runs the `iris ps` live view over the first fetched snapshot,
	// reporting whether the view actually entered the terminal (false falls the
	// command back to the JSON emit -- stdin refused raw mode). It is nil in
	// production (the handler falls back to tui.RunLive, the real alternate-screen
	// view); tests inject it to drive the output-mode gate without a terminal.
	psLive func(cmd *cobra.Command, c *tui.Client, first tui.Snapshot, target string) (bool, error)
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

// stdoutTTY resolves the stdout half of an interactivity gate through the
// injectable isTTY seam, falling back to the real stdout char-device stat.
func (a *app) stdoutTTY() bool {
	if a.isTTY != nil {
		return a.isTTY()
	}
	return a.stdoutIsTerminal()
}

// stdinTTY resolves the stdin half of an interactivity gate through the
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

// run builds and executes the command tree and maps the outcome to an exit-code
// category, with a background context.
func (a *app) run(args []string) int {
	return a.runContext(context.Background(), args)
}

// runContext builds and executes the command tree under ctx and maps the outcome
// to an exit-code category. On error it resolves the output mode -- honoring
// exactly how pflag consumed --json -- and renders the error accordingly.
func (a *app) runContext(ctx context.Context, args []string) int {
	root := a.newRootCommand()
	root.SetArgs(args)
	root.SetOut(a.out)
	root.SetErr(a.errOut)
	cmd, err := root.ExecuteContextC(ctx)
	if err == nil {
		return exitOK
	}
	a.jsonMode = a.jsonModeAfterError(cmd, err, args)
	return a.renderError(err)
}

// fault is a command outcome carrying an exit-code category, a machine code for
// the --json error envelope, and a human message.
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
// iris.toml > defaults) so the socket/host it would have dialed is real, and
// logs the diagnostic to stderr (never stdout) at debug
// level, off by default.
func (a *app) noDaemon(cmd *cobra.Command, op string) error {
	target := a.resolveTarget(cmd)
	a.logger.Debug("no iris daemon reachable", "op", op, "socket", target.Socket, "host", target.Host)
	return &fault{
		code:    exitNoDaemon,
		codeStr: "no_daemon",
		message: `Cannot connect to the iris engine. Is the engine running? Start it with "iris engine start", or target a running engine with --socket or --host`,
	}
}

// usage is a usage-error outcome (exit 2) raised by a handler, distinct from the
// arg/flag errors cobra raises before a handler runs.
func (a *app) usage(msg string) error {
	return &fault{code: exitUsage, codeStr: "usage", message: msg}
}

// parseRunRef parses a <run> token per the run-ref grammar:
// a bare pipeline name stands for its latest run; <name>~n stands for the nth
// prior run of that pipeline (n>=0; ~0 is latest). Git ^ and .. forms are
// rejected as false cognates. The parse is pure; resolution of the (name, prior)
// to a concrete run id is the caller's I/O (latest run + offset by id desc).
func parseRunRef(s string) (pipeline string, prior int, err error) {
	if s == "" {
		return "", 0, fmt.Errorf("empty run ref")
	}
	if strings.Contains(s, "^") || strings.Contains(s, "..") {
		return "", 0, fmt.Errorf("git reachability forms ^ and .. are not run refs: %s", s)
	}
	if idx := strings.Index(s, "~"); idx >= 0 {
		name := s[:idx]
		if name == "" {
			return "", 0, fmt.Errorf("run ref must have pipeline name before ~: %s", s)
		}
		nstr := s[idx+1:]
		if nstr == "" {
			return "", 0, fmt.Errorf("~ requires a number: %s", s)
		}
		var n int
		if _, scanErr := fmt.Sscanf(nstr, "%d", &n); scanErr != nil || n < 0 || fmt.Sprintf("%d", n) != nstr {
			return "", 0, fmt.Errorf("~n requires non-negative integer n: %s", s)
		}
		return name, n, nil
	}
	return s, 0, nil
}

// errEnvelope is the --json error document: the read-API error envelope shape,
// {"error":{"code":...,"message":...}}, plus any advisory warnings that
// accompany the outcome (omitted when there are none). A warning
// never blocks a command; it rides the terminal envelope alongside the error.
type errEnvelope struct {
	Error    errBody           `json:"error"`
	Warnings []declare.Warning `json:"warnings,omitempty"`
}

// errBody is the error object inside errEnvelope.
type errBody struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

// dataEnvelope is the --json success document: the read-API success envelope
// shape, {"data":...}.
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
		_ = json.NewEncoder(a.out).Encode(errEnvelope{
			Error:    errBody{Code: f.codeStr, Message: f.message},
			Warnings: a.warnings,
		})
	} else {
		for _, w := range a.warnings {
			fmt.Fprintf(a.errOut, "iris: warning: %s\n", w.Message)
		}
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

// confirmOrFlags returns true if --yes or --force is set, or if the injected
// confirm seam (for tests or interactive TTY) approves. isTeardown selects
// prompt flavor for the seam.
func (a *app) confirmOrFlags(cmd *cobra.Command, name string, isTeardown bool) (bool, error) {
	yes, _ := cmd.Flags().GetBool("yes")
	force, _ := cmd.Flags().GetBool("force")
	if yes || force {
		return true, nil
	}
	if a.confirm != nil {
		return a.confirm(name, isTeardown)
	}
	return false, nil
}
