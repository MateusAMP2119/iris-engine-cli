package cli

import (
	"encoding/json"
	"fmt"
	"strings"

	"github.com/spf13/cobra"

	"github.com/MateusAMP2119/iris-engine-cli/internal/api"
)

// This file is the CLI side of `iris endpoint apply` (specification section 7):
// publishing declared read surfaces, apart from declare apply (endpoints have their
// own lifecycle). Publishing is a leader meta write plus a data-database
// prepare-verify, so it happens on the leader: the command POSTs the optional
// endpoint name to the daemon's /endpoint/apply route, which discovers and compiles
// the endpoints/ tree from its own workspace, verifies each derived statement
// against the data database, persists atomically, and swaps the shapes into the live
// serving registry. Exit codes follow the section-8 categories: success 0, no daemon
// 3, operation failed 4, not leader 6.

// endpointApply is the handler for `iris endpoint apply`. It sends the optional
// endpoint name (empty publishes every declared endpoint) to the daemon's endpoint
// apply route and reports the endpoints published.
func (a *app) endpointApply() runE {
	return func(cmd *cobra.Command, args []string) error {
		name := ""
		if len(args) == 1 {
			name = args[0]
		}
		var res api.EndpointApplyResult
		if err := a.postDaemonJSON(cmd, "/endpoint/apply", api.EndpointApplyRequest{Name: name}, "endpoint apply", &res); err != nil {
			return err
		}
		if jsonMode, _ := cmd.Flags().GetBool("json"); jsonMode {
			return json.NewEncoder(a.out).Encode(dataEnvelope{Data: res})
		}
		if len(res.Applied) == 0 {
			fmt.Fprintln(a.out, "no endpoints applied")
			return nil
		}
		fmt.Fprintf(a.out, "applied endpoint(s): %s\n", strings.Join(res.Applied, ", "))
		return nil
	}
}
