package cli

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/spf13/cobra"
)

// writeDeclareTargetFile writes contents at path, creating its parent
// directories, for building single-file/folder apply-destroy fixtures.
func writeDeclareTargetFile(t *testing.T, path, contents string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatalf("mkdir parent of %s: %v", path, err)
	}
	if err := os.WriteFile(path, []byte(contents), 0o644); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
}

const declareTargetPipelineYAML = "name: extract\nrun: [python, main.py]\n"

// declareVerbCmd returns the named verb ("apply" or "destroy") under a fresh
// declare noun, for exercising its Args validation directly with no
// execution side effect.
func declareVerbCmd(t *testing.T, verb string) *cobra.Command {
	t.Helper()
	root := testRoot()
	declareNoun := find(root, "declare")
	if declareNoun == nil {
		t.Fatal("declare noun missing from the command tree")
	}
	c := find(declareNoun, verb)
	if c == nil {
		t.Fatalf("declare %s missing from the command tree", verb)
	}
	return c
}

// TestDeclareSingleFileTarget proves that iris declare apply and iris declare
// destroy each accept exactly one declaration file as target: cobra's arg
// count validation rejects zero, two, or more positional targets (a
// workspace-wide or multi-file invocation), and accepts exactly one.
func TestDeclareSingleFileTarget(t *testing.T) {
	// spec: S03/apply-destroy-single-file
	t.Run("S03/apply-destroy-single-file", func(t *testing.T) {
		for _, verb := range []string{"apply", "destroy"} {
			t.Run(verb, func(t *testing.T) {
				cases := []struct {
					name    string
					args    []string
					wantErr bool
				}{
					{"zero args rejected", nil, true},
					{"exactly one arg accepted", []string{"iris-declare.yaml"}, false},
					{"two args rejected (multi-file)", []string{"a/iris-declare.yaml", "b/iris-declare.yaml"}, true},
					{"three args rejected (workspace-wide)", []string{"a", "b", "c"}, true},
				}
				for _, tc := range cases {
					t.Run(tc.name, func(t *testing.T) {
						cmd := declareVerbCmd(t, verb)
						err := cmd.Args(cmd, tc.args)
						if (err != nil) != tc.wantErr {
							t.Errorf("Args(%v) err = %v, want error presence %v", tc.args, err, tc.wantErr)
						}
					})
				}
			})
		}
	})
}

// TestApplySingleFileResolution proves iris declare apply's target resolution
// (specification sections 6.3 and 8): a bare apply is a usage error (exit 2);
// a resolvable target (file or folder) passes resolution and reaches the
// unchanged daemon-dial stub (exit 3, no daemon reachable, never auto-started
// -- proof resolution succeeded rather than short-circuiting); a folder with
// no iris-declare.yaml is a precise error (exit 4), never a sweep into a
// nested declaration (no workspace sweep, no transitive chaining).
func TestApplySingleFileResolution(t *testing.T) {
	// spec: S06.3/apply-single-file-resolution
	t.Run("S06.3/apply-single-file-resolution", func(t *testing.T) {
		t.Run("bare apply is a usage error (exit 2)", func(t *testing.T) {
			var out, errb bytes.Buffer
			code := newApp(&out, &errb).run([]string{"declare", "apply"})
			if code != exitUsage {
				t.Fatalf("exit = %d, want %d (usage)\nstderr: %s", code, exitUsage, errb.String())
			}
		})

		t.Run("a resolved file target reaches the daemon-dial stub", func(t *testing.T) {
			dir := t.TempDir()
			p := filepath.Join(dir, "iris-declare.yaml")
			writeDeclareTargetFile(t, p, declareTargetPipelineYAML)

			sock := shortSocket(t)
			var out, errb bytes.Buffer
			code := newApp(&out, &errb).run([]string{"--socket", sock, "declare", "apply", p})
			if code != exitNoDaemon {
				t.Fatalf("exit = %d, want %d (no-daemon, proving the file target resolved)\nstderr: %s", code, exitNoDaemon, errb.String())
			}
		})

		t.Run("a folder target resolves to its iris-declare.yaml", func(t *testing.T) {
			dir := t.TempDir()
			writeDeclareTargetFile(t, filepath.Join(dir, "iris-declare.yaml"), declareTargetPipelineYAML)

			sock := shortSocket(t)
			var out, errb bytes.Buffer
			code := newApp(&out, &errb).run([]string{"--socket", sock, "declare", "apply", dir})
			if code != exitNoDaemon {
				t.Fatalf("exit = %d, want %d (no-daemon, proving the folder resolved to its file)\nstderr: %s", code, exitNoDaemon, errb.String())
			}
		})

		t.Run("a folder with no declaration is a precise error, not a sweep", func(t *testing.T) {
			root := t.TempDir()
			// Plant a real declaration well below the target folder; a sweep would
			// find it, but apply must fail on the target itself.
			nested := filepath.Join(root, "pipelines", "ingest", "extract", "iris-declare.yaml")
			writeDeclareTargetFile(t, nested, declareTargetPipelineYAML)

			var out, errb bytes.Buffer
			code := newApp(&out, &errb).run([]string{"declare", "apply", root})
			if code != exitOpFailed {
				t.Fatalf("exit = %d, want %d (operation failed, no sweep into the nested declaration)\nstderr: %s", code, exitOpFailed, errb.String())
			}
			if !strings.Contains(errb.String(), "iris-declare.yaml") {
				t.Errorf("error does not name the missing declaration file: %q", errb.String())
			}
		})

		t.Run("a malformed declaration surfaces its parse error (exit 4)", func(t *testing.T) {
			dir := t.TempDir()
			p := filepath.Join(dir, "iris-declare.yaml")
			writeDeclareTargetFile(t, p, "name: extract\n") // missing required run

			var out, errb bytes.Buffer
			code := newApp(&out, &errb).run([]string{"declare", "apply", p})
			if code != exitOpFailed {
				t.Fatalf("exit = %d, want %d (operation failed)\nstderr: %s", code, exitOpFailed, errb.String())
			}
		})
	})
}

// TestDeclareBareUsageError proves that a bare iris declare apply or iris
// declare destroy is a usage error (exit 2), and that no --all flag is
// registered anywhere under the declare noun (specification section 8: no
// workspace sweep, so no flag that would request one).
func TestDeclareBareUsageError(t *testing.T) {
	// spec: S08/declare-bare-usage-error
	t.Run("S08/declare-bare-usage-error", func(t *testing.T) {
		for _, verb := range []string{"apply", "destroy"} {
			t.Run(verb, func(t *testing.T) {
				var out, errb bytes.Buffer
				code := newApp(&out, &errb).run([]string{"declare", verb})
				if code != exitUsage {
					t.Fatalf("bare declare %s: exit = %d, want %d (usage)\nstderr: %s", verb, code, exitUsage, errb.String())
				}
			})
		}

		t.Run("no --all flag anywhere under declare", func(t *testing.T) {
			root := testRoot()
			declareNoun := find(root, "declare")
			if declareNoun == nil {
				t.Fatal("declare noun missing from the command tree")
			}
			walk(declareNoun, func(c *cobra.Command) {
				if acceptsFlag(c, "all") {
					t.Errorf("command %q accepts --all; declare offers no --all by design (no workspace sweep)", c.CommandPath())
				}
			})
		})
	})
}

// TestDestroySingleDeclaration proves iris declare destroy accepts exactly
// one declaration file per invocation -- the same single-target rule as
// apply, folder resolution included -- so a full teardown is one confirmed
// destroy per declaration (leaf-first ordering is enforced elsewhere, later
// epics E03.10/E10).
func TestDestroySingleDeclaration(t *testing.T) {
	// spec: S12/destroy-single-declaration
	t.Run("S12/destroy-single-declaration", func(t *testing.T) {
		t.Run("exactly one declaration file per invocation", func(t *testing.T) {
			cmd := declareVerbCmd(t, "destroy")
			if err := cmd.Args(cmd, []string{"a/iris-declare.yaml", "b/iris-declare.yaml"}); err == nil {
				t.Error("destroy with two targets: want an error (one declaration per invocation)")
			}
			if err := cmd.Args(cmd, []string{"a/iris-declare.yaml"}); err != nil {
				t.Errorf("destroy with one target: want nil, got %v", err)
			}
		})

		t.Run("bare destroy is a usage error (exit 2)", func(t *testing.T) {
			var out, errb bytes.Buffer
			code := newApp(&out, &errb).run([]string{"declare", "destroy"})
			if code != exitUsage {
				t.Fatalf("exit = %d, want %d (usage)\nstderr: %s", code, exitUsage, errb.String())
			}
		})

		t.Run("a folder target resolves to its iris-declare.yaml, same as apply", func(t *testing.T) {
			dir := t.TempDir()
			writeDeclareTargetFile(t, filepath.Join(dir, "iris-declare.yaml"), declareTargetPipelineYAML)

			sock := shortSocket(t)
			var out, errb bytes.Buffer
			code := newApp(&out, &errb).run([]string{"--socket", sock, "declare", "destroy", dir, "--yes"})
			if code != exitNoDaemon {
				t.Fatalf("exit = %d, want %d (no-daemon, proving the folder resolved)\nstderr: %s", code, exitNoDaemon, errb.String())
			}
		})
	})
}
