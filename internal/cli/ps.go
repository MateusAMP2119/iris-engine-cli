package cli

import (
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/spf13/cobra"

	"github.com/MateusAMP2119/iris-lakehouse/internal/api"
	"github.com/MateusAMP2119/iris-lakehouse/internal/config"
)

// This file is the CLI side of `iris ps`: the process-status verb with exactly
// two output modes, resolved from the terminal by one rule. Stdout an
// interactive terminal and --json absent: a live full-screen view (top style,
// re-polled every second; see psview.go). Everything else -- a pipe, a
// redirect, a script, --json: the single data envelope GET /ps serves, printed
// once, immediate exit. There is no plain-text table: the live view is the
// human surface, the envelope is the machine surface, and the two stay one
// payload by construction. The readout works against any reachable engine,
// local socket or remote --host/--token alike. Transport failure is no-daemon
// (exit 3) with start guidance, any other failure operation-failed (4).

// psCmd builds `iris ps`: the top-level process-status verb. --all widens the
// JSON document's run rows to the whole history (the live view holds the
// history already; its `a` key toggles).
func (a *app) psCmd() *cobra.Command {
	c := &cobra.Command{
		Use:   "ps",
		Short: "Live view of the engine's runs and host load; JSON when piped or under --json",
		Long: `Show what the engine is doing and what it costs the host.

On an interactive terminal, iris ps opens a live full-screen view, refreshed
every second: lanes, their pipelines, each pipeline's runs, and each run's
live log tail. Keys: j/k or arrows move, enter descends, <- ascends, a toggles
the run history, / searches everything, f toggles the log follow, c cancels a
running run, q quits.

Piped, redirected, or under --json, iris ps prints the exact GET /ps data
envelope once and exits: the machine surface scripts and agents parse. --all
applies to that document only, widening the run rows to the whole history.`,
		Args: cobra.NoArgs,
		RunE: a.ps(),
	}
	c.Flags().Bool("all", false, "JSON mode only: widen the run rows to the whole history")
	return daemonTouching(c)
}

// ps is the handler for `iris ps`: it resolves the output mode from the
// terminal, fetches the /ps readout, and either prints the envelope once or
// hands the first snapshot to the live view.
func (a *app) ps() runE {
	return func(cmd *cobra.Command, _ []string) error {
		jsonMode, _ := cmd.Flags().GetBool("json")
		all, _ := cmd.Flags().GetBool("all")

		tty := a.isTTY
		if tty == nil {
			tty = a.stdoutIsTerminal
		}
		stdinTTY := a.stdinIsTTY
		if stdinTTY == nil {
			stdinTTY = stdinIsTerminal
		}
		// The issue's rule keys on stdout, but a view without a key-capable
		// stdin could never be quit; folding stdin in up front keeps the mode
		// (and the one fetch) decided before anything is written.
		live := tty() && stdinTTY() && !jsonMode

		if live && cmd.Flags().Changed("all") {
			return a.usage(`--all shapes the JSON document only; in the live view press "a", or pipe / pass --json for the envelope`)
		}

		settings := a.resolveTarget(cmd)
		client := a.newPsDaemonClient(settings)

		// One fetch either way: the JSON mode reads the route's default document
		// (?all=true under --all); the live view starts from the whole run
		// history plus the daemon's recorded load history (its rings open
		// pre-seeded, so the graphs never restart with the client) and filters
		// client-side.
		payload, err := client.fetchPs(cmd.Context(), all || live, live)

		// An unreachable engine at open revives the target's last known state
		// (when one is cached) instead of tearing down: the view opens
		// read-only under the unreachable banner and the poller retries until
		// the engine returns. A reached daemon REFUSING the read, and the
		// machine surface (--json, pipes), keep the fault path.
		var first psSnapshot
		stale := false
		if err != nil {
			var herr *psHTTPError
			if !live || errors.As(err, &herr) {
				return a.psFetchFault(settings, err)
			}
			snap, savedAt, ok := client.cache.load()
			if !ok {
				return a.psFetchFault(settings, err)
			}
			first, stale = snap, true
			first.staleAge = time.Since(savedAt)
		}

		if live {
			if !stale {
				first = psSnapshot{ps: payload}
				if pipes, perr := client.fetchPipelines(cmd.Context()); perr == nil {
					first.pipelines = pipes
				}
				client.cache.save(first) // the open itself is a fresh last known state
			}
			entered, lerr := a.livePs(cmd, client, first, psTarget(settings, client.overTCP))
			if entered {
				if lerr == nil {
					return nil
				}
				// A reached daemon refusing the poll keeps its own message
				// (exit 4); anything else is the engine gone mid-view (exit 3).
				var herr *psHTTPError
				if errors.As(lerr, &herr) {
					return &fault{code: exitOpFailed, codeStr: herr.code, message: "ps: " + herr.Error()}
				}
				return a.noDaemonFaultAt(settings)
			}
			// Raw mode refused despite an interactive stdin (rare): fall back
			// to the JSON emit -- never a hung or key-less view. Refetch the
			// default document so the envelope matches the route's default.
			if payload, err = client.fetchPs(cmd.Context(), false, false); err != nil {
				return a.psFetchFault(settings, err)
			}
		}

		return json.NewEncoder(a.out).Encode(dataEnvelope{Data: payload})
	}
}

// psFetchFault classifies a one-shot /ps read failure: a reached daemon's
// refusal keeps its own message (operation-failed), a transport failure is
// no-daemon with the docker-ps-shaped connect message naming the exact target.
func (a *app) psFetchFault(settings config.Settings, err error) error {
	var herr *psHTTPError
	if errors.As(err, &herr) {
		return &fault{code: exitOpFailed, codeStr: herr.code, message: "ps: " + herr.Error()}
	}
	a.logger.Debug("no iris daemon reachable", "op", "ps", "socket", settings.Socket, "host", settings.Host, "err", err)
	return a.noDaemonFaultAt(settings)
}

// engineAddr renders the resolved engine target the way docker names its
// daemon socket in connect errors: the remote host when one is set, else the
// unix socket path.
func engineAddr(s config.Settings) string {
	if s.Host != "" {
		return s.Host
	}
	return "unix://" + s.Socket
}

// noDaemonFaultAt is the docker-ps-shaped no-daemon fault naming the exact
// resolved target ("Cannot connect to the Docker daemon at
// unix:///var/run/docker.sock. Is the docker daemon running?" is the shape it
// mirrors). Verbs that never resolved a target keep the address-less
// noDaemonFault.
func (a *app) noDaemonFaultAt(s config.Settings) error {
	return &fault{
		code:    exitNoDaemon,
		codeStr: "no_daemon",
		message: fmt.Sprintf(`Cannot connect to the iris engine at %s. Is the engine running? Start it with "iris engine start"`, engineAddr(s)),
	}
}

// livePs resolves the live-view seam: the injected fake in tests, the real
// terminal view in production.
func (a *app) livePs(cmd *cobra.Command, c *psDaemonClient, first psSnapshot, target string) (bool, error) {
	if a.psLive != nil {
		return a.psLive(cmd, c, first, target)
	}
	return a.runPsLive(cmd, c, first, target)
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
