package cli

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestServiceInstallSweep proves the "on demand, never auto-shipped" half of the
// service-unit contract (specification section 2): exactly one command installs a
// service unit, and no other command installs a unit or a boot autostart. It
// mirrors the daemonless-roster sweep -- a structural scan over the tree rather
// than a hand-maintained list -- by scanning the CLI source and asserting the
// unit-installing entrypoint (daemon.InstallServiceUnit) is called only from the
// engine service-install handler, never from install, start, uninstall, or any
// other command.
//
// spec: S02/service-install-on-demand
func TestServiceInstallSweep(t *testing.T) {
	const installCall = "daemon.InstallServiceUnit("
	const uninstallCall = "daemon.UninstallServiceUnit("

	// The service-install and service-uninstall handlers both live in engine.go;
	// that is the only CLI file permitted to invoke the unit lifecycle.
	const allowed = "engine.go"

	installers := filesContaining(t, ".", installCall)
	uninstallers := filesContaining(t, ".", uninstallCall)

	if len(installers) == 0 {
		t.Fatalf("no CLI file calls %s; the service-install command must install a unit", installCall)
	}
	for _, f := range installers {
		if f != allowed {
			t.Errorf("%s calls %s but only %s (the service-install handler) may install a service unit", f, installCall, allowed)
		}
	}
	for _, f := range uninstallers {
		if f != allowed {
			t.Errorf("%s calls %s but only %s (the service-uninstall handler) may remove a service unit", f, uninstallCall, allowed)
		}
	}

	// The engine install/start/uninstall commands, though daemonless, must never
	// reach for the unit installer: assert their source does not.
	engineSrc := readFileString(t, filepath.Join(".", allowed))
	for _, handler := range []string{"func (a *app) engineInstall()", "func (a *app) engineStart()", "func (a *app) engineUninstall()"} {
		body := funcBody(engineSrc, handler)
		if body == "" {
			t.Fatalf("could not locate %s in %s", handler, allowed)
		}
		if strings.Contains(body, installCall) {
			t.Errorf("%s installs a service unit; only engine service install may (no autostart via install/start/uninstall)", handler)
		}
	}
}

// filesContaining returns the base names of the non-test .go files directly under
// dir whose contents contain needle.
func filesContaining(t *testing.T, dir, needle string) []string {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("read dir %s: %v", dir, err)
	}
	var out []string
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		if strings.Contains(readFileString(t, filepath.Join(dir, name)), needle) {
			out = append(out, name)
		}
	}
	return out
}

// readFileString reads path and returns its contents as a string.
func readFileString(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path) //nolint:gosec // G304: path is a CLI source file the structural sweep reads from the trusted module tree, never user or network input.
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	return string(b)
}

// funcBody returns the source text from the start of the function whose signature
// prefix is sig up to the next top-level closing brace at column 0, or "" if the
// signature is not found. It is a coarse extractor sufficient for confining a call
// to a specific handler in gofmt'd source (top-level funcs close with a "\n}").
func funcBody(src, sig string) string {
	i := strings.Index(src, sig)
	if i < 0 {
		return ""
	}
	rest := src[i:]
	if end := strings.Index(rest, "\n}\n"); end >= 0 {
		return rest[:end]
	}
	return rest
}
