package cli

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"

	"github.com/spf13/cobra"

	"github.com/MateusAMP2119/iris-lakehouse/internal/api"
)

// This file is the CLI side of `iris engine inspect`: the read-only
// engine-table DDL dump. The CLI GETs the daemon's /inspect route and prints
// exactly the statements the route serves -- under --json the same data
// envelope any HTTP consumer reads. The dump renders the embedded schema model,
// so inspect reads no rows and mutates no engine state; it is served on any
// role. Transport failure is no-daemon (exit 3) with start guidance, any other
// failure operation-failed (exit 4).

// engineInspect is the handler for `iris engine inspect`: it GETs the daemon's
// /inspect DDL dump and renders it.
func (a *app) engineInspect() runE {
	return func(cmd *cobra.Command, _ []string) error {
		settings := a.resolveTarget(cmd)
		client, base, overTCP := a.daemonHTTPClient(settings)

		hreq, err := http.NewRequestWithContext(cmd.Context(), http.MethodGet, base+"/inspect", nil)
		if err != nil {
			return &fault{code: exitOpFailed, codeStr: "request", message: fmt.Sprintf("engine inspect: build request: %v", err)}
		}
		if overTCP && settings.Token != "" {
			hreq.Header.Set("Authorization", "Bearer "+settings.Token)
		}

		resp, err := client.Do(hreq)
		if err != nil {
			a.logger.Debug("no iris daemon reachable", "op", "engine inspect", "socket", settings.Socket, "host", settings.Host, "err", err)
			return a.noDaemonFault()
		}
		defer func() {
			_, _ = io.Copy(io.Discard, resp.Body)
			_ = resp.Body.Close()
		}()

		if resp.StatusCode != http.StatusOK {
			return a.controlFailure(resp, "engine inspect")
		}
		var env struct {
			Data api.InspectPayload `json:"data"`
		}
		if err := json.NewDecoder(resp.Body).Decode(&env); err != nil {
			return &fault{code: exitOpFailed, codeStr: "decode", message: fmt.Sprintf("engine inspect: decode daemon response: %v", err)}
		}

		if jsonMode, _ := cmd.Flags().GetBool("json"); jsonMode {
			return json.NewEncoder(a.out).Encode(dataEnvelope{Data: env.Data})
		}
		for _, stmt := range env.Data.DDL {
			fmt.Fprintln(a.out, stmt)
		}
		return nil
	}
}
