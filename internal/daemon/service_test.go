package daemon_test

import (
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"testing"

	"github.com/MateusAMP2119/iris-lakehouse/internal/config"
	"github.com/MateusAMP2119/iris-lakehouse/internal/daemon"
)

// TestServiceInstallOnDemand proves `iris engine service install` generates a
// platform service unit (systemd on linux, launchd on darwin) that wraps the
// detached daemon and is written to the agreed unit path, and that `service
// uninstall` removes exactly that unit (on demand, never auto-shipped; the unit
// wraps the detached daemon). The generation is proven for both platforms
// (rendering is host-independent so both legs are exercised regardless of the test
// host); install/uninstall file mechanics are proven against a temp path.
func TestServiceInstallOnDemand(t *testing.T) {
	t.Run("service-install-on-demand", func(t *testing.T) {
		const exe = "/opt/iris/bin/iris"
		ws := "/home/op/project"
		pidPath := filepath.Join(ws, ".iris", "iris.pid")

		t.Run("platform mapping: linux->systemd, darwin->launchd, others unsupported", func(t *testing.T) {
			cases := []struct {
				goos string
				want daemon.ServicePlatform
				ok   bool
			}{
				{"linux", daemon.ServiceSystemd, true},
				{"darwin", daemon.ServiceLaunchd, true},
				{"windows", "", false},
				{"plan9", "", false},
			}
			for _, c := range cases {
				got, ok := daemon.ServicePlatformForGOOS(c.goos)
				if ok != c.ok || (ok && got != c.want) {
					t.Errorf("ServicePlatformForGOOS(%q) = (%q,%v), want (%q,%v)", c.goos, got, ok, c.want, c.ok)
				}
			}
		})

		t.Run("systemd unit wraps the detached daemon rooted at the workspace", func(t *testing.T) {
			unit, err := daemon.RenderServiceUnit(daemon.ServiceSystemd, exe, ws, pidPath)
			if err != nil {
				t.Fatalf("RenderServiceUnit(systemd): %v", err)
			}
			for _, want := range []string{
				"[Service]",
				exe,        // the real binary
				"engine",   // ... running the engine ...
				"start",    // ... start verb ...
				"--detach", // ... detached
				"WorkingDirectory=" + ws,
				"PIDFile=" + pidPath,
				"SIGTERM", // clean signal-driven stop
			} {
				if !strings.Contains(unit, want) {
					t.Errorf("systemd unit missing %q:\n%s", want, unit)
				}
			}
		})

		t.Run("launchd plist wraps the detached daemon and never autostarts at boot", func(t *testing.T) {
			unit, err := daemon.RenderServiceUnit(daemon.ServiceLaunchd, exe, ws, pidPath)
			if err != nil {
				t.Fatalf("RenderServiceUnit(launchd): %v", err)
			}
			for _, want := range []string{
				"<plist",
				"ProgramArguments",
				exe,
				"engine",
				"start",
				"--detach",
				"WorkingDirectory",
				ws,
				"RunAtLoad", // ... explicitly not at load/boot
			} {
				if !strings.Contains(unit, want) {
					t.Errorf("launchd plist missing %q:\n%s", want, unit)
				}
			}
			// No boot autostart: RunAtLoad is false.
			if !strings.Contains(unit, "<key>RunAtLoad</key>") || !strings.Contains(unit, "<false") {
				t.Errorf("launchd plist must set RunAtLoad false (no boot autostart):\n%s", unit)
			}
		})

		t.Run("unsupported platform is a clear error, not a silent empty unit", func(t *testing.T) {
			if _, err := daemon.RenderServiceUnit(daemon.ServicePlatform("windows"), exe, ws, pidPath); err == nil {
				t.Error("RenderServiceUnit for an unsupported platform returned no error")
			}
		})

		t.Run("install writes the unit; uninstall removes exactly it", func(t *testing.T) {
			tmp := t.TempDir()
			realWS := t.TempDir()
			s := config.Resolve(config.Defaults(realWS), config.Layer{}, config.Layer{}, config.Layer{})
			unitPath := filepath.Join(tmp, "iris.unit")

			written, err := daemon.InstallServiceUnit(s, exe, unitPath, false)
			if err != nil {
				t.Fatalf("InstallServiceUnit: %v", err)
			}
			if written != unitPath {
				t.Errorf("InstallServiceUnit returned %q, want %q", written, unitPath)
			}
			body, err := os.ReadFile(unitPath) //nolint:gosec // G304: unitPath is under the test's throwaway temp directory, never user or network input.
			if err != nil {
				t.Fatalf("read installed unit: %v", err)
			}
			// The installed unit wraps the daemon: it names the binary and the
			// detached start regardless of host platform.
			for _, want := range []string{exe, "engine", "start", "--detach"} {
				if !strings.Contains(string(body), want) {
					t.Errorf("installed unit missing %q:\n%s", want, body)
				}
			}

			removed, err := daemon.UninstallServiceUnit(unitPath)
			if err != nil {
				t.Fatalf("UninstallServiceUnit: %v", err)
			}
			if !removed {
				t.Error("UninstallServiceUnit reported nothing removed, but a unit was installed")
			}
			if _, err := os.Stat(unitPath); !os.IsNotExist(err) {
				t.Errorf("unit survived uninstall (stat err=%v)", err)
			}

			// Uninstall is idempotent: removing an absent unit is not an error.
			removedAgain, err := daemon.UninstallServiceUnit(unitPath)
			if err != nil {
				t.Fatalf("second UninstallServiceUnit: %v", err)
			}
			if removedAgain {
				t.Error("second uninstall reported a removal, but the unit was already gone")
			}
		})

		t.Run("install creates traversable parent dirs for an out-of-workspace --path", func(t *testing.T) {
			base := t.TempDir()
			realWS := t.TempDir()
			s := config.Resolve(config.Defaults(realWS), config.Layer{}, config.Layer{}, config.Layer{})
			unitPath := filepath.Join(base, "sub", "systemd", "iris.service")

			if _, err := daemon.InstallServiceUnit(s, exe, unitPath, false); err != nil {
				t.Fatalf("InstallServiceUnit(nested --path): %v", err)
			}

			// The unit file is world-readable (0644); an owner-only 0700 parent would
			// block a service manager running as another user from traversing to it.
			// The created intermediate dirs must be group/other-traversable -- 0755
			// under the process umask, compared against a reference dir created the
			// same way so the assertion is umask-independent.
			refParent := filepath.Join(t.TempDir(), "ref")
			if err := os.MkdirAll(refParent, 0o755); err != nil {
				t.Fatalf("reference MkdirAll: %v", err)
			}
			ref, err := os.Stat(refParent)
			if err != nil {
				t.Fatalf("stat reference dir: %v", err)
			}
			got, err := os.Stat(filepath.Join(base, "sub"))
			if err != nil {
				t.Fatalf("stat created intermediate dir: %v", err)
			}
			if got.Mode().Perm() != ref.Mode().Perm() {
				t.Errorf("intermediate dir perm = %#o, want %#o (0755 under umask; a 0700 parent blocks non-owner traversal to the world-readable unit)",
					got.Mode().Perm(), ref.Mode().Perm())
			}
		})

		t.Run("install defaults to the ServiceUnitPath seam", func(t *testing.T) {
			realWS := t.TempDir()
			s := config.Resolve(config.Defaults(realWS), config.Layer{}, config.Layer{}, config.Layer{})

			// An empty target path installs at the agreed workspace-local seam that
			// engine uninstall also removes, so the two never disagree on location.
			written, err := daemon.InstallServiceUnit(s, exe, "", false)
			if err != nil {
				t.Fatalf("InstallServiceUnit(default path): %v", err)
			}
			if written != daemon.ServiceUnitPath(s) {
				t.Errorf("default install path = %q, want ServiceUnitPath %q", written, daemon.ServiceUnitPath(s))
			}
			if _, err := os.Stat(daemon.ServiceUnitPath(s)); err != nil {
				t.Errorf("unit not written at the default seam: %v", err)
			}
		})
	})
}

// TestAutostartServiceUnit proves the opt-in always-on variant: the rendered
// units run the FOREGROUND daemon under the service manager's supervision,
// start at login, restart on failure only, and the activation/deactivation
// hand the exact expected commands to the init system.
func TestAutostartServiceUnit(t *testing.T) {
	t.Run("autostart-service-unit", func(t *testing.T) {
		exe := "/usr/local/bin/iris"
		ws := "/data/warehouse"
		logPath := "/home/u/.iris/logs/daemon.log"

		t.Run("systemd autostart supervises the foreground daemon", func(t *testing.T) {
			unit, err := daemon.RenderAutostartServiceUnit(daemon.ServiceSystemd, exe, ws, logPath)
			if err != nil {
				t.Fatalf("RenderAutostartServiceUnit(systemd): %v", err)
			}
			for _, want := range []string{
				"Type=exec",
				"ExecStart=" + exe + " engine start\n", // foreground: no --detach
				"Restart=on-failure",
				"WorkingDirectory=" + ws,
				"WantedBy=default.target",
			} {
				if !strings.Contains(unit, want) {
					t.Errorf("systemd autostart unit missing %q:\n%s", want, unit)
				}
			}
			if strings.Contains(unit, "--detach") || strings.Contains(unit, "PIDFile") {
				t.Errorf("systemd autostart unit must supervise the foreground daemon, not a forking wrapper:\n%s", unit)
			}
		})

		t.Run("launchd autostart runs at login and restarts on failure only", func(t *testing.T) {
			unit, err := daemon.RenderAutostartServiceUnit(daemon.ServiceLaunchd, exe, ws, logPath)
			if err != nil {
				t.Fatalf("RenderAutostartServiceUnit(launchd): %v", err)
			}
			for _, want := range []string{
				"<key>RunAtLoad</key>\n\t<true/>",
				"<key>SuccessfulExit</key>", // restart on failure only: stop never fights launchd
				"<key>StandardOutPath</key>\n\t<string>" + logPath + "</string>",
			} {
				if !strings.Contains(unit, want) {
					t.Errorf("launchd autostart plist missing %q:\n%s", want, unit)
				}
			}
			if strings.Contains(unit, "--detach") {
				t.Errorf("launchd autostart plist must run the foreground daemon:\n%s", unit)
			}
		})

		t.Run("activation hands the init system the exact commands", func(t *testing.T) {
			var got [][]string
			ctl := func(name string, args ...string) error {
				got = append(got, append([]string{name}, args...))
				return nil
			}
			if err := daemon.ActivateServiceUnit(daemon.ServiceSystemd, "/u/iris.service", ctl); err != nil {
				t.Fatalf("ActivateServiceUnit(systemd): %v", err)
			}
			want := [][]string{
				{"systemctl", "--user", "daemon-reload"},
				{"systemctl", "--user", "enable", "--now", "/u/iris.service"},
			}
			if !reflect.DeepEqual(got, want) {
				t.Errorf("systemd activation = %v, want %v", got, want)
			}

			got = nil
			domain := "gui/" + strconv.Itoa(os.Getuid())
			if err := daemon.ActivateServiceUnit(daemon.ServiceLaunchd, "/u/agent.plist", ctl); err != nil {
				t.Fatalf("ActivateServiceUnit(launchd): %v", err)
			}
			want = [][]string{
				{"launchctl", "bootout", domain + "/com.iris.engine"},
				{"launchctl", "bootstrap", domain, "/u/agent.plist"},
			}
			if !reflect.DeepEqual(got, want) {
				t.Errorf("launchd activation = %v, want %v", got, want)
			}

			got = nil
			if err := daemon.DeactivateServiceUnit(daemon.ServiceSystemd, ctl); err != nil {
				t.Fatalf("DeactivateServiceUnit(systemd): %v", err)
			}
			if want := [][]string{{"systemctl", "--user", "disable", "--now", daemon.ServiceUnitName}}; !reflect.DeepEqual(got, want) {
				t.Errorf("systemd deactivation = %v, want %v", got, want)
			}
		})

		t.Run("a re-installed launchd agent survives an already-loaded bootout failure", func(t *testing.T) {
			calls := 0
			ctl := func(_ string, args ...string) error {
				calls++
				if args[0] == "bootout" {
					return errors.New("Boot-out failed: 5: Input/output error")
				}
				return nil
			}
			if err := daemon.ActivateServiceUnit(daemon.ServiceLaunchd, "/u/agent.plist", ctl); err != nil {
				t.Fatalf("activation must drop the bootout error: %v", err)
			}
			if calls != 2 {
				t.Errorf("activation made %d calls, want bootout then bootstrap", calls)
			}
		})
	})
}
