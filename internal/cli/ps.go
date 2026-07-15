package cli

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"text/tabwriter"

	"github.com/spf13/cobra"

	"github.com/MateusAMP2119/iris-lakehouse/internal/api"
)

// This file is the CLI side of `iris ps [-a] [-q]`: the docker-ps-shaped
// process-status readout. The CLI GETs the daemon's /ps route and prints
// exactly the payload the route serves -- under --json, the same data envelope
// any HTTP consumer reads, so the two surfaces are one payload by construction.
// The readout works against any reachable engine, local socket or remote
// --host/--token alike: it reads nothing from the local disk, so a machine
// with no engine installed sees the same rows the engine's own operator does.
// Transport failure is no-daemon (exit 3) with start guidance, any other
// failure operation-failed (exit 4).

// psCmd builds `iris ps`: the top-level process-status verb. -a widens the run
// rows from queued+running to the whole history; -q prints run ids only.
func (a *app) psCmd() *cobra.Command {
	c := &cobra.Command{
		Use:   "ps",
		Short: "Show the engine's runs and its load on the host (CPU, memory)",
		Args:  cobra.NoArgs,
		RunE:  a.ps(),
	}
	c.Flags().BoolP("all", "a", false, "show all runs, not only queued and running ones")
	c.Flags().BoolP("quiet", "q", false, "print run ids only")
	return daemonTouching(c)
}

// ps is the handler for `iris ps`: it GETs the daemon's /ps readout and
// renders it.
func (a *app) ps() runE {
	return func(cmd *cobra.Command, _ []string) error {
		all, _ := cmd.Flags().GetBool("all")
		quiet, _ := cmd.Flags().GetBool("quiet")

		settings := a.resolveTarget(cmd)
		client, base, overTCP := a.daemonHTTPClient(settings)

		reqURL := base + "/ps"
		if all {
			reqURL += "?" + url.Values{"all": {"true"}}.Encode()
		}
		hreq, err := http.NewRequestWithContext(cmd.Context(), http.MethodGet, reqURL, nil)
		if err != nil {
			return &fault{code: exitOpFailed, codeStr: "request", message: fmt.Sprintf("ps: build request: %v", err)}
		}
		if overTCP && settings.Token != "" {
			hreq.Header.Set("Authorization", "Bearer "+settings.Token)
		}

		resp, err := client.Do(hreq)
		if err != nil {
			a.logger.Debug("no iris daemon reachable", "op", "ps", "socket", settings.Socket, "host", settings.Host, "err", err)
			return a.noDaemonFault()
		}
		defer func() {
			_, _ = io.Copy(io.Discard, resp.Body)
			_ = resp.Body.Close()
		}()

		if resp.StatusCode != http.StatusOK {
			return a.controlFailure(resp, "ps")
		}
		var env struct {
			Data api.PsPayload `json:"data"`
		}
		if err := json.NewDecoder(resp.Body).Decode(&env); err != nil {
			return &fault{code: exitOpFailed, codeStr: "decode", message: fmt.Sprintf("ps: decode daemon response: %v", err)}
		}
		return a.emitPs(cmd, env.Data, quiet)
	}
}

// emitPs renders the readout: under --json the single data envelope carrying
// exactly the route's payload (the parity surface), under -q the run ids one
// per line, otherwise the engine block and the run table on stdout.
func (a *app) emitPs(cmd *cobra.Command, payload api.PsPayload, quiet bool) error {
	if jsonMode, _ := cmd.Flags().GetBool("json"); jsonMode {
		return json.NewEncoder(a.out).Encode(dataEnvelope{Data: payload})
	}
	if quiet {
		for _, r := range payload.Runs {
			if _, err := fmt.Fprintln(a.out, r.ID); err != nil {
				return err
			}
		}
		return nil
	}

	e := payload.Engine
	role := e.Role
	if e.Leader != "" {
		role += " (leader " + e.Leader + ")"
	}
	tw := tabwriter.NewWriter(a.out, 2, 4, 2, ' ', 0)
	fmt.Fprintln(tw, "ENGINE\tROLE\tUPTIME\tPID\tCPU\tMEM\tQUEUED\tRUNNING")
	fmt.Fprintf(tw, "%s\t%s\t%s\t%d\t%s\t%s\t%d\t%d\n",
		e.Version, role, e.Uptime, e.PID, cpuText(e.Load), memText(e.Load), e.QueuedRuns, e.RunningRuns)
	fmt.Fprintln(tw)
	fmt.Fprintln(tw, "RUN\tPIPELINE\tLANE\tSTATE\tEXIT\tCPU\tMEM")
	for _, r := range payload.Runs {
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\t%s\t%s\n",
			r.ID, r.Pipeline, orDash(r.Lane), r.State, exitCodeCell(r.ExitCode), cpuText(r.Load), memText(r.Load))
	}
	return tw.Flush()
}

// cpuText renders a sampled CPU load, or "-" when the host was not probed.
func cpuText(l *api.PsLoad) string {
	if l == nil {
		return "-"
	}
	return fmt.Sprintf("%.1f%%", l.CPUPercent)
}

// memText renders a sampled resident memory load, or "-" when the host was not
// probed.
func memText(l *api.PsLoad) string {
	if l == nil {
		return "-"
	}
	return memBytes(l.RSSBytes)
}

// memBytes renders a byte count human-readably (KiB/MiB/GiB, one decimal).
func memBytes(b int64) string {
	switch {
	case b >= 1<<30:
		return fmt.Sprintf("%.1fGiB", float64(b)/(1<<30))
	case b >= 1<<20:
		return fmt.Sprintf("%.1fMiB", float64(b)/(1<<20))
	case b >= 1<<10:
		return fmt.Sprintf("%.1fKiB", float64(b)/(1<<10))
	default:
		return fmt.Sprintf("%dB", b)
	}
}

// exitCodeCell renders a run row's exit code, or "-" while the run carries none.
func exitCodeCell(code *int) string {
	if code == nil {
		return "-"
	}
	return fmt.Sprintf("%d", *code)
}

// exitCodeText renders a last exit code, or "none" while no run has exited.
func exitCodeText(code *int) string {
	if code == nil {
		return "none"
	}
	return fmt.Sprintf("%d", *code)
}

// orDash substitutes "-" for an empty table cell.
func orDash(s string) string {
	if s == "" {
		return "-"
	}
	return s
}
