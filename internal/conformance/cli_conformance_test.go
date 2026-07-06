//go:build conformance

package conformance

import (
	"strings"
	"testing"
)

// cliErrEnvelope is the --json error document the CLI emits: the read-API error
// envelope shape of specification section 7.
type cliErrEnvelope struct {
	Error struct {
		Code    string `json:"code"`
		Message string `json:"message"`
	} `json:"error"`
}

// exitCategories is the closed set of specification section 8 exit codes. The
// binary never emits a code outside it (in particular never cobra's default 1).
var exitCategories = map[int]bool{0: true, 2: true, 3: true, 4: true, 5: true, 6: true}

// leafCommands is every leaf command of the tree, as argument paths. The --json
// contract sweep drives each one.
func leafCommands() [][]string {
	return [][]string{
		{"declare", "apply"}, {"declare", "destroy"},
		{"pipeline", "build"}, {"pipeline", "promote"}, {"pipeline", "run"}, {"pipeline", "list"}, {"pipeline", "show"},
		{"run", "list"}, {"run", "show"}, {"run", "logs"}, {"run", "cancel"},
		{"data", "provenance"},
		{"workload", "show"}, {"workload", "wipe"},
		{"engine", "start"}, {"engine", "stop"}, {"engine", "install"}, {"engine", "uninstall"},
		{"engine", "info"}, {"engine", "logs"}, {"engine", "inspect"}, {"engine", "stats"},
		{"engine", "service", "install"}, {"engine", "service", "uninstall"},
		{"deadletter", "list"}, {"deadletter", "show"}, {"deadletter", "replay"}, {"deadletter", "drain"},
		{"endpoint", "apply"}, {"endpoint", "remove"}, {"endpoint", "list"}, {"endpoint", "show"},
		{"pat", "create"}, {"pat", "list"}, {"pat", "revoke"},
	}
}

// TestCLIExitCodesAndJSON drives the real iris binary and proves the exit-code
// and --json output contracts of specification section 8 against it: categorical
// exit codes, no-daemon exit 3 with start guidance, and the single-JSON envelope
// on stdout under --json.
func TestCLIExitCodesAndJSON(t *testing.T) {
	bin := Build(t)

	// spec: S08/exit-code-categories
	t.Run("S08/exit-code-categories", func(t *testing.T) {
		// 0 success: bare invocation prints help and exits clean.
		bin.Run(t, RunOptions{}).RequireExit(t, 0)
		// 2 usage: an unknown command, and a required argument omitted.
		bin.Run(t, RunOptions{Args: []string{"not-a-real-command"}}).RequireExit(t, 2)
		bin.Run(t, RunOptions{Args: []string{"declare", "apply"}}).RequireExit(t, 2)
		// 3 no daemon: a command that must reach a running daemon.
		bin.Run(t, RunOptions{Args: []string{"pipeline", "list"}}).RequireExit(t, 3)
		// 4 operation failed: a local-lifecycle command not wired yet.
		bin.Run(t, RunOptions{Args: []string{"engine", "install"}}).RequireExit(t, 4)

		// Detail rides the message/--json, never an out-of-category code: a broad
		// sweep never yields a code outside the closed set.
		for _, leaf := range leafCommands() {
			res := bin.Run(t, RunOptions{Args: leaf})
			if !exitCategories[res.ExitCode] {
				t.Errorf("iris %s exited %d, outside the specification section 8 categories",
					strings.Join(leaf, " "), res.ExitCode)
			}
		}
	})

	// spec: S08/exit3-no-daemon-guidance
	t.Run("S08/exit3-no-daemon-guidance", func(t *testing.T) {
		// Human mode: guidance to start the engine on stderr.
		res := bin.Run(t, RunOptions{Args: []string{"pipeline", "list"}})
		res.RequireExit(t, 3)
		if !strings.Contains(string(res.Stderr), "engine start") {
			t.Errorf("no-daemon guidance to start the engine missing from stderr:\n%s", res.Stderr)
		}
		// JSON mode: the guidance rides the single envelope on stdout.
		jres := bin.Run(t, RunOptions{Args: []string{"--json", "pipeline", "list"}})
		jres.RequireExit(t, 3)
		var env cliErrEnvelope
		jres.DecodeJSON(t, &env)
		if !strings.Contains(env.Error.Message, "engine start") {
			t.Errorf("no-daemon guidance missing from the --json envelope: %+v", env)
		}
	})

	// spec: S08/json-single-envelope-stdout
	t.Run("S08/json-single-envelope-stdout", func(t *testing.T) {
		// --json: exactly one JSON document on stdout (DecodeJSON enforces one and
		// only one), carrying the error envelope with code and message.
		res := bin.Run(t, RunOptions{Args: []string{"--json", "pipeline", "list"}})
		var env cliErrEnvelope
		res.DecodeJSON(t, &env)
		if env.Error.Code == "" || env.Error.Message == "" {
			t.Errorf("--json envelope missing code/message: %+v", env)
		}
		// Default: human-readable, not a JSON document on stdout. The error is on
		// stderr and stdout stays clean.
		human := bin.Run(t, RunOptions{Args: []string{"pipeline", "list"}})
		if got := strings.TrimSpace(string(human.Stdout)); got != "" {
			t.Errorf("default (human) mode wrote to stdout: %q", got)
		}
		if len(human.Stderr) == 0 {
			t.Errorf("default (human) mode wrote no message to stderr")
		}
	})
}

// TestCLIContractEverywhere sweeps every leaf command under --json and proves the
// two invariants of the CLI contract hold for all of them: the exit code is a
// specification section 8 category, and stdout is exactly one JSON document.
//
// spec: S13/exit-json-contract-everywhere
func TestCLIContractEverywhere(t *testing.T) {
	bin := Build(t)
	for _, leaf := range leafCommands() {
		args := append([]string{"--json"}, leaf...)
		res := bin.Run(t, RunOptions{Args: args})
		if !exitCategories[res.ExitCode] {
			t.Errorf("iris %s exited %d, outside the specification section 8 categories",
				strings.Join(args, " "), res.ExitCode)
		}
		var doc any
		res.DecodeJSON(t, &doc)
	}
}
