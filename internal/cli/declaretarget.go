package cli

import (
	"github.com/spf13/cobra"

	"github.com/MateusAMP2119/iris-engine-cli/internal/declare"
)

// declareTargetStub is the handler for declare apply/destroy. Cobra's
// ExactArgs(1) (set at command construction) already enforces the single-
// target rule and rejects a bare or multi-file invocation before this ever
// runs (specification sections 3, 6.3, 8, and 12: exactly one declaration
// file per invocation, no --all, bare invocation exits 2). This resolves and
// parses that one target -- a file named iris-declare.yaml, or a folder
// resolved to its iris-declare.yaml, with no workspace sweep or transitive
// chaining -- and surfaces a resolution or parse failure as operation-failed
// (exit 4) naming the problem. A resolved target then passes, unchanged, to
// the daemon-dial stub: the command's real apply/destroy semantics land in a
// later epic (E03.9/E03.10).
func (a *app) declareTargetStub(op string) runE {
	return func(cmd *cobra.Command, args []string) error {
		if _, _, err := declare.LoadDeclarationFile(args[0]); err != nil {
			return &fault{code: exitOpFailed, codeStr: "declare_target", message: err.Error()}
		}
		return a.requireDaemon(cmd, op)
	}
}
