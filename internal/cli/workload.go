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

// This file is the CLI side of `iris workload wipe [<pipeline>]`. wipe is a
// control mutation POSTed to the leader-gated
// /workload/wipe route. It is a dev-loop op (y/N or --yes/--force); --yes honors
// soft-blocks, --force overrides. A successful wipe exits 0 (counts may be
// emitted under --json); refusals are operation-failed (4) or other categories.

// workloadWipe is the handler for `iris workload wipe [pipeline]`: it is a gated
// dev-loop destructive operation. It first enforces the confirmation surface
// (--yes/--force or interactive y/N via the confirm
// seam) and only then POSTs the request to the leader-gated /workload/wipe route,
// mapping the outcome.
func (a *app) workloadWipe() runE {
	return func(cmd *cobra.Command, args []string) error {
		var pipeline string
		if len(args) == 1 {
			pipeline = args[0]
		}
		name := pipeline
		if name == "" {
			name = "the engine"
		}
		confirmed, err := a.confirmOrFlags(cmd, name, false)
		if err != nil {
			return err
		}
		yes, _ := cmd.Flags().GetBool("yes")
		force, _ := cmd.Flags().GetBool("force")
		if !confirmed && !yes && !force {
			return &fault{
				code:    exitOpFailed,
				codeStr: "confirmation_required",
				message: "workload wipe is destructive; re-run with --yes or --force, or confirm interactively",
			}
		}
		settings := a.resolveTarget(cmd)
		client, base, overTCP := a.daemonHTTPClient(settings)

		body, err := json.Marshal(api.WorkloadWipeRequest{Pipeline: pipeline, Confirm: confirmed || yes || force, Force: force})
		if err != nil {
			return &fault{code: exitOpFailed, codeStr: "encode", message: fmt.Sprintf("workload wipe: encode request: %v", err)}
		}
		hreq, err := http.NewRequestWithContext(cmd.Context(), http.MethodPost, base+"/workload/wipe", bytes.NewReader(body))
		if err != nil {
			return &fault{code: exitOpFailed, codeStr: "request", message: fmt.Sprintf("workload wipe: build request: %v", err)}
		}
		hreq.Header.Set("Content-Type", "application/json")
		if overTCP && settings.Token != "" {
			hreq.Header.Set("Authorization", "Bearer "+settings.Token)
		}

		resp, err := client.Do(hreq)
		if err != nil {
			a.logger.Debug("no iris daemon reachable", "op", "workload wipe", "socket", settings.Socket, "host", settings.Host, "err", err)
			return a.noDaemonFault()
		}
		defer func() {
			_, _ = io.Copy(io.Discard, resp.Body)
			_ = resp.Body.Close()
		}()
		return a.classifyWorkloadWipe(cmd, resp)
	}
}

// classifyWorkloadWipe maps the response. 200 is success (emit counts under json
// or human line); not_leader -> exit 6; else op-failed (4).
func (a *app) classifyWorkloadWipe(cmd *cobra.Command, resp *http.Response) error {
	switch resp.StatusCode {
	case http.StatusOK:
		var env struct {
			Data api.WorkloadWipeResult `json:"data"`
		}
		if err := json.NewDecoder(resp.Body).Decode(&env); err != nil {
			return &fault{code: exitOpFailed, codeStr: "decode", message: fmt.Sprintf("workload wipe: decode daemon response: %v", err)}
		}
		if jsonMode, _ := cmd.Flags().GetBool("json"); jsonMode {
			return json.NewEncoder(a.out).Encode(dataEnvelope{Data: env.Data})
		}
		fmt.Fprintf(a.out, "wiped %d, skipped %d\n", env.Data.Wiped, env.Data.Skipped)
		return nil
	case api.StatusNotLeader:
		return a.notLeaderFault(resp)
	default:
		return a.controlFailure(resp, "workload wipe")
	}
}
