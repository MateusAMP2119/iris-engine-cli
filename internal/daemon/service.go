package daemon

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"

	"github.com/MateusAMP2119/iris-engine-cli/internal/config"
)

// This file generates the platform service unit `iris engine service install`
// writes on demand (on demand, never auto-shipped: iris engine service install
// installs a systemd/launchd unit wrapping the detached daemon, itself
// service-ready (clean SIGTERM/SIGINT, no TTY, sane exit codes)). The unit wraps
// `<binary> engine start --detach` rooted at the workspace, so the service manager
// runs the same detached daemon the CLI does. It is generated only here and written
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
// the owner-only mode the rest of the .iris tree uses.
const serviceUnitPerm os.FileMode = 0o644

// serviceUnitDirPerm is the mode intermediate directories are created with for a
// --path target outside the workspace: 0755, so a service manager running as a
// different user can traverse to the world-readable unit file. The .iris tree's
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

// WorkspaceRoot returns the workspace root the settings are rooted at (the parent
// of the .iris tree), used as the service unit's WorkingDirectory so the daemon
// starts in the same workspace the CLI ran from.
func WorkspaceRoot(s config.Settings) string {
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

// InstallServiceUnit generates the host platform's service unit wrapping the
// detached daemon at exePath and writes it to unitPath, returning the path
// written. An empty unitPath installs at the workspace-local ServiceUnitPath seam
// that engine uninstall also removes, so the two never disagree on the unit's
// location. It is the single unit-installing entrypoint; a structural sweep proves
// only the service-install command calls it.
func InstallServiceUnit(s config.Settings, exePath, unitPath string) (string, error) {
	platform, ok := HostServicePlatform()
	if !ok {
		return "", fmt.Errorf("daemon: service install is unsupported on %s (systemd/launchd only)", runtime.GOOS)
	}
	if unitPath == "" {
		unitPath = ServiceUnitPath(s)
	}
	unit, err := RenderServiceUnit(platform, exePath, WorkspaceRoot(s), PIDPath(s))
	if err != nil {
		return "", err
	}
	// Intermediate-directory mode: the workspace .iris tree stays owner-only (0700,
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
