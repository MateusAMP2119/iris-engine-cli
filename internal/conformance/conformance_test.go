//go:build conformance

package conformance

import (
	"regexp"
	"testing"
)

// versionRE matches a semantic-version-like token (major.minor.patch) the
// shipped binary prints on bare invocation.
var versionRE = regexp.MustCompile(`\b\d+\.\d+\.\d+`)

// TestConformanceRealBinaryJSON drives the actual shipped iris binary and
// asserts its real exit codes: this is the conformance tier proving its own
// mechanics against the real build, not a fake. The harness compiles ./cmd/iris,
// runs it as a real process, and checks that a bare invocation exits 0 and
// prints a version, while an unknown argument exits 2 (the spec's usage-error
// category, spec section 8).
//
// This proves the runner's spine -- real binary, real process, real exit codes.
// The daemon-over-socket and real-Postgres legs of S16/conformance-real-binary-json
// (grants enforced, every write captured, wipes exact, --json envelopes over the
// socket) land as later epics fill the binary: E02 gives StartDaemon a real
// daemon and managed Postgres, and each subsequent epic's conformance test
// reuses this same harness to drive the matching acceptance-scenario step.
//
// spec: S16/conformance-real-binary-json
func TestConformanceRealBinaryJSON(t *testing.T) {
	bin := Build(t)

	t.Run("bare invocation exits 0 with a version", func(t *testing.T) {
		res := bin.Run(t, RunOptions{})
		res.RequireExit(t, 0)
		if !versionRE.Match(res.Stdout) {
			t.Fatalf("bare invocation stdout = %q, want a version string", res.Stdout)
		}
	})

	t.Run("unknown argument exits 2 (usage error)", func(t *testing.T) {
		res := bin.Run(t, RunOptions{Args: []string{"definitely-not-a-real-command"}})
		res.RequireExit(t, 2)
	})
}
