package cli

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"

	"github.com/spf13/cobra"

	"github.com/MateusAMP2119/iris-engine-cli/internal/api"
)

// This file is the CLI side of `iris workload show [<pipeline>]`
// (specification section 8 and S08/workload-show-wiring-panel): the wiring
// panel over the standing wiring. GET /workload (with optional ?pipeline= for
// zoom). Under --json emits the data envelope; otherwise a human panel.
// The panel never renders commit/lineage rails.

// workloadShow is the handler for `iris workload show [pipeline]`.
func (a *app) workloadShow() runE {
	return func(cmd *cobra.Command, args []string) error {
		var pipeline string
		if len(args) == 1 {
			pipeline = args[0]
		}
		settings := a.resolveTarget(cmd)
		client, base, overTCP := a.daemonHTTPClient(settings)

		path := base + "/workload"
		if pipeline != "" {
			path += "?pipeline=" + url.QueryEscape(pipeline)
		}
		hreq, err := http.NewRequestWithContext(cmd.Context(), http.MethodGet, path, nil)
		if err != nil {
			return &fault{code: exitOpFailed, codeStr: "request", message: fmt.Sprintf("workload show: build request: %v", err)}
		}
		if overTCP && settings.Token != "" {
			hreq.Header.Set("Authorization", "Bearer "+settings.Token)
		}

		resp, err := client.Do(hreq)
		if err != nil {
			a.logger.Debug("no iris daemon reachable", "op", "workload show", "socket", settings.Socket, "host", settings.Host, "err", err)
			return a.noDaemonFault()
		}
		defer func() {
			_, _ = io.Copy(io.Discard, resp.Body)
			_ = resp.Body.Close()
		}()

		if resp.StatusCode != http.StatusOK {
			return a.controlFailure(resp, "workload show")
		}
		var env struct {
			Data api.WorkloadShowResult `json:"data"`
		}
		if err := json.NewDecoder(resp.Body).Decode(&env); err != nil {
			return &fault{code: exitOpFailed, codeStr: "decode", message: fmt.Sprintf("workload show: decode daemon response: %v", err)}
		}
		return a.emitWorkloadShow(cmd, env.Data)
	}
}

// emitWorkloadShow renders the panel: --json the envelope, else human panel
// text on stdout. The human form lists lanes, their composer order, per-pipeline
// artifact/data mode, a run tip, and the gate ledger per edge.
func (a *app) emitWorkloadShow(cmd *cobra.Command, d api.WorkloadShowResult) error {
	if jsonMode, _ := cmd.Flags().GetBool("json"); jsonMode {
		return json.NewEncoder(a.out).Encode(dataEnvelope{Data: d})
	}

	if len(d.Lanes) == 0 {
		fmt.Fprintln(a.out, "(no registered pipelines)")
		return nil
	}
	for i, lane := range d.Lanes {
		if i > 0 {
			fmt.Fprintln(a.out)
		}
		fmt.Fprintf(a.out, "lane: %s\n", lane.Name)
		for _, p := range lane.Pipelines {
			tip := p.RunTip
			if tip == "" {
				tip = "none"
			}
			fmt.Fprintf(a.out, "  %s  %s/%s  tip:%s\n", p.Name, p.Artifact, p.DataMode, tip)
			if len(p.Gate) > 0 {
				fmt.Fprintln(a.out, "    gate:")
				for _, e := range p.Gate {
					latest := e.LatestRunID
					if latest == "" {
						latest = "none"
					}
					fmt.Fprintf(a.out, "      %s %s (latest %s)\n", e.Upstream, e.Verdict, latest)
				}
			}
		}
	}
	return nil
}
