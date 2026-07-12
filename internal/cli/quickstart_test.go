package cli

import (
	"bytes"
	"context"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/MateusAMP2119/iris-engine-cli/internal/golden"
)

// runQuickstart drives `iris quickstart <args...>` with the two TTY seams
// forced, returning stdout, stderr, and the exit code. The tour seams are
// pinned shut -- the workspace question reads EOF, the first gate quits, and
// any executed step fails the test -- so the gating tests stay about gating:
// they never read the process stdin, never touch the filesystem, and never run
// a step, whichever rendering the gate picks.
func runQuickstart(t *testing.T, stdoutTTY, stdinTTY bool, args ...string) (stdout, stderr string, code int) {
	t.Helper()
	var out, errb bytes.Buffer
	a := newApp(&out, &errb)
	a.isTTY = func() bool { return stdoutTTY }
	a.stdinIsTTY = func() bool { return stdinTTY }
	a.tourPick = func(string, int) (int, promptAnswer, error) { return 0, answerQuit, nil }
	a.tourInput = func(string, string) (string, error) { return "", io.EOF }
	a.runStep = func(_ context.Context, argv []string) int {
		t.Errorf("quickstart gating test executed a step: %v", argv)
		return 0
	}
	code = a.run(append([]string{"quickstart"}, args...))
	return out.String(), errb.String(), code
}

// requireEmptyDir fails the test when dir holds any entry: the guide renderings
// execute nothing, so a cwd they ran in must stay untouched.
func requireEmptyDir(t *testing.T, dir string) {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("read %s: %v", dir, err)
	}
	if len(entries) != 0 {
		names := make([]string, 0, len(entries))
		for _, e := range entries {
			names = append(names, e.Name())
		}
		t.Fatalf("quickstart guide touched the filesystem: cwd now holds %v", names)
	}
}

// TestQuickstartRootVerb proves `iris quickstart` is the third root verb beside
// update/uninstall: the tree stays nine nouns plus three root verbs, and the
// verb is a runnable daemonless leaf (a bare invocation is valid, never a group
// stub's usage error).
func TestQuickstartRootVerb(t *testing.T) {
	// spec: S08/quickstart-root-verb
	t.Run("S08/quickstart-root-verb", func(t *testing.T) {
		root := testRoot()

		// Top level: the nine nouns plus exactly the three root verbs.
		wantTop := append(mapKeys(wantTree), "update", "uninstall", "quickstart")
		assertSetEqual(t, "top-level nouns and root verbs", childNames(root), wantTop)

		qs := find(root, "quickstart")
		if qs == nil {
			t.Fatal("quickstart root verb missing from the tree")
		}
		if !isLeafCommand(qs) {
			t.Errorf("quickstart owns subcommands %v; it is a flat root verb, not a noun", childNames(qs))
		}
		if life := qs.Annotations[lifecycleAnnotation]; life != lifecycleDaemonless {
			t.Errorf("quickstart lifecycle annotation = %q, want %q (the tour runs before any engine exists)", life, lifecycleDaemonless)
		}

		// A bare piped invocation is valid: exit 0 with the guide, not exit 2.
		out, _, code := runQuickstart(t, false, false)
		if code != exitOK {
			t.Fatalf("bare `iris quickstart` exit = %d, want %d (a runnable verb, not a group stub)", code, exitOK)
		}
		if out == "" {
			t.Error("bare `iris quickstart` printed nothing")
		}
	})
}

// TestQuickstartTTYGating proves the interactivity gate: the interactive tour
// only when stdin AND stdout are both interactive terminals and --json is off;
// any other invocation gets the plain guide (or, under --json, the step-list
// envelope). Every rendering exits 0.
func TestQuickstartTTYGating(t *testing.T) {
	unsetNoColor(t)
	// spec: S08/quickstart-tty-gating
	t.Run("S08/quickstart-tty-gating", func(t *testing.T) {
		cases := []struct {
			name            string
			stdoutTTY       bool
			stdinTTY        bool
			jsonMode        bool
			wantInteractive bool
		}{
			{"both terminals -> interactive", true, true, false, true},
			{"stdout piped -> plain guide", false, true, false, false},
			{"stdin piped -> plain guide", true, false, false, false},
			{"both piped -> plain guide", false, false, false, false},
			{"--json beats both terminals", true, true, true, false},
		}
		for _, tc := range cases {
			t.Run(tc.name, func(t *testing.T) {
				var args []string
				if tc.jsonMode {
					args = append(args, "--json")
				}
				out, errb, code := runQuickstart(t, tc.stdoutTTY, tc.stdinTTY, args...)
				if code != exitOK {
					t.Fatalf("exit = %d, want %d\nstderr: %s", code, exitOK, errb)
				}
				if gotInteractive := strings.Contains(out, "Welcome to iris"); gotInteractive != tc.wantInteractive {
					t.Errorf("interactive rendering = %v, want %v\nstdout: %q", gotInteractive, tc.wantInteractive, out)
				}
				switch {
				case tc.jsonMode:
					if !looksJSON([]byte(out)) {
						t.Errorf("--json did not render the envelope: %q", out)
					}
				case !tc.wantInteractive:
					if !strings.Contains(out, "1. ") || !strings.Contains(out, "iris engine install") {
						t.Errorf("non-interactive rendering is not the numbered guide: %q", out)
					}
				}
			})
		}
	})
}

// TestQuickstartCeremonyColorGating proves color follows the ceremony rule
// while never gating interactivity: NO_COLOR strips every ANSI escape from the
// interactive tour but leaves it interactive, and piped or --json output never
// carries an escape.
func TestQuickstartCeremonyColorGating(t *testing.T) {
	// spec: S08/quickstart-ceremony-color-gating
	t.Run("S08/quickstart-ceremony-color-gating", func(t *testing.T) {
		t.Run("NO_COLOR strips paint, keeps interactivity", func(t *testing.T) {
			t.Setenv("NO_COLOR", "1")
			out, _, code := runQuickstart(t, true, true)
			if code != exitOK {
				t.Fatalf("exit = %d, want %d", code, exitOK)
			}
			if !strings.Contains(out, "Welcome to iris") {
				t.Errorf("NO_COLOR disabled interactivity: %q", out)
			}
			assertNoEsc(t, out)
		})
		t.Run("interactive terminal paints", func(t *testing.T) {
			unsetNoColor(t)
			out, _, code := runQuickstart(t, true, true)
			if code != exitOK {
				t.Fatalf("exit = %d, want %d", code, exitOK)
			}
			if !strings.Contains(out, esc) {
				t.Errorf("interactive tour on a terminal emitted no escape: %q", out)
			}
		})
		t.Run("piped guide carries no escape", func(t *testing.T) {
			unsetNoColor(t)
			out, _, code := runQuickstart(t, false, false)
			if code != exitOK {
				t.Fatalf("exit = %d, want %d", code, exitOK)
			}
			assertNoEsc(t, out)
		})
		t.Run("--json carries no escape", func(t *testing.T) {
			unsetNoColor(t)
			out, _, code := runQuickstart(t, true, true, "--json")
			if code != exitOK {
				t.Fatalf("exit = %d, want %d", code, exitOK)
			}
			assertNoEsc(t, out)
		})
	})
}

// TestQuickstartPlainGuideWhenPiped proves a non-TTY invocation prints the
// complete numbered copy-paste guide -- byte-stable plain text pinned by a
// golden file -- executes nothing, and exits 0.
func TestQuickstartPlainGuideWhenPiped(t *testing.T) {
	clearTargetEnv(t)
	unsetNoColor(t)
	// spec: S08/quickstart-plain-guide-when-piped
	t.Run("S08/quickstart-plain-guide-when-piped", func(t *testing.T) {
		// Resolve the golden path before leaving the package directory.
		pkgDir, err := os.Getwd()
		if err != nil {
			t.Fatalf("getwd: %v", err)
		}
		goldenPath := filepath.Join(pkgDir, "testdata", "quickstart_guide.txt")

		// Run from an empty scratch cwd: the guide must not touch it.
		scratch := t.TempDir()
		t.Chdir(scratch)

		out, errb, code := runQuickstart(t, false, false)
		if code != exitOK {
			t.Fatalf("exit = %d, want %d\nstderr: %s", code, exitOK, errb)
		}
		if errb != "" {
			t.Errorf("plain guide wrote to stderr: %q", errb)
		}
		golden.Assert(t, []byte(out), goldenPath)
		requireEmptyDir(t, scratch)

		// Byte-stable: a second run renders the identical bytes.
		again, _, code := runQuickstart(t, false, false)
		if code != exitOK || again != out {
			t.Errorf("guide is not byte-stable across runs (exit %d)", code)
		}
	})
}

// quickstartEnvelope mirrors the --json data envelope of `iris quickstart`: the
// ordered step list of the tour, each step carrying its act, plus the additive
// catalog object (default, selected, entries).
type quickstartEnvelope struct {
	Data struct {
		Steps []struct {
			ID          string   `json:"id"`
			Explanation string   `json:"explanation"`
			Argv        []string `json:"argv"`
			Act         string   `json:"act"`
		} `json:"steps"`
		Catalog struct {
			Default  string `json:"default"`
			Selected string `json:"selected"`
			Entries  []struct {
				ID string `json:"id"`
			} `json:"entries"`
		} `json:"catalog"`
	} `json:"data"`
}

// TestQuickstartJSONGuideEnvelope proves --json emits exactly one data envelope
// carrying the ordered step list (id, explanation, argv, act) and executes
// nothing, even when both streams are interactive terminals.
func TestQuickstartJSONGuideEnvelope(t *testing.T) {
	clearTargetEnv(t)
	unsetNoColor(t)
	// spec: S08/quickstart-json-guide-envelope
	t.Run("S08/quickstart-json-guide-envelope", func(t *testing.T) {
		scratch := t.TempDir()
		t.Chdir(scratch)

		out, errb, code := runQuickstart(t, true, true, "--json")
		if code != exitOK {
			t.Fatalf("exit = %d, want %d\nstderr: %s", code, exitOK, errb)
		}
		assertNoEsc(t, out)

		var env quickstartEnvelope
		decodeSingleJSON(t, []byte(out), &env)

		wantOrder := []string{"install", "start", "info", "apply", "run", "provenance"}
		wantActs := []string{"engine", "engine", "engine", "pipeline", "pipeline", "pipeline"}
		if len(env.Data.Steps) != len(wantOrder) {
			t.Fatalf("envelope carries %d steps, want %d: %q", len(env.Data.Steps), len(wantOrder), out)
		}
		for i, step := range env.Data.Steps {
			if step.ID != wantOrder[i] {
				t.Errorf("step[%d].id = %q, want %q (tour order)", i, step.ID, wantOrder[i])
			}
			if step.Act != wantActs[i] {
				t.Errorf("step[%d].act = %q, want %q (the additive act field)", i, step.Act, wantActs[i])
			}
			if step.Explanation == "" {
				t.Errorf("step %q carries no explanation", step.ID)
			}
			if len(step.Argv) == 0 || step.Argv[0] != "iris" {
				t.Errorf("step %q argv = %v, want an iris command vector", step.ID, step.Argv)
			}
		}

		// The additive catalog object rides the same envelope.
		if env.Data.Catalog.Default != "hello_iris" || env.Data.Catalog.Selected != "hello_iris" {
			t.Errorf("catalog default/selected = %q/%q, want hello_iris/hello_iris (no --pipeline)",
				env.Data.Catalog.Default, env.Data.Catalog.Selected)
		}
		if len(env.Data.Catalog.Entries) == 0 || env.Data.Catalog.Entries[0].ID != "hello_iris" {
			t.Errorf("catalog entries missing or misordered: %+v", env.Data.Catalog.Entries)
		}

		// Executes nothing: the scratch cwd stays untouched.
		requireEmptyDir(t, scratch)
	})
}
