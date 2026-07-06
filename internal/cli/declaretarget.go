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

// declareApply is the handler for `iris declare apply`. It resolves and parses the
// single target declaration (like declareTargetStub), computes the advisory
// warnings the apply surfaces -- cross-mode reads and the like (specification
// section 5) -- through the applyWarnings seam, then stashes them so they ride the
// command's terminal --json envelope. Crucially the warnings accompany apply; they
// never replace it: the handler still falls through to the daemon dial, so a
// warning can never silently suppress the actual apply.
//
// The applyWarnings seam is nil in production, so pre-daemon apply computes no
// warnings and this reduces to the resolve-then-dial stub of E03.3: the meta-backed
// data-mode facts the warning needs, and the full apply, arrive in E03.9/E03.10,
// which will carry these same warnings on the success envelope. What is real from
// now is the warning structure and its place in the terminal --json envelope.
func (a *app) declareApply() runE {
	return func(cmd *cobra.Command, args []string) error {
		_, decl, err := declare.LoadDeclarationFile(args[0])
		if err != nil {
			return &fault{code: exitOpFailed, codeStr: "declare_target", message: err.Error()}
		}
		if a.applyWarnings != nil {
			a.warnings = a.applyWarnings(decl)
		}
		return a.requireDaemon(cmd, "declare apply")
	}
}
