package cli

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"

	"github.com/spf13/cobra"

	"github.com/MateusAMP2119/iris-engine-cli/internal/api"
)

// This file is the CLI side of iris pipeline promote (specification sections 1, 5,
// and 8): the command that marks a pipeline's data permanent, gated on built.
// promote is a control mutation POSTed to the leader-gated /pipeline/promote route;
// a successful promote reports the flipped data mode and exits 0, printing any
// repeated cross-mode read warning to stderr (or carrying it in the --json data
// envelope) -- a warning accompanies the success, it never blocks it. A refused
// promote (un-built or unregistered pipeline) is operation-failed (exit 4) carrying
// the daemon's reason; transport failure is no-daemon (exit 3) with start guidance;
// a not_leader rejection is exit 6 with leader guidance.

// pipelinePromote is the handler for `iris pipeline promote <name>`: it POSTs the
// promote request to the daemon and renders the flipped data mode plus any repeated
// cross-mode read warnings.
func (a *app) pipelinePromote() runE {
	return func(cmd *cobra.Command, args []string) error {
		settings := a.resolveTarget(cmd)
		client, base, overTCP := a.daemonHTTPClient(settings)

		body, err := json.Marshal(api.PipelinePromoteRequest{Pipeline: args[0]})
		if err != nil {
			return &fault{code: exitOpFailed, codeStr: "encode", message: fmt.Sprintf("pipeline promote: encode request: %v", err)}
		}
		hreq, err := http.NewRequestWithContext(cmd.Context(), http.MethodPost, base+"/pipeline/promote", bytes.NewReader(body))
		if err != nil {
			return &fault{code: exitOpFailed, codeStr: "request", message: fmt.Sprintf("pipeline promote: build request: %v", err)}
		}
		hreq.Header.Set("Content-Type", "application/json")
		if overTCP && settings.Token != "" {
			hreq.Header.Set("Authorization", "Bearer "+settings.Token)
		}

		resp, err := client.Do(hreq)
		if err != nil {
			a.logger.Debug("no iris daemon reachable", "op", "pipeline promote", "socket", settings.Socket, "host", settings.Host, "err", err)
			return a.noDaemonFault()
		}
		defer func() {
			_, _ = io.Copy(io.Discard, resp.Body)
			_ = resp.Body.Close()
		}()
		return a.classifyPipelinePromote(cmd, resp)
	}
}

// classifyPipelinePromote maps a /pipeline/promote response to a command outcome.
// A 200 carries the flipped data mode and any repeated cross-mode read warnings:
// under --json the result (warnings included) is the data envelope; otherwise the
// warnings print to stderr and the flipped mode to stdout. A not_leader status is
// exit 6 naming the leader; every other status is operation-failed (exit 4) with
// the daemon's reason -- the built-gate refusal arrives on this path.
func (a *app) classifyPipelinePromote(cmd *cobra.Command, resp *http.Response) error {
	switch resp.StatusCode {
	case http.StatusOK:
		var env struct {
			Data api.PipelinePromoteResult `json:"data"`
		}
		if err := json.NewDecoder(resp.Body).Decode(&env); err != nil {
			return &fault{code: exitOpFailed, codeStr: "decode", message: fmt.Sprintf("pipeline promote: decode daemon response: %v", err)}
		}
		// The repeated warnings accompany the terminal outcome (cli.go doctrine):
		// stash them so a downstream render sees them too.
		a.warnings = env.Data.Warnings
		if jsonMode, _ := cmd.Flags().GetBool("json"); jsonMode {
			return json.NewEncoder(a.out).Encode(dataEnvelope{Data: env.Data})
		}
		for _, w := range env.Data.Warnings {
			fmt.Fprintf(a.errOut, "iris: warning: %s\n", w.Message)
		}
		fmt.Fprintf(a.out, "%s: data mode %s\n", env.Data.Pipeline, env.Data.DataMode)
		return nil
	case api.StatusNotLeader:
		return a.notLeaderFault(resp)
	default:
		return a.controlFailure(resp, "pipeline promote")
	}
}
