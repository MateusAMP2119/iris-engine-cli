package cli

import (
	"testing"

	"github.com/spf13/cobra"
)

// daemonFlags are the daemon-scoped flags: they configure a running engine and
// so belong only to `iris engine start`, the
// command that starts the daemon. -d is the shorthand of --detach.
var daemonFlags = []string{
	"detach",
	"pg-dsn",
	"retain",
	"journal-partition-rows",
	"objects-path",
	"tcp",
	"tls-cert",
	"tls-key",
}

// TestEngineStartOwnsDaemonFlags proves the daemon-scoped flags exist only on
// `iris engine start`: every one is registered there, and no other command in the
// tree registers or inherits any of them. The chain begins at engine start, so its
// configuration surface is not leaked onto reads or one-off verbs.
func TestEngineStartOwnsDaemonFlags(t *testing.T) {
	t.Run("engine-start-owns-daemon-flags", func(t *testing.T) {
		root := testRoot()

		walk(root, func(c *cobra.Command) {
			onEngineStart := c.Name() == "start" && parentName(c) == "engine"
			for _, name := range daemonFlags {
				if onEngineStart {
					if c.Flags().Lookup(name) == nil {
						t.Errorf("engine start is missing daemon flag --%s", name)
					}
					continue
				}
				// acceptsFlag covers local, own-persistent, and inherited scopes, so a
				// daemon flag reachable from any other command is caught.
				if acceptsFlag(c, name) {
					t.Errorf("command %q accepts daemon flag --%s; it belongs only on engine start", c.CommandPath(), name)
				}
			}
		})

		// The -d shorthand is the detach flag on engine start and appears nowhere
		// else in the tree.
		start := find(find(root, "engine"), "start")
		if start == nil {
			t.Fatal("engine start command missing from the tree")
		}
		if f := start.Flags().ShorthandLookup("d"); f == nil || f.Name != "detach" {
			t.Errorf("engine start -d shorthand = %v, want the detach flag", f)
		}
		walk(root, func(c *cobra.Command) {
			if c.Name() == "start" && parentName(c) == "engine" {
				return
			}
			if f := c.Flags().ShorthandLookup("d"); f != nil {
				t.Errorf("command %q registers a -d shorthand (%s); -d is the daemon detach flag, only on engine start", c.CommandPath(), f.Name)
			}
		})
	})
}
