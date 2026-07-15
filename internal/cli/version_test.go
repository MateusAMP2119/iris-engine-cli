package cli

import (
	"bytes"
	"io"
	"testing"

	"github.com/MateusAMP2119/iris-lakehouse/internal/buildinfo"
)

// runVersion executes the root command with a single flag and returns what it
// wrote to stdout. It captures command output on a buffer so the exact bytes of
// the version surface can be asserted, and fails the test on any execution error.
func runVersion(t *testing.T, flag string) string {
	t.Helper()
	var out bytes.Buffer
	root := newApp(&out, io.Discard).newRootCommand()
	root.SetArgs([]string{flag})
	root.SetOut(&out)
	if err := root.Execute(); err != nil {
		t.Fatalf("iris %s: unexpected error: %v", flag, err)
	}
	return out.String()
}

// TestVersionFlagDefaultsDev proves the root command exposes a --version flag
// wired to the build's version string, which for an ordinary (unstamped) build is
// "dev". The default is asserted through buildinfo.Version rather than a literal
// so the test tracks the constant, yet an unstamped test build must observe the
// "dev" default -- the value the release workflow overrides with -ldflags -X.
func TestVersionFlagDefaultsDev(t *testing.T) {
	if buildinfo.Version != "dev" {
		t.Fatalf("unstamped build: buildinfo.Version = %q, want the \"dev\" default", buildinfo.Version)
	}
	got := runVersion(t, "--version")
	if want := "iris version dev\n"; got != want {
		t.Errorf("iris --version = %q, want %q", got, want)
	}
}

// TestVersionTemplateFormat proves the version output is exactly
// "iris version <Version>\n": the "iris version " prefix, the build's version
// string verbatim, and a single trailing newline, with no help text or usage
// noise. Both --version and its short form -v (if wired) resolve to the same
// single-line surface the installer parses.
func TestVersionTemplateFormat(t *testing.T) {
	got := runVersion(t, "--version")
	want := "iris version " + buildinfo.Version + "\n"
	if got != want {
		t.Errorf("iris --version = %q, want %q", got, want)
	}
}
