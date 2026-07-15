package cli

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"runtime"
	"strings"
	"syscall"
	"time"

	"github.com/spf13/cobra"
	"github.com/spf13/pflag"

	"github.com/MateusAMP2119/iris-lakehouse/internal/api"
	"github.com/MateusAMP2119/iris-lakehouse/internal/config"
	"github.com/MateusAMP2119/iris-lakehouse/internal/daemon"
)

// The daemon-lifecycle timeouts bound `engine start --detach` (waiting for the
// detached daemon to become reachable) and `engine stop` (waiting for the
// signalled daemon to exit). They bound the operation, never readiness itself:
// readiness is a reachable socket and liveness a signalled process, polled within
// these windows.
const (
	detachReadyTimeout = 30 * time.Second
	stopGraceTimeout   = 10 * time.Second
)

// engineVersion is the engine's own version string reported by `iris engine info`.
// It is a placeholder until the release-stamping build wiring lands; `info` also
// reports the Go runtime version, which is real.
const engineVersion = "0.0.0-dev"

// installResult is the machine-readable outcome of `iris engine install`, the
// payload of its --json data envelope. It names the mode and, in managed mode, the
// directory the managed Postgres was placed under -- never any credential.
type installResult struct {
	Mode  string `json:"mode"`
	PgDir string `json:"pg_dir,omitempty"`
}

// engineInstall is the handler for `iris engine install`: a daemonless lifecycle
// command (it dials no daemon) that performs the engine bootstrap. Through the one
// Manager code path it brings up Postgres for the configured mode -- downloading and
// placing the pinned, checksum-verified managed Postgres under <engine home>/pg
// and starting it, or resolving the external admin DSN -- then creates meta alongside
// the data database, ensures the control tables and the partitioned journal, and sets
// up the control socket. In managed mode the local instance is stopped again once the
// bootstrap completes. It fails fast (operation failed, exit 4) on any error. (The
// engine-key leg is deferred: see daemon.InstallEngine.)
func (a *app) engineInstall() runE {
	return func(cmd *cobra.Command, _ []string) error {
		settings := a.resolveTarget(cmd)
		if err := a.refuseLegacyWorkspaceState(settings); err != nil {
			return err
		}
		ctx := cmd.Context()
		if ctx == nil {
			ctx = context.Background()
		}

		if _, err := daemon.InstallEngine(ctx, settings, a.logger); err != nil {
			a.logger.Error("engine install failed", "err", err)
			return &fault{
				code:    exitOpFailed,
				codeStr: "install_failed",
				message: fmt.Sprintf("engine install failed: %v", err),
			}
		}
		return a.emitInstallResult(cmd, settings)
	}
}

// engineInfoResult is the machine-readable payload of `iris engine info`, the
// --json data envelope: the local configuration (engine and Go version, control
// socket, Postgres mode, object store path) merged with the engine key's PUBLIC
// half and, when a daemon is reachable, its runtime readout from GET /info -- the
// leadership role (naming the leader when known), the TCP listener, the data and
// meta targets, the per-lane pass counts, and uptime, the engine's sole,
// display-only wall-clock. It never carries private key material, a DSN, or a
// credential.
type engineInfoResult struct {
	Version         string           `json:"version"`
	Go              string           `json:"go"`
	Socket          string           `json:"socket"`
	TCP             string           `json:"tcp,omitempty"`
	Mode            string           `json:"mode"`
	ObjectsPath     string           `json:"objects_path"`
	EngineKeyPublic string           `json:"engine_key_public"`
	Role            string           `json:"role,omitempty"`
	Leader          string           `json:"leader,omitempty"`
	DataTarget      string           `json:"data_target,omitempty"`
	MetaTarget      string           `json:"meta_target,omitempty"`
	LanePasses      []api.LanePasses `json:"lane_passes,omitempty"`
	Uptime          string           `json:"uptime,omitempty"`
}

// engineInfo is the handler for `iris engine info`: the engine readout. It reads
// the engine key through an EngineKeyReader seam, derives the public half, and
// shows only that -- the private half stays in meta and never reaches an output
// stream. It then merges the daemon-held runtime readout (GET /info: role,
// listeners, targets, lane passes, uptime) best-effort: info remains a real local
// readout when no daemon is running (the daemonless install/key surface), and
// reports the full field set against a live daemon. When the key cannot be read
// the engine is not installed or unreachable, reported as operation-failed (exit
// 4) with a clear message.
func (a *app) engineInfo() runE {
	return func(cmd *cobra.Command, _ []string) error {
		settings := a.resolveTarget(cmd)
		ctx := cmd.Context()
		if ctx == nil {
			ctx = context.Background()
		}

		mk := a.newKeyReader
		if mk == nil {
			mk = daemon.NewEngineKeyReader
		}
		key, err := mk(settings).ReadEngineKey(ctx)
		if err != nil {
			a.logger.Debug("engine info: engine key unavailable", "err", err)
			return &fault{
				code:    exitOpFailed,
				codeStr: "engine_unavailable",
				message: `iris engine is not installed or not reachable; run "iris engine install" or target a running engine`,
			}
		}

		res := engineInfoResult{
			Version:         engineVersion,
			Go:              runtime.Version(),
			Socket:          settings.Socket,
			Mode:            modeName(settings),
			ObjectsPath:     settings.ObjectsPath,
			EngineKeyPublic: key.PublicBase64(),
		}
		runtimeInfo, daemonUp := a.fetchDaemonInfo(cmd.Context(), settings)
		if daemonUp {
			res.Role = runtimeInfo.Role
			res.Leader = runtimeInfo.Leader
			res.TCP = runtimeInfo.TCP
			res.DataTarget = runtimeInfo.DataTarget
			res.MetaTarget = runtimeInfo.MetaTarget
			res.LanePasses = runtimeInfo.LanePasses
			res.Uptime = runtimeInfo.Uptime
		}

		if jsonMode, _ := cmd.Flags().GetBool("json"); jsonMode {
			return json.NewEncoder(a.out).Encode(dataEnvelope{Data: res})
		}
		fmt.Fprintf(a.out, "engine version:    %s\n", res.Version)
		fmt.Fprintf(a.out, "go version:        %s\n", res.Go)
		fmt.Fprintf(a.out, "control socket:    %s\n", res.Socket)
		fmt.Fprintf(a.out, "postgres mode:     %s\n", res.Mode)
		fmt.Fprintf(a.out, "object store:      %s\n", res.ObjectsPath)
		fmt.Fprintf(a.out, "engine key (pub):  %s\n", res.EngineKeyPublic)
		if !daemonUp {
			fmt.Fprintln(a.out, `daemon:            not reachable (run "iris engine start")`)
			return nil
		}
		role := res.Role
		if res.Leader != "" {
			role += " (leader: " + res.Leader + ")"
		}
		fmt.Fprintf(a.out, "role:              %s\n", role)
		if res.TCP != "" {
			fmt.Fprintf(a.out, "tcp listener:      %s\n", res.TCP)
		}
		fmt.Fprintf(a.out, "data target:       %s\n", res.DataTarget)
		fmt.Fprintf(a.out, "meta target:       %s\n", res.MetaTarget)
		fmt.Fprintf(a.out, "lane passes:       %s\n", lanePassList(res.LanePasses))
		fmt.Fprintf(a.out, "uptime:            %s\n", res.Uptime)
		return nil
	}
}

// fetchDaemonInfo reads the daemon's runtime info readout (GET /info)
// best-effort: false when no daemon is reachable or the read fails, so `iris
// engine info` stays a real local readout on a daemonless engine.
func (a *app) fetchDaemonInfo(ctx context.Context, settings config.Settings) (api.InfoPayload, bool) {
	client, base, overTCP := a.daemonHTTPClient(settings)
	hreq, err := http.NewRequestWithContext(ctx, http.MethodGet, base+"/info", nil)
	if err != nil {
		a.logger.Debug("engine info: build daemon request", "err", err)
		return api.InfoPayload{}, false
	}
	if overTCP && settings.Token != "" {
		hreq.Header.Set("Authorization", "Bearer "+settings.Token)
	}
	resp, err := client.Do(hreq)
	if err != nil {
		a.logger.Debug("engine info: no iris daemon reachable", "socket", settings.Socket, "host", settings.Host, "err", err)
		return api.InfoPayload{}, false
	}
	defer func() {
		_, _ = io.Copy(io.Discard, resp.Body)
		_ = resp.Body.Close()
	}()
	if resp.StatusCode != http.StatusOK {
		a.logger.Debug("engine info: daemon /info failed", "status", resp.StatusCode)
		return api.InfoPayload{}, false
	}
	var env struct {
		Data api.InfoPayload `json:"data"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&env); err != nil {
		a.logger.Debug("engine info: decode daemon response", "err", err)
		return api.InfoPayload{}, false
	}
	return env.Data, true
}

// lanePassList renders the per-lane pass counts as "lane count" pairs in lane
// order, or "none" while no lane has completed a pass.
func lanePassList(passes []api.LanePasses) string {
	if len(passes) == 0 {
		return "none"
	}
	parts := make([]string, 0, len(passes))
	for _, lp := range passes {
		parts = append(parts, fmt.Sprintf("%s %d", lp.Lane, lp.Passes))
	}
	return strings.Join(parts, ", ")
}

// uninstallResult is the machine-readable payload of `iris engine uninstall`, the
// --json data envelope: the on-disk paths the teardown removed. It carries no
// secret.
type uninstallResult struct {
	Removed []string `json:"removed"`
}

// engineUninstall is the handler for `iris engine uninstall`: the gated, daemonless,
// local-machine-only engine teardown. It refuses without --yes (operation failed,
// exit 4, with guidance), and otherwise removes the engine's on-disk state -- the
// object store under objects_path (artifact bytes and archived partitions), the
// control socket, and the service unit. The meta and journal drops are orchestrated
// by daemon.UninstallEngine and run against the cluster once the daemon's live admin
// connection is wired; the on-disk teardown is real from now.
func (a *app) engineUninstall() runE {
	return func(cmd *cobra.Command, _ []string) error {
		// Confirmation gate for teardown: typed name ("engine") or --yes/--force.
		confirmed, cerr := a.confirmOrFlags(cmd, "engine", true)
		yes, _ := cmd.Flags().GetBool("yes")
		force, _ := cmd.Flags().GetBool("force")
		if !confirmed && !yes && !force {
			if cerr != nil {
				return cerr
			}
			return &fault{
				code:    exitOpFailed,
				codeStr: "confirmation_required",
				message: `iris engine uninstall is an irreversible teardown; re-run with --yes or --force, or type the target name to confirm (it drops meta, the journal, the object store, the socket, and the service unit)`,
			}
		}

		// Print what will be removed for teardowns (typed-name confirm path) on human output only.
		if jsonMode, _ := cmd.Flags().GetBool("json"); !jsonMode {
			fmt.Fprintln(a.out, "engine uninstall: will remove engine state (meta, journal, object store, socket, service unit)")
		} else {
			fmt.Fprintln(a.errOut, "engine uninstall: will remove engine state (meta, journal, object store, socket, service unit)")
		}

		settings := a.resolveTarget(cmd)
		ctx := cmd.Context()
		if ctx == nil {
			ctx = context.Background()
		}

		// Refuse while a daemon candidate is live (engine state is never torn down
		// out from under a running daemon). Two probes back the guard: the daemon
		// probe (GET /healthz over the resolved socket/TCP target) catches any
		// serving daemon, and the pidfile check catches a detached daemon whose
		// listener is wedged or still starting. A stale pidfile (process gone)
		// never blocks.
		held := a.probeDaemon(ctx, settings) == nil
		if !held {
			held, _ = daemon.PIDFileLiveCheck(settings).LiveCandidateHoldsMeta(ctx)
		}
		if held {
			return &fault{
				code:    exitOpFailed,
				codeStr: "uninstall_refused",
				message: daemon.ErrLiveCandidate.Error() + `; stop the engine first with "iris engine stop"`,
			}
		}

		removed, err := daemon.RemoveEngineArtifacts(settings)
		if err != nil {
			a.logger.Error("engine uninstall failed", "err", err)
			return &fault{
				code:    exitOpFailed,
				codeStr: "uninstall_failed",
				message: fmt.Sprintf("engine uninstall failed: %v", err),
			}
		}
		a.logger.Info("engine uninstall: removed on-disk engine state", "count", len(removed))

		res := uninstallResult{Removed: removed}
		if jsonMode, _ := cmd.Flags().GetBool("json"); jsonMode {
			return json.NewEncoder(a.out).Encode(dataEnvelope{Data: res})
		}
		if len(removed) == 0 {
			fmt.Fprintln(a.out, "engine uninstall: no on-disk engine state to remove")
			return nil
		}
		fmt.Fprintln(a.out, "engine uninstall: removed engine state")
		for _, path := range removed {
			fmt.Fprintf(a.out, "  %s\n", path)
		}
		return nil
	}
}

// startResult is the machine-readable outcome of a detached `iris engine start`:
// the daemon is running in the background and reachable on the socket. It carries
// no credential.
type startResult struct {
	Status string `json:"status"`
	Socket string `json:"socket"`
	PID    int    `json:"pid,omitempty"`
}

// engineStart is the handler for `iris engine start`: a daemonless lifecycle
// command that runs an engine candidate. By default it runs the daemon attached
// in the foreground, streaming logs to stderr and blocking until
// SIGTERM/SIGINT; with -d it detaches, re-execing itself as a background daemon
// and returning once the socket is reachable so the daemon survives the CLI's
// exit. In managed mode with no installed Postgres it fails fast with install
// guidance (exit 4); otherwise the candidate it runs (daemon.Run) brings Postgres
// up itself -- the managed subprocess, or the external cluster's DSN -- and
// connects meta before it serves.
func (a *app) engineStart() runE {
	return func(cmd *cobra.Command, _ []string) error {
		settings := a.resolveTarget(cmd)
		if err := a.refuseLegacyWorkspaceState(settings); err != nil {
			return err
		}
		detach, _ := cmd.Flags().GetBool("detach")
		daemonized := os.Getenv(daemon.DaemonizedEnv) == "1"

		if settings.Managed() && !daemon.IsManagedInstalled(settings) {
			return &fault{
				code:    exitOpFailed,
				codeStr: "engine_not_installed",
				message: `the engine's managed Postgres is not installed; run "iris engine install" first`,
			}
		}
		if detach && !daemonized {
			return a.startDetached(cmd, settings)
		}
		return a.startForeground(cmd, settings, daemonized)
	}
}

// startForeground runs the daemon attached in the current process, cancelling on
// SIGTERM/SIGINT so a graceful shutdown follows. It blocks for the daemon's
// lifetime; a clean signalled shutdown returns exit 0. When daemonized (the
// detached re-exec of `engine start -d`), the daemon's logs are structured JSON
// routed through the size-rotated daemon.log; attached in the foreground they stay
// human-readable on the CLI's stderr console.
func (a *app) startForeground(cmd *cobra.Command, settings config.Settings, daemonized bool) error {
	base := cmd.Context()
	if base == nil {
		base = context.Background()
	}
	ctx, stop := signal.NotifyContext(base, os.Interrupt, syscall.SIGTERM)
	defer stop()

	logger := a.logger
	if daemonized {
		l, closer, err := daemon.OpenDaemonLogger(settings)
		if err != nil {
			a.logger.Error("engine start failed", "err", err)
			return &fault{
				code:    exitOpFailed,
				codeStr: "engine_start_failed",
				message: fmt.Sprintf("engine start failed: %v", err),
			}
		}
		defer func() { _ = closer.Close() }()
		logger = l
	}

	logger.Info("iris engine starting", "socket", settings.Socket, "mode", modeName(settings))
	if err := daemon.Run(ctx, settings, logger); err != nil {
		logger.Error("engine start failed", "err", err)
		return &fault{
			code:    exitOpFailed,
			codeStr: "engine_start_failed",
			message: fmt.Sprintf("engine start failed: %v", err),
		}
	}
	return nil
}

// startDetached backgrounds the daemon by re-execing this binary as a session
// leader with -d stripped and a marker in the environment, its output redirected
// to the daemon log, and returns once the daemon's socket is reachable.
func (a *app) startDetached(cmd *cobra.Command, settings config.Settings) error {
	exe, err := os.Executable()
	if err != nil {
		return &fault{
			code:    exitOpFailed,
			codeStr: "engine_start_failed",
			message: fmt.Sprintf("engine start (detach) failed: cannot locate the iris binary: %v", err),
		}
	}
	base := cmd.Context()
	if base == nil {
		base = context.Background()
	}
	ctx, cancel := context.WithTimeout(base, detachReadyTimeout)
	defer cancel()

	if err := daemon.Detach(ctx, settings, exe, detachChildArgs(cmd)); err != nil {
		a.logger.Error("engine start (detach) failed", "err", err)
		return &fault{
			code:    exitOpFailed,
			codeStr: "engine_start_failed",
			message: fmt.Sprintf("engine start (detach) failed: %v", err),
		}
	}
	pid, _ := daemon.ReadPIDFile(settings) // best-effort: the daemon is up, pid is informational
	res := startResult{Status: "detached", Socket: settings.Socket, PID: pid}
	if jsonMode, _ := cmd.Flags().GetBool("json"); jsonMode {
		return json.NewEncoder(a.out).Encode(dataEnvelope{Data: res})
	}
	fmt.Fprintf(a.out, "iris engine detached; daemon listening on %s\n", settings.Socket)
	return nil
}

// detachChildArgs rebuilds the detached child's argv from the executed cobra
// command rather than from os.Args: the fixed `engine start` path plus every
// flag the invocation explicitly set (global and daemon-scoped alike -- cobra
// has merged the inherited persistent flags into the command's flag set by
// execution time), except detach itself, so the re-exec'd child runs in the
// foreground of its new session. Deriving the argv from the command rather than
// the process means an in-process re-entrant invocation of `engine start -d`
// can never re-exec its calling verb as the daemon child.
func detachChildArgs(cmd *cobra.Command) []string {
	args := []string{"engine", "start"}
	cmd.Flags().Visit(func(f *pflag.Flag) {
		if f.Name == "detach" {
			return
		}
		args = append(args, fmt.Sprintf("--%s=%s", f.Name, f.Value.String()))
	})
	return args
}

// stopResult is the machine-readable outcome of `iris engine stop`.
type stopResult struct {
	Status string `json:"status"`
	PID    int    `json:"pid"`
}

// engineStop is the handler for `iris engine stop`: it stops a detached daemon by
// the pid it recorded, signalling SIGTERM and waiting for it to exit. With no
// recorded daemon there is nothing to stop, so it reports no-daemon (exit 3) with
// start guidance. The stop is graceful: SIGTERM lands on the daemon's signal
// context, which drains the listeners, releases the leader lock and tears the
// managed Postgres down; daemon.StopDaemon waits out the grace window, escalating
// to SIGKILL only if the daemon overruns it, and reaps the pidfile either way.
func (a *app) engineStop() runE {
	return func(cmd *cobra.Command, _ []string) error {
		settings := a.resolveTarget(cmd)
		pid, err := daemon.ReadPIDFile(settings)
		if err != nil {
			a.logger.Debug("no detached iris daemon to stop", "err", err)
			return a.noDaemon(cmd, "engine stop")
		}
		base := cmd.Context()
		if base == nil {
			base = context.Background()
		}
		ctx, cancel := context.WithTimeout(base, stopGraceTimeout)
		defer cancel()

		if err := daemon.StopDaemon(ctx, settings, pid); err != nil {
			a.logger.Error("engine stop failed", "err", err)
			return &fault{
				code:    exitOpFailed,
				codeStr: "stop_failed",
				message: fmt.Sprintf("engine stop failed: %v", err),
			}
		}
		a.logger.Info("engine stopped", "pid", pid)
		res := stopResult{Status: "stopped", PID: pid}
		if jsonMode, _ := cmd.Flags().GetBool("json"); jsonMode {
			return json.NewEncoder(a.out).Encode(dataEnvelope{Data: res})
		}
		fmt.Fprintf(a.out, "iris engine stopped (pid %d)\n", pid)
		return nil
	}
}

// serviceResult is the machine-readable outcome of `iris engine service
// install`/`uninstall`: the action taken and the unit path it acted on. It carries
// no secret.
type serviceResult struct {
	Status string `json:"status"`
	Unit   string `json:"unit"`
}

// engineServiceInstall is the handler for `iris engine service install`: a
// daemonless command that generates the host platform's service unit (systemd on
// linux, launchd on darwin) wrapping the detached daemon and writes it on demand
// (never auto-shipped). It writes to the workspace-local ServiceUnitPath seam by
// default, or to --path when given, and is the only command that installs a unit
// (no boot autostart is configured elsewhere). It fails fast (exit 4) on any
// generation or write error.
func (a *app) engineServiceInstall() runE {
	return func(cmd *cobra.Command, _ []string) error {
		settings := a.resolveTarget(cmd)
		exe, err := os.Executable()
		if err != nil {
			return &fault{
				code:    exitOpFailed,
				codeStr: "service_install_failed",
				message: fmt.Sprintf("engine service install failed: cannot locate the iris binary: %v", err),
			}
		}
		unitPath, _ := cmd.Flags().GetString("path")
		written, err := daemon.InstallServiceUnit(settings, exe, unitPath)
		if err != nil {
			a.logger.Error("engine service install failed", "err", err)
			return &fault{
				code:    exitOpFailed,
				codeStr: "service_install_failed",
				message: fmt.Sprintf("engine service install failed: %v", err),
			}
		}
		a.logger.Info("engine service unit installed", "unit", written)
		res := serviceResult{Status: "installed", Unit: written}
		if jsonMode, _ := cmd.Flags().GetBool("json"); jsonMode {
			return json.NewEncoder(a.out).Encode(dataEnvelope{Data: res})
		}
		fmt.Fprintf(a.out, "iris engine service unit installed at %s\n", written)
		return nil
	}
}

// engineServiceUninstall is the handler for `iris engine service uninstall`: a
// daemonless command that removes the generated service unit at the workspace-local
// ServiceUnitPath seam (or --path when given). Removing an absent unit is not an
// error (idempotent). It fails fast (exit 4) on a hard removal error.
func (a *app) engineServiceUninstall() runE {
	return func(cmd *cobra.Command, _ []string) error {
		settings := a.resolveTarget(cmd)
		unitPath, _ := cmd.Flags().GetString("path")
		if unitPath == "" {
			unitPath = daemon.ServiceUnitPath(settings)
		}
		removed, err := daemon.UninstallServiceUnit(unitPath)
		if err != nil {
			a.logger.Error("engine service uninstall failed", "err", err)
			return &fault{
				code:    exitOpFailed,
				codeStr: "service_uninstall_failed",
				message: fmt.Sprintf("engine service uninstall failed: %v", err),
			}
		}
		res := serviceResult{Status: "removed", Unit: unitPath}
		if !removed {
			res.Status = "absent"
		}
		if jsonMode, _ := cmd.Flags().GetBool("json"); jsonMode {
			return json.NewEncoder(a.out).Encode(dataEnvelope{Data: res})
		}
		if removed {
			fmt.Fprintf(a.out, "iris engine service unit removed from %s\n", unitPath)
		} else {
			fmt.Fprintln(a.out, "iris engine service unit: nothing to remove")
		}
		return nil
	}
}

// refuseLegacyWorkspaceState fails `iris engine install`/`start` fast when the
// invoking directory holds pre-engine-home state: earlier releases resolved the
// engine target from the cwd and placed the socket, config, and managed
// Postgres under <cwd>/.iris, so silently provisioning a second engine at the
// fixed engine home would strand that state (and its data). The check looks for
// the engine-owned leaves under <cwd>/.iris (iris.toml, iris.sock, iris.pid,
// the managed-Postgres pg/ directory) and skips itself when that directory IS
// the resolved engine directory (the cwd is the user's home, or IRIS_HOME
// points there). Pre-1.0 this is a documented breaking change: the operator
// moves or removes the legacy state once.
func (a *app) refuseLegacyWorkspaceState(settings config.Settings) error {
	wd, err := os.Getwd()
	if err != nil {
		return nil // no invoking directory to inspect; nothing to adopt
	}
	legacy := filepath.Join(wd, config.DirName)
	if engineDir, aerr := filepath.Abs(filepath.Dir(settings.Socket)); aerr == nil {
		if abs, lerr := filepath.Abs(legacy); lerr == nil && abs == engineDir {
			return nil
		}
	}
	// The engine-owned leaves earlier releases placed under <workspace>/.iris; a
	// bare or unrelated .iris directory does not trip the guard.
	for _, marker := range []string{config.FileName, config.SocketName, "iris.pid", "pg"} {
		if _, serr := os.Stat(filepath.Join(legacy, marker)); serr == nil {
			return &fault{
				code:    exitOpFailed,
				codeStr: "legacy_workspace_state",
				message: fmt.Sprintf("found engine state from an older iris at %s; the engine now lives at the fixed per-user engine home (%s), not the invoking directory — stop any old daemon, then move the state (mv %s %s) or remove it and reinstall",
					legacy, filepath.Dir(settings.Socket), legacy, filepath.Dir(settings.Socket)),
			}
		}
	}
	return nil
}

// modeName names the Postgres mode for `iris engine info`: managed when no admin
// DSN is configured, external otherwise. It never renders the DSN.
func modeName(s config.Settings) string {
	if s.Managed() {
		return "managed"
	}
	return "external"
}

// emitInstallResult renders a successful install: a single JSON data envelope under
// --json, otherwise a human line on stdout. Neither carries the engine-minted
// superuser credential.
func (a *app) emitInstallResult(cmd *cobra.Command, s config.Settings) error {
	res := installResult{Mode: "external"}
	if s.Managed() {
		res.Mode = "managed"
		res.PgDir = daemon.ManagedPGDir(s)
	}

	if jsonMode, _ := cmd.Flags().GetBool("json"); jsonMode {
		return json.NewEncoder(a.out).Encode(dataEnvelope{Data: res})
	}
	if res.Mode == "managed" {
		fmt.Fprintf(a.out, "engine managed Postgres installed under %s; created meta and data databases\n", res.PgDir)
	} else {
		fmt.Fprintln(a.out, "external Postgres configured; created meta and data databases")
	}
	return nil
}
