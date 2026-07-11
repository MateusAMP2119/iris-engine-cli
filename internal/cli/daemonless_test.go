package cli

import (
	"testing"

	"github.com/spf13/cobra"
)

// isLeafCommand reports whether c is a runnable leaf: a command with no
// user-facing subcommands (cobra's auto-added help/completion do not count) and
// not the root. Group/noun nodes have real children and are not leaves.
func isLeafCommand(c *cobra.Command) bool {
	if c.Name() == "help" || c.Name() == "completion" {
		return false
	}
	if c.Parent() == nil {
		return false // the root is not a leaf command
	}
	for _, ch := range c.Commands() {
		if ch.Name() == "help" || ch.Name() == "completion" {
			continue
		}
		return false // has a real subcommand: a group, not a leaf
	}
	return true
}

// TestDaemonlessLifecycleCommands proves the daemonless roster (specification
// section 2): exactly `iris engine install`, `engine start`, `engine service
// install`, `engine service uninstall`, `engine uninstall`, `engine update`, and
// the root `iris uninstall` (self-removal of the binary) are classified runnable
// without a daemon; every other leaf command is
// daemon-touching. The
// classification is an explicit lifecycle annotation set at command construction
// (reused by later epics), so this sweep reads the annotations, not a string
// list: every leaf must carry exactly one, and the daemonless set must match the
// roster exactly.
func TestDaemonlessLifecycleCommands(t *testing.T) {
	// spec: S02/daemonless-lifecycle-commands
	t.Run("S02/daemonless-lifecycle-commands", func(t *testing.T) {
		root := testRoot()

		wantDaemonless := map[string]bool{
			"iris engine install":           true,
			"iris engine start":             true,
			"iris engine uninstall":         true,
			"iris engine service install":   true,
			"iris engine service uninstall": true,
			"iris engine update":            true,
			"iris uninstall":                true,
		}
		gotDaemonless := map[string]bool{}

		walk(root, func(c *cobra.Command) {
			if !isLeafCommand(c) {
				return
			}
			life, ok := c.Annotations[lifecycleAnnotation]
			if !ok {
				t.Errorf("leaf %q carries no lifecycle annotation; every leaf must be classified", c.CommandPath())
				return
			}
			switch life {
			case lifecycleDaemonless:
				gotDaemonless[c.CommandPath()] = true
			case lifecycleDaemonTouching:
				// The default and majority: verified below by exclusion.
			default:
				t.Errorf("leaf %q has unknown lifecycle annotation %q", c.CommandPath(), life)
			}
		})

		// The daemonless set matches the spec roster exactly: no missing, no extra.
		for path := range wantDaemonless {
			if !gotDaemonless[path] {
				t.Errorf("command %q is not classified daemonless but must be (spec section 2 roster)", path)
			}
		}
		for path := range gotDaemonless {
			if !wantDaemonless[path] {
				t.Errorf("command %q is classified daemonless but is not in the spec section 2 roster", path)
			}
		}
	})
}
