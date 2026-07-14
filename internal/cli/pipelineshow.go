package cli

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"

	"github.com/spf13/cobra"

	"github.com/MateusAMP2119/iris-engine-cli/internal/api"
)

// This file is the CLI side of `iris pipeline show <name>`: the single-pipeline
// readout. The CLI GETs the daemon's /pipeline/show route and prints exactly the
// payload the route serves -- the
// resolved declaration, the role and its field-level grants, the recent runs, and
// the gate ledger with the per-edge verdict from the closed set -- under --json
// the same data envelope any HTTP consumer reads. It is a read, served on any
// role. Transport failure is no-daemon (exit 3) with start guidance; an
// unregistered pipeline or any other failure is operation-failed (exit 4).

// pipelineShow is the handler for `iris pipeline show <name>`: it GETs the
// daemon's /pipeline/show readout and renders it.
func (a *app) pipelineShow() runE {
	return func(cmd *cobra.Command, args []string) error {
		name := args[0]
		settings := a.resolveTarget(cmd)
		client, base, overTCP := a.daemonHTTPClient(settings)

		hreq, err := http.NewRequestWithContext(cmd.Context(), http.MethodGet,
			base+"/pipeline/show?name="+url.QueryEscape(name), nil)
		if err != nil {
			return &fault{code: exitOpFailed, codeStr: "request", message: fmt.Sprintf("pipeline show: build request: %v", err)}
		}
		if overTCP && settings.Token != "" {
			hreq.Header.Set("Authorization", "Bearer "+settings.Token)
		}

		resp, err := client.Do(hreq)
		if err != nil {
			a.logger.Debug("no iris daemon reachable", "op", "pipeline show", "socket", settings.Socket, "host", settings.Host, "err", err)
			return a.noDaemonFault()
		}
		defer func() {
			_, _ = io.Copy(io.Discard, resp.Body)
			_ = resp.Body.Close()
		}()

		if resp.StatusCode != http.StatusOK {
			return a.controlFailure(resp, "pipeline show")
		}
		var env struct {
			Data api.PipelineShowResult `json:"data"`
		}
		if err := json.NewDecoder(resp.Body).Decode(&env); err != nil {
			return &fault{code: exitOpFailed, codeStr: "decode", message: fmt.Sprintf("pipeline show: decode daemon response: %v", err)}
		}
		return a.emitPipelineShow(cmd, env.Data)
	}
}

// emitPipelineShow renders the readout: under --json the single data envelope
// carrying exactly the route's payload, otherwise a human readout of the
// declaration, role and grants, recent runs, and gate ledger on stdout.
func (a *app) emitPipelineShow(cmd *cobra.Command, d api.PipelineShowResult) error {
	if jsonMode, _ := cmd.Flags().GetBool("json"); jsonMode {
		return json.NewEncoder(a.out).Encode(dataEnvelope{Data: d})
	}

	fmt.Fprintf(a.out, "pipeline:   %s\n", d.Name)
	fmt.Fprintf(a.out, "folder:     %s\n", d.Folder)
	fmt.Fprintf(a.out, "run:        %s\n", strings.Join(d.Run, " "))
	fmt.Fprintf(a.out, "artifact:   %s\n", d.Artifact)
	fmt.Fprintf(a.out, "data mode:  %s\n", d.DataMode)
	if len(d.DependsOn) > 0 {
		fmt.Fprintf(a.out, "depends on: %s\n", strings.Join(d.DependsOn, ", "))
	}
	fmt.Fprintf(a.out, "role:       %s\n", d.Role)
	if len(d.Grants) > 0 {
		fmt.Fprintln(a.out, "grants:")
		for _, g := range d.Grants {
			fmt.Fprintf(a.out, "  %s %s.%s.%s\n", g.Access, g.Schema, g.Table, g.Field)
		}
	}
	if len(d.RecentRuns) > 0 {
		fmt.Fprintln(a.out, "recent runs:")
		for _, r := range d.RecentRuns {
			fmt.Fprintf(a.out, "  %s\t%s\texit %s\n", r.ID, r.State, exitCodeText(r.ExitCode))
		}
	}
	if len(d.GateLedger) > 0 {
		fmt.Fprintln(a.out, "gate ledger:")
		for _, e := range d.GateLedger {
			latest := e.LatestRunID
			if latest == "" {
				latest = "none"
			}
			fmt.Fprintf(a.out, "  %s\t%s\t(latest run %s)\n", e.Upstream, e.Verdict, latest)
		}
	}
	return nil
}
