package cli

import (
	"github.com/spf13/cobra"

	"github.com/MateusAMP2119/iris-engine-cli/internal/pat"
)

// This file is the CLI side of `iris pat create` (specification sections 7 and 8).
// The command validates its scopes and grant intent locally -- a bare invocation or
// an unknown scope is a usage error (exit 2), so a malformed request never reaches
// the leader -- then requires a running daemon: minting and persistence are meta
// writes, so they happen on the leader (single-writer path), which prints the
// show-once token exactly once. The leader-side mint route lands with the read-API
// route tasks this substrate is built for; until then a valid request that reaches a
// running daemon is handled there, and none reachable is exit 3 with start guidance.

// patCreate is the handler for `iris pat create`. It resolves and validates the
// requested scopes and data-read grants locally, then reaches the leader to mint and
// persist the PAT (the leader shows the token once). Validation failures are usage
// errors (exit 2); a missing daemon is exit 3.
func (a *app) patCreate() runE {
	return func(cmd *cobra.Command, _ []string) error {
		rawScopes, _ := cmd.Flags().GetStringSlice("scope")
		reads, _ := cmd.Flags().GetStringSlice("read")
		endpoints, _ := cmd.Flags().GetStringSlice("endpoint")

		// Nothing defaults to everything: a PAT needs an explicit, non-empty scope set.
		if len(rawScopes) == 0 {
			return a.usage("pat create requires at least one --scope from {control, read, data}")
		}
		scopes, err := pat.ParseScopes(rawScopes)
		if err != nil {
			return a.usage(err.Error())
		}
		if err := pat.ValidateScopes(scopes); err != nil {
			return a.usage(err.Error())
		}

		// Read grants are the data scope's alone: --read/--endpoint expand into a data
		// PAT's read role, so they are meaningless without it (specification section 7).
		if (len(reads) > 0 || len(endpoints) > 0) && !hasScope(scopes, pat.ScopeData) {
			return a.usage("--read and --endpoint require --scope data")
		}

		// Minting and persistence are leader meta writes: reach the daemon (the leader
		// shows the token once). None reachable is exit 3 with start guidance.
		return a.requireDaemon(cmd, "pat create")
	}
}

// hasScope reports whether want is in scopes.
func hasScope(scopes []pat.Scope, want pat.Scope) bool {
	for _, s := range scopes {
		if s == want {
			return true
		}
	}
	return false
}
