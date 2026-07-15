package cli

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"

	"github.com/spf13/cobra"

	"github.com/MateusAMP2119/iris-lakehouse/internal/api"
)

// This file is the CLI side of iris pipeline build: the one explicit entry point
// that compiles anything -- declare apply never
// builds. build is a control mutation POSTed to the leader-gated /pipeline/build
// route; a successful build reports the new artifact's content hash (the identity
// the executed bytes are always recognizable by) and exits 0. A build failure
// (unsupported runtime, failing toolchain, unregistered pipeline) is
// operation-failed (exit 4) carrying the daemon's reason; transport failure is
// no-daemon (exit 3) with start guidance; a not_leader rejection is exit 6 with
// leader guidance.

// pipelineBuild is the handler for `iris pipeline build <name>`: it POSTs the build
// request to the daemon and renders the recorded artifact identity.
func (a *app) pipelineBuild() runE {
	return func(cmd *cobra.Command, args []string) error {
		settings := a.resolveTarget(cmd)
		client, base, overTCP := a.daemonHTTPClient(settings)

		body, err := json.Marshal(api.PipelineBuildRequest{Pipeline: args[0]})
		if err != nil {
			return &fault{code: exitOpFailed, codeStr: "encode", message: fmt.Sprintf("pipeline build: encode request: %v", err)}
		}
		hreq, err := http.NewRequestWithContext(cmd.Context(), http.MethodPost, base+"/pipeline/build", bytes.NewReader(body))
		if err != nil {
			return &fault{code: exitOpFailed, codeStr: "request", message: fmt.Sprintf("pipeline build: build request: %v", err)}
		}
		hreq.Header.Set("Content-Type", "application/json")
		if overTCP && settings.Token != "" {
			hreq.Header.Set("Authorization", "Bearer "+settings.Token)
		}

		resp, err := client.Do(hreq)
		if err != nil {
			a.logger.Debug("no iris daemon reachable", "op", "pipeline build", "socket", settings.Socket, "host", settings.Host, "err", err)
			return a.noDaemonFault()
		}
		defer func() {
			_, _ = io.Copy(io.Discard, resp.Body)
			_ = resp.Body.Close()
		}()
		return a.classifyPipelineBuild(cmd, resp)
	}
}

// classifyPipelineBuild maps a /pipeline/build response to a command outcome. A 200
// carries the recorded artifact identity; a not_leader status is exit 6 naming the
// leader; every other status is operation-failed (exit 4) with the daemon's reason.
func (a *app) classifyPipelineBuild(cmd *cobra.Command, resp *http.Response) error {
	switch resp.StatusCode {
	case http.StatusOK:
		var env struct {
			Data api.PipelineBuildResult `json:"data"`
		}
		if err := json.NewDecoder(resp.Body).Decode(&env); err != nil {
			return &fault{code: exitOpFailed, codeStr: "decode", message: fmt.Sprintf("pipeline build: decode daemon response: %v", err)}
		}
		if jsonMode, _ := cmd.Flags().GetBool("json"); jsonMode {
			return json.NewEncoder(a.out).Encode(dataEnvelope{Data: env.Data})
		}
		fmt.Fprintf(a.out, "%s: built %s (%d bytes)\n", env.Data.Pipeline, env.Data.Hash, env.Data.SizeBytes)
		return nil
	case api.StatusNotLeader:
		return a.notLeaderFault(resp)
	default:
		return a.controlFailure(resp, "pipeline build")
	}
}
