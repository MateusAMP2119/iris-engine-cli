//go:build conformance

package conformance

import (
	"testing"
)

// TestConformanceRealBinaryJSON drives the actual shipped iris binary as a real
// process and asserts only what the specification fixes: the runner mechanics
// (the real binary is built, invoked, and its stdout/stderr/exit code captured)
// and the exit-code categories of spec section 8 -- a bare invocation exits with
// the success code, and an unknown argument exits 2, the usage-error category,
// with the usage error on stderr. It deliberately asserts no stdout SHAPE: the
// spec pins exit codes, not the words the binary prints, so this test stands
// unchanged when E01 replaces the placeholder with the cobra tree (bare iris
// printing root help instead of a version, still exit 0).
//
// This proves the runner's spine -- real binary, real process, real exit codes.
// The remaining legs of S16/conformance-real-binary-json (a daemon over a unix
// socket, a real Postgres created by the engine, grants enforced, every write
// captured, wipes exact, and the single-JSON --json envelope decoded by
// Result.DecodeJSON) activate as later epics fill the binary: E01 gives it a
// --json surface, E02 gives StartDaemon a real daemon and managed Postgres, and
// each subsequent epic's conformance test reuses this same harness to drive the
// matching acceptance-scenario step.
//
// spec: S16/conformance-real-binary-json
func TestConformanceRealBinaryJSON(t *testing.T) {
	bin := Build(t)

	t.Run("bare invocation exits with the success code", func(t *testing.T) {
		// Spec section 8 category 0. Exit code only, never stdout shape: E01's
		// root help exits 0 exactly as today's placeholder version banner does.
		res := bin.Run(t, RunOptions{})
		res.RequireExit(t, 0)
	})

	t.Run("unknown argument exits 2 with a usage error on stderr", func(t *testing.T) {
		// Spec section 8 category 2 (usage error); detail lives in the message.
		res := bin.Run(t, RunOptions{Args: []string{"definitely-not-a-real-command"}})
		res.RequireExit(t, 2)
		if len(res.Stderr) == 0 {
			t.Fatalf("usage error wrote nothing to stderr\nstdout:\n%s", res.Stdout)
		}
	})
}
