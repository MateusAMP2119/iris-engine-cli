package daemon

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"

	"github.com/MateusAMP2119/iris-lakehouse/internal/config"
)

// This file generates the platform service unit `iris engine service install`
// writes on demand (on demand, never auto-shipped: iris engine service install
// installs a systemd/launchd unit wrapping the detached daemon, itself
// service-ready (clean SIGTERM/SIGINT, no TTY, sane exit codes)). The unit wraps
// `<binary> engine start --detach` rooted at the unit-install invocation's
// working directory (the workspace tree the daemon dispatches from), so the
// service manager runs the same detached daemon the CLI does. It is generated only here and written
// only by the service-install command (a structural sweep proves no other command
// installs a unit or a boot autostart); `service uninstall` removes it, at the
// ServiceUnitPath seam engine uninstall shares.

// ServicePlatform identifies the init system a generated unit targets.
type ServicePlatform string

const (
	// ServiceSystemd is Linux's systemd (a .service unit).
	ServiceSystemd ServicePlatform = "systemd"
	// ServiceLaunchd is macOS's launchd (a .plist agent).
	ServiceLaunchd ServicePlatform = "launchd"
)

// serviceLaunchdLabel is the launchd job label the generated agent carries.
const serviceLaunchdLabel = "com.iris.engine"

// serviceUnitPerm is the mode a generated unit file is written with. Unit files
// carry no secret and must be readable by the service manager (which may run as a
// different user), so they follow the conventional world-readable 0644 rather than
// the owner-only mode the rest of the engine home uses.
const serviceUnitPerm os.FileMode = 0o644

// serviceUnitDirPerm is the mode intermediate directories are created with for a
// --path target outside the engine home: 0755, so a service manager running as a
// different user can traverse to the world-readable unit file. The engine home's
// own 0700 is for engine-private state and would block that traversal.
const serviceUnitDirPerm os.FileMode = 0o755

// systemdUnitTemplate renders a systemd service that wraps the detached daemon.
// Type=forking with PIDFile tracks the daemon that `engine start --detach`
// backgrounds; KillSignal=SIGTERM drives the daemon's graceful shutdown on
// `systemctl stop`. The [Install] section makes the unit enablable, but install
// never enables it (no boot autostart is configured by Iris).
const systemdUnitTemplate = `[Unit]
Description=Iris engine daemon
After=network.target

[Service]
Type=forking
WorkingDirectory=%[2]s
ExecStart=%[1]s engine start --detach
ExecStop=%[1]s engine stop
PIDFile=%[3]s
KillSignal=SIGTERM
Restart=no

[Install]
WantedBy=default.target
`

// launchdPlistTemplate renders a launchd agent that wraps the detached daemon.
// RunAtLoad and KeepAlive are both false, so launchd never starts the daemon at
// load or boot: it runs on demand only (no boot autostart).
const launchdPlistTemplate = `<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0">
<dict>
	<key>Label</key>
	<string>%[3]s</string>
	<key>ProgramArguments</key>
	<array>
		<string>%[1]s</string>
		<string>engine</string>
		<string>start</string>
		<string>--detach</string>
	</array>
	<key>WorkingDirectory</key>
	<string>%[2]s</string>
	<key>RunAtLoad</key>
	<false/>
	<key>KeepAlive</key>
	<false/>
</dict>
</plist>
`

// systemdAutostartUnitTemplate renders the opt-in autostart variant: the
// FOREGROUND daemon under systemd's own supervision (no forking wrapper --
// the service manager is the babysitter), restarted on failure, enablable at
// login. Autostart is never the default: only `engine service install
// --autostart` renders this.
const systemdAutostartUnitTemplate = `[Unit]
Description=Iris engine daemon
After=network.target

[Service]
Type=exec
WorkingDirectory=%[2]s
ExecStart=%[1]s engine start
KillSignal=SIGTERM
Restart=on-failure
RestartSec=2

[Install]
WantedBy=default.target
`

// launchdAutostartPlistTemplate renders the opt-in autostart variant: the
// FOREGROUND daemon under launchd's supervision, started at load and login
// (RunAtLoad), restarted only when it fails (KeepAlive/SuccessfulExit=false,
// so `iris engine stop` and `launchctl bootout` do not fight the manager),
// its output landing in the engine's own daemon log.
const launchdAutostartPlistTemplate = `<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0">
<dict>
	<key>Label</key>
	<string>%[4]s</string>
	<key>ProgramArguments</key>
	<array>
		<string>%[1]s</string>
		<string>engine</string>
		<string>start</string>
	</array>
	<key>WorkingDirectory</key>
	<string>%[2]s</string>
	<key>RunAtLoad</key>
	<true/>
	<key>KeepAlive</key>
	<dict>
		<key>SuccessfulExit</key>
		<false/>
	</dict>
	<key>StandardOutPath</key>
	<string>%[3]s</string>
	<key>StandardErrorPath</key>
	<string>%[3]s</string>
</dict>
</plist>
`

// ServicePlatformForGOOS maps a GOOS to its init system: linux to systemd, darwin
// to launchd. Other platforms are unsupported (reported by the false return),
// matching the daemon's unix-only surface.
func ServicePlatformForGOOS(goos string) (ServicePlatform, bool) {
	switch goos {
	case "linux":
		return ServiceSystemd, true
	case "darwin":
		return ServiceLaunchd, true
	default:
		return "", false
	}
}

// HostServicePlatform returns the init system of the host the binary runs on, and
// whether it is supported.
func HostServicePlatform() (ServicePlatform, bool) {
	return ServicePlatformForGOOS(runtime.GOOS)
}

// WorkspaceRoot returns the workspace tree the generated unit's daemon
// dispatches from, used as the service unit's WorkingDirectory: the resolved workspace setting, never the invoking directory (#203).
func WorkspaceRoot(s config.Settings) string {
	if s.Workspace != "" {
		return s.Workspace
	}
	return filepath.Dir(irisDir(s))
}

// RenderServiceUnit renders the service unit text for platform: a systemd unit or
// a launchd plist that wraps `<exePath> engine start --detach` with WorkingDirectory
// set to workspace, and (systemd) PIDFile set to pidPath. An unsupported platform
// is an error, never a silent empty unit.
func RenderServiceUnit(platform ServicePlatform, exePath, workspace, pidPath string) (string, error) {
	switch platform {
	case ServiceSystemd:
		return fmt.Sprintf(systemdUnitTemplate, exePath, workspace, pidPath), nil
	case ServiceLaunchd:
		return fmt.Sprintf(launchdPlistTemplate, exePath, workspace, serviceLaunchdLabel), nil
	default:
		return "", fmt.Errorf("daemon: no service unit for platform %q (systemd/launchd only)", platform)
	}
}

// RenderAutostartServiceUnit renders the opt-in autostart unit text for
// platform: the FOREGROUND daemon under the service manager's own supervision,
// started at login, restarted on failure -- the docker-parity always-on shape.
// logPath is where launchd lands the foreground daemon's output (systemd owns
// its journal). An unsupported platform is an error, never a silent empty
// unit.
func RenderAutostartServiceUnit(platform ServicePlatform, exePath, workspace, logPath string) (string, error) {
	switch platform {
	case ServiceSystemd:
		return fmt.Sprintf(systemdAutostartUnitTemplate, exePath, workspace), nil
	case ServiceLaunchd:
		return fmt.Sprintf(launchdAutostartPlistTemplate, exePath, workspace, logPath, serviceLaunchdLabel), nil
	default:
		return "", fmt.Errorf("daemon: no service unit for platform %q (systemd/launchd only)", platform)
	}
}

// LaunchAgentPath is the ~/Library/LaunchAgents path an autostart plist must
// live at: launchd's login scan reads only that directory, so an autostart
// install on darwin defaults there (a plain install keeps the engine-home
// seam).
func LaunchAgentPath() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("daemon: resolve the launch-agent directory: %w", err)
	}
	return filepath.Join(home, "Library", "LaunchAgents", serviceLaunchdLabel+".plist"), nil
}

// InstallServiceUnit generates the host platform's service unit wrapping the
// daemon at exePath and writes it to unitPath, returning the path written.
// autostart renders the always-on variant (foreground daemon, login start,
// restart on failure) instead of the on-demand default. An empty unitPath
// installs at the engine-home ServiceUnitPath seam that engine uninstall also
// removes -- except a darwin autostart install, which must default to the
// ~/Library/LaunchAgents path launchd's login scan reads. It is the single
// unit-installing entrypoint; a structural sweep proves only the
// service-install command calls it.
func InstallServiceUnit(s config.Settings, exePath, unitPath string, autostart bool) (string, error) {
	platform, ok := HostServicePlatform()
	if !ok {
		return "", fmt.Errorf("daemon: service install is unsupported on %s (systemd/launchd only)", runtime.GOOS)
	}
	if unitPath == "" {
		unitPath = ServiceUnitPath(s)
		if autostart && platform == ServiceLaunchd {
			var err error
			if unitPath, err = LaunchAgentPath(); err != nil {
				return "", err
			}
		}
	}
	var unit string
	var err error
	if autostart {
		unit, err = RenderAutostartServiceUnit(platform, exePath, WorkspaceRoot(s), LogPath(s))
	} else {
		unit, err = RenderServiceUnit(platform, exePath, WorkspaceRoot(s), PIDPath(s))
	}
	if err != nil {
		return "", err
	}
	// Intermediate-directory mode: the engine home stays owner-only (0700,
	// private engine state); a --path target outside it gets traversable 0755 so a
	// service manager running as another user can reach the world-readable unit.
	// MkdirAll leaves an existing directory's mode untouched, so this only sets the
	// mode on directories it creates.
	dir := filepath.Dir(unitPath)
	dirPerm := serviceUnitDirPerm
	if dir == irisDir(s) {
		dirPerm = socketDirPerm
	}
	if err := os.MkdirAll(dir, dirPerm); err != nil {
		return "", fmt.Errorf("daemon: create service unit directory for %s: %w", unitPath, err)
	}
	// serviceUnitPerm is the conventional world-readable 0644: a unit carries no
	// secret and the service manager (which may run as another user) must read it.
	if err := os.WriteFile(unitPath, []byte(unit), serviceUnitPerm); err != nil {
		return "", fmt.Errorf("daemon: write service unit %s: %w", unitPath, err)
	}
	return unitPath, nil
}

// UninstallServiceUnit removes the service unit at unitPath, reporting whether a
// unit was present to remove. An already-absent unit is not an error (uninstall is
// idempotent).
func UninstallServiceUnit(unitPath string) (bool, error) {
	if err := os.Remove(unitPath); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return false, nil
		}
		return false, fmt.Errorf("daemon: remove service unit %s: %w", unitPath, err)
	}
	return true, nil
}

// ServiceCtl runs one init-system control command (systemctl, launchctl). The
// production implementation shells out; tests inject a recorder.
type ServiceCtl func(name string, args ...string) error

// OSServiceCtl is the production ServiceCtl: it runs the command and folds its
// combined output into the error, so a refusing init system explains itself.
func OSServiceCtl() ServiceCtl {
	return func(name string, args ...string) error {
		out, err := exec.Command(name, args...).CombinedOutput()
		if err != nil {
			return fmt.Errorf("daemon: %s %s: %w (%s)", name, strings.Join(args, " "), err, strings.TrimSpace(string(out)))
		}
		return nil
	}
}

// ActivateServiceUnit hands the written unit to the host's init system and
// starts it: systemd reloads, then enables --now by absolute path (the symlink
// carries the iris.service name); launchd boots the agent out first (a
// re-install must not fail on an already-loaded job; the bootout error is
// deliberately dropped) and bootstraps the plist into the user's gui domain.
// Called only for an autostart install -- activation is as opt-in as the
// render.
func ActivateServiceUnit(platform ServicePlatform, unitPath string, ctl ServiceCtl) error {
	switch platform {
	case ServiceSystemd:
		if err := ctl("systemctl", "--user", "daemon-reload"); err != nil {
			return err
		}
		return ctl("systemctl", "--user", "enable", "--now", unitPath)
	case ServiceLaunchd:
		domain := "gui/" + strconv.Itoa(os.Getuid())
		_ = ctl("launchctl", "bootout", domain+"/"+serviceLaunchdLabel)
		return ctl("launchctl", "bootstrap", domain, unitPath)
	default:
		return fmt.Errorf("daemon: no service activation for platform %q (systemd/launchd only)", platform)
	}
}

// DeactivateServiceUnit asks the host's init system to stop and forget the
// unit: systemd disables --now by the iris.service name, launchd boots the
// labeled agent out of the user's gui domain. A unit that was never activated
// makes the init system grumble; the caller treats that as best-effort noise,
// so uninstall stays idempotent.
func DeactivateServiceUnit(platform ServicePlatform, ctl ServiceCtl) error {
	switch platform {
	case ServiceSystemd:
		return ctl("systemctl", "--user", "disable", "--now", ServiceUnitName)
	case ServiceLaunchd:
		return ctl("launchctl", "bootout", "gui/"+strconv.Itoa(os.Getuid())+"/"+serviceLaunchdLabel)
	default:
		return fmt.Errorf("daemon: no service deactivation for platform %q (systemd/launchd only)", platform)
	}
}

// ResolveServiceUnitPath resolves where an installed unit lives for uninstall:
// the darwin autostart LaunchAgents path when a unit sits there, else the
// engine-home seam -- mirroring install's own defaulting, so install and
// uninstall never disagree on the unit's location.
func ResolveServiceUnitPath(s config.Settings) string {
	if agent, err := LaunchAgentPath(); err == nil {
		if _, serr := os.Stat(agent); serr == nil {
			return agent
		}
	}
	return ServiceUnitPath(s)
}
