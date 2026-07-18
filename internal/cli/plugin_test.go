package cli

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/MateusAMP2119/iris-lakehouse/internal/plugin"
)

// writePluginSource lays a valid tool manifest + binary and returns the manifest path.
func writePluginSource(t *testing.T) string {
	t.Helper()
	src := t.TempDir()
	binary := []byte("#!/bin/sh\nprintf '{}'\n")
	if err := os.WriteFile(filepath.Join(src, "bin"), binary, 0o755); err != nil { //nolint:gosec // test plugin binary
		t.Fatal(err)
	}
	manifest := fmt.Sprintf("name: mailer\nversion: \"1.0\"\nkind: tool\nverbs:\n  send: {}\nbinaries:\n  %s:\n    url: ./bin\n    sha256: \"%s\"\n",
		plugin.Platform(), plugin.Digest(binary))
	path := filepath.Join(src, plugin.ManifestFile)
	if err := os.WriteFile(path, []byte(manifest), 0o644); err != nil {
		t.Fatal(err)
	}
	return path
}

// runPlugin runs one iris plugin invocation against an isolated IRIS_HOME.
func runPlugin(t *testing.T, home string, args ...string) (string, int) {
	t.Helper()
	t.Setenv("IRIS_HOME", home)
	var out, errOut bytes.Buffer
	code := Execute(append([]string{"plugin"}, args...), &out, &errOut)
	return out.String() + errOut.String(), code
}

func TestPluginCommandLifecycle(t *testing.T) {
	home := t.TempDir()
	manifest := writePluginSource(t)

	out, code := runPlugin(t, home, "install", manifest)
	if code != 0 || !strings.Contains(out, "installed mailer@1.0") {
		t.Fatalf("install: code %d, out %q", code, out)
	}

	out, code = runPlugin(t, home, "list")
	if code != 0 || !strings.Contains(out, "mailer@1.0") || !strings.Contains(out, "verbs send") {
		t.Fatalf("list: code %d, out %q", code, out)
	}

	out, code = runPlugin(t, home, "verify")
	if code != 0 {
		t.Fatalf("verify: code %d, out %q", code, out)
	}

	// Tamper: verify must fail with exit 4 and name the breakage.
	bin := filepath.Join(home, plugin.DirName, "mailer", "1.0", "mailer")
	if err := os.WriteFile(bin, []byte("swapped"), 0o755); err != nil { //nolint:gosec // test tamper
		t.Fatal(err)
	}
	out, code = runPlugin(t, home, "verify")
	if code != 4 || !strings.Contains(out, "BROKEN") {
		t.Fatalf("tampered verify: code %d, out %q", code, out)
	}

	out, code = runPlugin(t, home, "remove", "mailer@1.0")
	if code != 0 || !strings.Contains(out, "removed mailer@1.0") {
		t.Fatalf("remove: code %d, out %q", code, out)
	}
	out, code = runPlugin(t, home, "remove", "mailer")
	if code != 4 || !strings.Contains(out, "not installed") {
		t.Fatalf("second remove: code %d, out %q", code, out)
	}
	out, code = runPlugin(t, home, "list")
	if code != 0 || !strings.Contains(out, "no plugins installed") {
		t.Fatalf("empty list: code %d, out %q", code, out)
	}
}

func TestPluginInstallRefusesBadManifest(t *testing.T) {
	home := t.TempDir()
	bad := filepath.Join(t.TempDir(), "manifest.yaml")
	if err := os.WriteFile(bad, []byte("name: x\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	out, code := runPlugin(t, home, "install", bad)
	if code != 4 || !strings.Contains(out, "iris plugin install") {
		t.Fatalf("bad install: code %d, out %q", code, out)
	}
}
