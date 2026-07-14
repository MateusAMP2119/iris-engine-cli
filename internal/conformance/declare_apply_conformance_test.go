//go:build conformance

package conformance

import (
	"path/filepath"
	"testing"

	"github.com/MateusAMP2119/iris-engine-cli/internal/fixtures"
)

// TestApplyStrictlySingleFile drives the real iris binary and proves iris declare
// apply is strictly single-file: exactly
// one declaration is accepted per invocation, a bare invocation exits 2 (usage
// error, no --all by design), and two targets in one invocation are rejected the same
// way. A single valid target is accepted past that gate (it proceeds to reach the
// daemon, exiting no-daemon rather than usage), so the single-file rule admits one
// and only one declaration.
//
// This is a pure CLI-contract leg: the arg-count rule is enforced before any daemon
// or Postgres is touched, so it needs neither. The daemon-backed idempotency leg is
// TestApplyRepeatNoop.
func TestApplyStrictlySingleFile(t *testing.T) {
	t.Run("apply-single-file-bare-exit-2", func(t *testing.T) {
		bin := Build(t)

		// A bare invocation exits 2 (usage error) with a message on stderr: no default
		// to everything, no --all.
		bare := bin.Run(t, RunOptions{Args: []string{"declare", "apply"}})
		bare.RequireExit(t, 2)
		if len(bare.Stderr) == 0 {
			t.Errorf("bare `declare apply` wrote no usage error to stderr\nstdout:\n%s", bare.Stdout)
		}

		// Two targets in one invocation are rejected: exactly one declaration per
		// invocation, never a set.
		golden := fixtures.WorkspaceGolden()
		extract := filepath.Join(golden, "pipelines", "ingest", "extract_orders")
		reset := filepath.Join(golden, "pipelines", "ingest", "reset_counters")
		two := bin.Run(t, RunOptions{Args: []string{"declare", "apply", extract, reset}})
		two.RequireExit(t, 2)

		// A single valid target is accepted past the single-file gate: with no daemon
		// reachable it exits no-daemon (3), never usage (2). One declaration is enough,
		// and enough is one.
		one := bin.Run(t, RunOptions{
			Args: []string{"declare", "apply", extract},
			Dir:  t.TempDir(), // an empty workspace: no daemon socket to reach.
		})
		if one.ExitCode == 2 {
			t.Errorf("a single valid target was rejected as a usage error (exit 2); one declaration must be accepted\nstderr:\n%s", one.Stderr)
		}
	})
}
