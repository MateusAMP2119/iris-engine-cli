package cli

import (
	"encoding/json"
	"fmt"

	"github.com/spf13/cobra"

	"github.com/MateusAMP2119/iris-lakehouse/internal/api"
	"github.com/MateusAMP2119/iris-lakehouse/internal/pat"
)

// This file is the CLI side of `iris pat create`. The command validates its
// scopes and grant intent locally -- a bare invocation or
// an unknown scope is a usage error (exit 2), so a malformed request never reaches
// the leader -- then reaches the leader to mint and persist the PAT: minting, the
// read-role provisioning, and the meta writes are leader work (single-writer path),
// and the leader returns the show-once token in its response. The CLI prints that
// token exactly once; it is never stored and never recoverable (revoke and re-mint).

// patCreate is the handler for `iris pat create`. It resolves and validates the
// requested scopes and data-read grants locally, then POSTs to the daemon's
// /pat/create route and prints the show-once token the leader returns. Validation
// failures are usage errors (exit 2); a missing daemon is exit 3; a not-leader
// daemon is exit 6; an operation failure (a bad grant, an unknown endpoint) is exit 4.
func (a *app) patCreate() runE {
	return func(cmd *cobra.Command, _ []string) error {
		rawScopes, _ := cmd.Flags().GetStringSlice("scope")
		reads, _ := cmd.Flags().GetStringSlice("read")
		endpoints, _ := cmd.Flags().GetStringSlice("endpoint")
		label, _ := cmd.Flags().GetString("label")

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
		// PAT's read role, so they are meaningless without it.
		if (len(reads) > 0 || len(endpoints) > 0) && !hasScope(scopes, pat.ScopeData) {
			return a.usage("--read and --endpoint require --scope data")
		}

		req := api.PATCreateRequest{Scopes: rawScopes, Label: label, Reads: reads, Endpoints: endpoints}
		var res api.PATCreateResult
		if err := a.postDaemonJSON(cmd, "/pat/create", req, "pat create", &res); err != nil {
			return err
		}
		if jsonMode, _ := cmd.Flags().GetBool("json"); jsonMode {
			return json.NewEncoder(a.out).Encode(dataEnvelope{Data: res})
		}
		// The show-once token: printed exactly once, never stored, never recoverable.
		fmt.Fprintf(a.out, "%s\n", res.Token)
		fmt.Fprintf(a.errOut, "iris: PAT %s created (scopes: %v); this token is shown once and cannot be recovered\n", res.ID, res.Scopes)
		return nil
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
