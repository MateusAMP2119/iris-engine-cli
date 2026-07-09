package cli

import (
	"testing"

	"github.com/spf13/cobra"
)

// TestNoRuntimeParams proves the CLI registers no runtime-parameter flag anywhere in
// the command tree -- no --param and no params-file -- so the yaml declaration fully
// determines every run (specification section 1: runtime parameters removed). It
// sweeps the whole tree, checking local, own-persistent, and inherited flag scopes,
// so a banned flag reachable from any command is caught.
func TestNoRuntimeParams(t *testing.T) {
	// spec: S01/no-runtime-params
	root := testRoot()
	banned := []string{"param", "params", "params-file", "param-file"}
	walk(root, func(c *cobra.Command) {
		for _, name := range banned {
			if acceptsFlag(c, name) {
				t.Errorf("command %q registers runtime-parameter flag --%s; the yaml declaration must fully determine a run", c.CommandPath(), name)
			}
		}
	})
}
