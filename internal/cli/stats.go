package cli

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"sort"
	"strings"

	"github.com/spf13/cobra"

	"github.com/MateusAMP2119/iris-lakehouse/internal/api"
)

// This file is the CLI side of `iris engine stats`: a read-only rollup readout.
// The CLI GETs the daemon's /stats route and prints exactly the payload the
// route serves -- under --json, the same data envelope any HTTP consumer reads,
// so the two surfaces are one payload by construction (the parity contract).
// Everything shown is a current count or a last-value: no time-series, no
// clock-derived metric, and the checkpoint chain head rides the engine rollup
// (iris engine stats reports the head). It is a read, served on any role;
// transport failure is no-daemon (exit 3) with start guidance, any other
// failure operation-failed (exit 4).

// engineStats is the handler for `iris engine stats`: it GETs the daemon's
// /stats rollup and renders it.
func (a *app) engineStats() runE {
	return func(cmd *cobra.Command, _ []string) error {
		settings := a.resolveTarget(cmd)
		client, base, overTCP := a.daemonHTTPClient(settings)

		hreq, err := http.NewRequestWithContext(cmd.Context(), http.MethodGet, base+"/stats", nil)
		if err != nil {
			return &fault{code: exitOpFailed, codeStr: "request", message: fmt.Sprintf("engine stats: build request: %v", err)}
		}
		if overTCP && settings.Token != "" {
			hreq.Header.Set("Authorization", "Bearer "+settings.Token)
		}

		resp, err := client.Do(hreq)
		if err != nil {
			a.logger.Debug("no iris daemon reachable", "op", "engine stats", "socket", settings.Socket, "host", settings.Host, "err", err)
			return a.noDaemonFault()
		}
		defer func() {
			_, _ = io.Copy(io.Discard, resp.Body)
			_ = resp.Body.Close()
		}()

		if resp.StatusCode != http.StatusOK {
			return a.controlFailure(resp, "engine stats")
		}
		var env struct {
			Data api.StatsPayload `json:"data"`
		}
		if err := json.NewDecoder(resp.Body).Decode(&env); err != nil {
			return &fault{code: exitOpFailed, codeStr: "decode", message: fmt.Sprintf("engine stats: decode daemon response: %v", err)}
		}
		return a.emitStats(cmd, env.Data)
	}
}

// emitStats renders the rollup: under --json the single data envelope carrying
// exactly the route's payload (the parity surface), otherwise a human readout of
// the engine, lane, and pipeline rollups on stdout.
func (a *app) emitStats(cmd *cobra.Command, payload api.StatsPayload) error {
	if jsonMode, _ := cmd.Flags().GetBool("json"); jsonMode {
		return json.NewEncoder(a.out).Encode(dataEnvelope{Data: payload})
	}

	e := payload.Engine
	fmt.Fprintf(a.out, "dead letters:       %d%s\n", e.DeadLetterDepth, reasonBreakdown(e.DeadLettersByReason))
	fmt.Fprintf(a.out, "running runs:       %d\n", e.RunningRuns)
	fmt.Fprintf(a.out, "captured writes:    %d\n", e.CapturedWrites)
	fmt.Fprintf(a.out, "wipe-eligible rows: %d\n", e.WipeEligibleRows)
	fmt.Fprintf(a.out, "journal rows:       %d (hot %d)\n", e.JournalRows, e.HotRows)
	fmt.Fprintf(a.out, "partitions:         %d sealed, %d archived\n", e.SealedPartitions, e.ArchivedPartitions)
	if head := e.CheckpointChainHead; head != nil {
		fmt.Fprintf(a.out, "chain head:         seq %d %s (%s)\n", head.Seq, head.Digest, head.Location)
	} else {
		fmt.Fprintln(a.out, "chain head:         none (no partition sealed yet)")
	}

	if len(payload.Lanes) > 0 {
		fmt.Fprintln(a.out, "lanes:")
		for _, l := range payload.Lanes {
			fmt.Fprintf(a.out, "  %s\tpipelines %d\tqueued %d\trunning %d\tpasses %d\n",
				l.Lane, l.Pipelines, l.Queued, l.Running, l.Passes)
		}
	}
	if len(payload.Pipelines) > 0 {
		fmt.Fprintln(a.out, "pipelines:")
		for _, p := range payload.Pipelines {
			fmt.Fprintf(a.out, "  %s\tlatest %s\tlast run %s\texit %s\truns %s\n",
				p.Pipeline, orNone(p.LatestRunState), orNone(p.LastRunID),
				exitCodeText(p.LastExitCode), stateBreakdown(p.RunsByState))
		}
	}
	return nil
}

// reasonBreakdown renders the per-reason dead-letter counts as a stable,
// name-ordered parenthetical, or "" when the worklist is empty.
func reasonBreakdown(byReason map[string]int64) string {
	if len(byReason) == 0 {
		return ""
	}
	return " (" + countList(byReason) + ")"
}

// stateBreakdown renders the per-state run counts name-ordered, or "none".
func stateBreakdown(byState map[string]int64) string {
	if len(byState) == 0 {
		return "none"
	}
	return countList(byState)
}

// countList renders a count map as "key value" pairs joined by ", ", in key
// order so the readout is stable.
func countList(counts map[string]int64) string {
	keys := make([]string, 0, len(counts))
	for k := range counts {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	parts := make([]string, 0, len(keys))
	for _, k := range keys {
		parts = append(parts, fmt.Sprintf("%s %d", k, counts[k]))
	}
	return strings.Join(parts, ", ")
}

// orNone substitutes "none" for an empty last-value.
func orNone(s string) string {
	if s == "" {
		return "none"
	}
	return s
}

// exitCodeText renders a last exit code, or "none" while no run has exited.
func exitCodeText(code *int) string {
	if code == nil {
		return "none"
	}
	return fmt.Sprintf("%d", *code)
}
