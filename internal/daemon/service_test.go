package daemon_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/MateusAMP2119/iris-engine-cli/internal/config"
	"github.com/MateusAMP2119/iris-engine-cli/internal/daemon"
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

			written, err := daemon.InstallServiceUnit(s, exe, unitPath)
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

			if _, err := daemon.InstallServiceUnit(s, exe, unitPath); err != nil {
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
			written, err := daemon.InstallServiceUnit(s, exe, "")
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
