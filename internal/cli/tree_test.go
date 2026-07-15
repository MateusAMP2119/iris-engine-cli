package cli

import (
	"io"
	"sort"
	"strings"
	"testing"

	"github.com/spf13/cobra"
)

// testRoot builds a fresh command tree with discarded output. Each test builds
// its own root because cobra mutates flag sets lazily.
func testRoot() *cobra.Command {
	return newApp(io.Discard, io.Discard).newRootCommand()
}

// walk visits cmd and every command beneath it, pre-order.
func walk(cmd *cobra.Command, fn func(*cobra.Command)) {
	fn(cmd)
	for _, c := range cmd.Commands() {
		walk(c, fn)
	}
}

// childNames returns the names of a command's direct subcommands.
func childNames(c *cobra.Command) []string {
	var out []string
	for _, ch := range c.Commands() {
		out = append(out, ch.Name())
	}
	return out
}

// find returns the named direct subcommand, or nil.
func find(c *cobra.Command, name string) *cobra.Command {
	for _, ch := range c.Commands() {
		if ch.Name() == name {
			return ch
		}
	}
	return nil
}

// assertSetEqual fails unless got and want hold the same names (order-independent).
func assertSetEqual(t *testing.T, what string, got, want []string) {
	t.Helper()
	g, w := sortedCopy(got), sortedCopy(want)
	if len(g) != len(w) {
		t.Errorf("%s = %v, want %v", what, g, w)
		return
	}
	for i := range g {
		if g[i] != w[i] {
			t.Errorf("%s = %v, want %v", what, g, w)
			return
		}
	}
}

func sortedCopy(in []string) []string {
	out := append([]string(nil), in...)
	sort.Strings(out)
	return out
}

func mapKeys(m map[string][]string) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}

// wantTree is the documented iris <noun> <verb> tree (with the E14 additions).
// engine.service is the one documented sub-noun.
var wantTree = map[string][]string{
	"declare":    {"apply", "destroy"},
	"pipeline":   {"build", "promote", "run", "list", "show"},
	"run":        {"list", "show", "logs", "cancel"},
	"data":       {"provenance"},
	"workload":   {"show", "wipe"},
	"engine":     {"start", "stop", "install", "uninstall", "logs", "inspect", "connect", "service"},
	"deadletter": {"list", "show", "replay", "drain"},
	"endpoint":   {"apply", "remove", "list", "show"},
	"pat":        {"create", "list", "revoke"},
}

// TestCommandTree pins the shape of the whole tree.
func TestCommandTree(t *testing.T) {
	t.Run("resource-first-command-tree", func(t *testing.T) {
		root := testRoot()

		// Top-level commands are exactly the nine resource nouns plus the four
		// admitted root verbs: the lifecycle pair `update` (self-replace of the
		// binary) and `uninstall` (self-removal of the binary), the onboarding
		// verb `quickstart` (the guided tour), and the process-status verb `ps`
		// (the docker-ps-shaped engine readout), each belonging to no resource
		// noun: no other flat verbs, no extras.
		wantTop := append(mapKeys(wantTree), "update", "uninstall", "quickstart", "ps")
		assertSetEqual(t, "top-level nouns", childNames(root), wantTop)

		// Each noun exposes exactly its documented verbs.
		for noun, verbs := range wantTree {
			c := find(root, noun)
			if c == nil {
				t.Errorf("noun %q missing from the tree", noun)
				continue
			}
			assertSetEqual(t, noun+" verbs", childNames(c), verbs)
		}

		// engine service is the single documented sub-noun (extra depth by design).
		engine := find(root, "engine")
		if engine == nil {
			t.Fatal("engine noun missing")
		}
		svc := find(engine, "service")
		if svc == nil {
			t.Fatal("engine service sub-noun missing")
		}
		assertSetEqual(t, "engine service verbs", childNames(svc), []string{"install", "uninstall"})

		// provenance is the single computed read, only under data; no ad-hoc read
		// verbs anywhere (reads are list/show plus provenance).
		adHocReads := map[string]bool{
			"get": true, "describe": true, "query": true,
			"fetch": true, "read": true, "view": true, "ls": true, "cat": true,
		}
		provenanceCount := 0
		walk(root, func(c *cobra.Command) {
			if adHocReads[c.Name()] {
				t.Errorf("ad-hoc read verb %q present; the only read verbs are list, show, and provenance", c.CommandPath())
			}
			if c.Name() == "provenance" {
				provenanceCount++
				if c.Parent() == nil || c.Parent().Name() != "data" {
					t.Errorf("provenance is under %q, want data", parentName(c))
				}
			}
		})
		if provenanceCount != 1 {
			t.Errorf("provenance appears %d times, want exactly 1", provenanceCount)
		}
	})
}

// TestSoleAlias proves dl is the one and only alias in the tree.
func TestSoleAlias(t *testing.T) {
	t.Run("dl-sole-alias", func(t *testing.T) {
		root := testRoot()
		var aliased []*cobra.Command
		walk(root, func(c *cobra.Command) {
			if len(c.Aliases) > 0 {
				aliased = append(aliased, c)
			}
		})
		if len(aliased) != 1 {
			names := make([]string, 0, len(aliased))
			for _, c := range aliased {
				names = append(names, c.CommandPath())
			}
			t.Fatalf("commands with aliases = %v, want exactly one (deadletter)", names)
		}
		if got := aliased[0].Name(); got != "deadletter" {
			t.Errorf("aliased command = %q, want deadletter", got)
		}
		assertSetEqual(t, "deadletter aliases", aliased[0].Aliases, []string{"dl"})
	})
}

// TestDryRunScope proves --dry-run is registered only on declare apply/destroy.
func TestDryRunScope(t *testing.T) {
	t.Run("dry-run-only-on-declare", func(t *testing.T) {
		root := testRoot()
		walk(root, func(c *cobra.Command) {
			hasDryRun := c.Flags().Lookup("dry-run") != nil
			onDeclareVerb := parentName(c) == "declare" && (c.Name() == "apply" || c.Name() == "destroy")
			if hasDryRun != onDeclareVerb {
				t.Errorf("command %q: registers --dry-run = %v, want %v", c.CommandPath(), hasDryRun, onDeclareVerb)
			}
		})
	})
}

// TestGlobalFlags proves every command accepts the four global flags.
func TestGlobalFlags(t *testing.T) {
	t.Run("global-flags-on-all-commands", func(t *testing.T) {
		root := testRoot()
		globals := []string{"json", "socket", "host", "token"}
		walk(root, func(c *cobra.Command) {
			for _, name := range globals {
				if !acceptsFlag(c, name) {
					t.Errorf("command %q does not accept global flag --%s", c.CommandPath(), name)
				}
			}
		})
	})
}

// TestNoRunShapingFlags proves the run-shaping flags are registered nowhere.
func TestNoRunShapingFlags(t *testing.T) {
	t.Run("no-run-shaping-flags", func(t *testing.T) {
		root := testRoot()
		banned := []string{"param", "timeout", "retry"}
		walk(root, func(c *cobra.Command) {
			for _, name := range banned {
				// acceptsFlag covers local, own-persistent, and inherited scopes, so a
				// banned flag registered anywhere reachable from this command is caught.
				if acceptsFlag(c, name) {
					t.Errorf("command %q registers banned run-shaping flag --%s", c.CommandPath(), name)
				}
			}
		})
	})
}

// acceptsFlag reports whether cmd accepts the named flag as a local, own-
// persistent, or inherited flag -- i.e. whether an invocation of cmd may set it.
func acceptsFlag(c *cobra.Command, name string) bool {
	return c.Flags().Lookup(name) != nil ||
		c.PersistentFlags().Lookup(name) != nil ||
		c.InheritedFlags().Lookup(name) != nil
}

// parentName returns the name of cmd's parent, or "" at the root.
func parentName(c *cobra.Command) string {
	if c.Parent() == nil {
		return ""
	}
	return c.Parent().Name()
}

// TestRunRefGrammar proves the <run> grammar: bare pipeline name means latest
// run of it; <name>~n means nth prior run (0=latest); git ^ and .. are rejected
// as false cognates. Pure unit logic; resolution to id is I/O later.
func TestRunRefGrammar(t *testing.T) {
	tests := []struct {
		ref          string
		wantName     string
		wantPrior    int
		wantErr      bool
		errSubstring string
	}{
		// bare name = latest
		{ref: "extract", wantName: "extract", wantPrior: 0},
		{ref: "load_orders", wantName: "load_orders", wantPrior: 0},
		// ~0 and ~n
		{ref: "foo~0", wantName: "foo", wantPrior: 0},
		{ref: "bar~1", wantName: "bar", wantPrior: 1},
		{ref: "baz~42", wantName: "baz", wantPrior: 42},
		// git false cognates rejected
		{ref: "x^1", wantErr: true, errSubstring: "^"},
		{ref: "y..z", wantErr: true, errSubstring: ".."},
		{ref: "a^b..c", wantErr: true},
		// malformed
		{ref: "", wantErr: true},
		{ref: "~1", wantErr: true},
		{ref: "name~", wantErr: true},
		{ref: "name~-1", wantErr: true},
		{ref: "name~abc", wantErr: true},
		{ref: "name~1~2", wantErr: true},
		{ref: "name~1^", wantErr: true},
	}

	for _, tt := range tests {
		name := tt.ref
		if name == "" {
			name = "(empty)"
		}
		t.Run(name, func(t *testing.T) {
			gotName, gotPrior, err := parseRunRef(tt.ref)
			if tt.wantErr {
				if err == nil {
					t.Fatalf("parseRunRef(%q) err = nil, want error", tt.ref)
				}
				if tt.errSubstring != "" && !strings.Contains(err.Error(), tt.errSubstring) {
					t.Errorf("parseRunRef(%q) err = %v, want substring %q", tt.ref, err, tt.errSubstring)
				}
				return
			}
			if err != nil {
				t.Fatalf("parseRunRef(%q) err = %v, want nil", tt.ref, err)
			}
			if gotName != tt.wantName || gotPrior != tt.wantPrior {
				t.Errorf("parseRunRef(%q) = (%q, %d), want (%q, %d)", tt.ref, gotName, gotPrior, tt.wantName, tt.wantPrior)
			}
		})
	}
}
