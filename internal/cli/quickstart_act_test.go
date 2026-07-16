package cli

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"unicode/utf8"
)

// mustGetwd returns the process working directory or fails the test.
func mustGetwd(t *testing.T) string {
	t.Helper()
	wd, err := os.Getwd()
	if err != nil {
		t.Fatalf("getwd: %v", err)
	}
	return wd
}

// requireSameDir fails the test unless got and want name the same directory
// once symlinks are resolved (t.TempDir on macOS hands out /var/folders paths
// that Getwd reports under /private).
func requireSameDir(t *testing.T, got, want string) {
	t.Helper()
	rg, err := filepath.EvalSymlinks(got)
	if err != nil {
		t.Fatalf("resolve %s: %v", got, err)
	}
	rw, err := filepath.EvalSymlinks(want)
	if err != nil {
		t.Fatalf("resolve %s: %v", want, err)
	}
	if rg != rw {
		t.Fatalf("directory = %s, want %s", got, want)
	}
}

// TestQuickstartActStructure proves the chaptered tour: an ENGINE then a
// PIPELINE chapter mark (rule-and-title artwork, TTY-only), steps grouped and
// ordered within their acts, consent per act rather than per step, and the
// first failing step stopping the tour with that command's own exit category.
func TestQuickstartActStructure(t *testing.T) {
	clearTargetEnv(t)
	unsetNoColor(t)
	t.Run("quickstart-act-structure", func(t *testing.T) {
		t.Run("marks frame the acts; one consent per act; steps run straight through", func(t *testing.T) {
			chdirWorkspace(t)
			var out, errb bytes.Buffer
			a := tourApp(&out, &errb, true)
			events := scriptTour(a, proceeds(1), nil) // exactly one consent: THE PIPELINE's pick

			code := a.run([]string{"quickstart"})
			if code != exitOK {
				t.Fatalf("exit = %d, want %d\nstdout: %s\nstderr: %s", code, exitOK, out.String(), errb.String())
			}

			// Event order: the workspace question opens THE ENGINE, its steps run
			// straight through with no further prompt, one pick opens THE PIPELINE,
			// then its steps run straight through.
			all := canonicalStepArgvs()
			want := []string{"input " + workspacePromptFor("~/.iris/workspace")}
			for _, argv := range all[:3] {
				want = append(want, "step "+argv)
			}
			want = append(want, "pick "+wantPickQuestion)
			for _, argv := range all[3:] {
				want = append(want, "step "+argv)
			}
			if got := *events; !equalStrings(got, want) {
				t.Errorf("tour event order:\n got %q\nwant %q", got, want)
			}

			// The one chapter mark renders as a 48-column light rule; the shop follows the act.
			plain := stripANSI(out.String())
			engineAt := strings.Index(plain, "── THE ENGINE ")
			if engineAt < 0 {
				t.Fatalf("ENGINE chapter mark missing:\n%s", plain)
			}
			for _, line := range strings.Split(plain, "\n") {
				rule := strings.TrimPrefix(line, "  ")
				if strings.HasPrefix(rule, "── THE ") {
					if w := utf8.RuneCountInString(rule); w != 48 {
						t.Errorf("chapter rule %q is %d columns, want 48", rule, w)
					}
				}
			}
			if !strings.Contains(out.String(), ansiCyan+"── THE ENGINE ") {
				t.Errorf("ENGINE mark is not painted cyan:\n%q", out.String())
			}

		})

		t.Run("marks are TTY-only; a piped tour still names its acts in plain text", func(t *testing.T) {
			t.Chdir(t.TempDir())
			var out, errb bytes.Buffer
			a := tourApp(&out, &errb, false)
			_ = scriptTour(a, nil, nil)

			code := a.run([]string{"quickstart", "--yes"})
			if code != exitOK {
				t.Fatalf("exit = %d, want %d\nstderr: %s", code, exitOK, errb.String())
			}
			assertNoEsc(t, out.String())
			if strings.Contains(out.String(), "──") {
				t.Errorf("rule artwork reached a pipe:\n%q", out.String())
			}
			if !strings.Contains(out.String(), "THE ENGINE") || !strings.Contains(out.String(), "THE PIPELINE") {
				t.Errorf("piped tour does not name its acts:\n%q", out.String())
			}
		})

		t.Run("first failing step stops the tour with its category, before the next act", func(t *testing.T) {
			chdirWorkspace(t)
			var out, errb bytes.Buffer
			a := tourApp(&out, &errb, true)
			events := scriptTour(a, proceeds(1), map[string]int{"engine start": 4})

			code := a.run([]string{"quickstart"})
			if code != exitOpFailed {
				t.Fatalf("exit = %d, want %d (the failing step's own category)\nstderr: %s", code, exitOpFailed, errb.String())
			}
			if got, want := stepEvents(*events), canonicalStepArgvs()[:2]; !equalStrings(got, want) {
				t.Errorf("steps past the failure executed:\n got %q\nwant %q", got, want)
			}
			if picks := pickEvents(*events); len(picks) != 0 {
				t.Errorf("a failed ENGINE act still offered the next act's pick: %q", picks)
			}
			if !strings.Contains(strings.ToLower(errb.String()), strings.ToLower(wantResumeHint)) {
				t.Errorf("failure carries no resume hint on stderr: %q", errb.String())
			}
		})
	})
}

// TestQuickstartWorkspacePrompt proves the ENGINE act's opening question:
// `Pipeline workspace [~/.iris/workspace]:` with the engine home's workspace
// directory as the visible default (never the invoking directory, however
// workspace-like), the empty answer accepting it, `~` expanding to the
// operator's home, mkdir -p plus chdir, and --yes using the invoking
// directory unprompted.
func TestQuickstartWorkspacePrompt(t *testing.T) {
	clearTargetEnv(t)
	unsetNoColor(t)
	t.Run("quickstart-workspace-prompt", func(t *testing.T) {
		t.Run("empty answer accepts the ~/.iris/workspace default: created, entered, announced", func(t *testing.T) {
			home := t.TempDir()
			t.Setenv("HOME", home)
			t.Setenv("IRIS_HOME", filepath.Join(home, ".iris"))
			start := t.TempDir()
			t.Chdir(start)
			var out, errb bytes.Buffer
			a := tourApp(&out, &errb, true)
			events := scriptTour(a, proceeds(1), nil) // input answers "" = accept the default

			code := a.run([]string{"quickstart"})
			if code != exitOK {
				t.Fatalf("exit = %d, want %d\nstdout: %s\nstderr: %s", code, exitOK, out.String(), errb.String())
			}
			if got := inputEvents(*events); len(got) != 1 || got[0] != workspacePromptFor("~/.iris/workspace") {
				t.Errorf("workspace question = %q, want exactly [%q] (the visible default)", got, workspacePromptFor("~/.iris/workspace"))
			}
			want := filepath.Join(home, ".iris", "workspace")
			requireSameDir(t, mustGetwd(t), want)
			if !strings.Contains(stripANSI(out.String()), "✓ workspace "+want) {
				t.Errorf("workspace not announced (want %q):\n%s", "✓ workspace "+want, stripANSI(out.String()))
			}
			// The sample landed in the chosen workspace, not the invoking directory.
			if _, err := os.Stat(filepath.Join(want, "pipelines", "hello_iris", "iris-declare.yaml")); err != nil {
				t.Errorf("sample not materialized into the workspace: %v", err)
			}
			requireEmptyDir(t, start)
		})

		t.Run("a typed ~ path expands to the operator's home, mkdir -p deep", func(t *testing.T) {
			home := t.TempDir()
			t.Setenv("HOME", home)
			t.Chdir(t.TempDir())
			var out, errb bytes.Buffer
			a := tourApp(&out, &errb, true)
			_ = scriptTour(a, proceeds(1), nil)
			a.tourInput = func(string, string) (string, error) { return "~/lab/deep/iris", nil }

			code := a.run([]string{"quickstart"})
			if code != exitOK {
				t.Fatalf("exit = %d, want %d\nstderr: %s", code, exitOK, errb.String())
			}
			requireSameDir(t, mustGetwd(t), filepath.Join(home, "lab", "deep", "iris"))
		})

		t.Run("a workspace-like cwd is never proposed as the default", func(t *testing.T) {
			home := t.TempDir()
			t.Setenv("HOME", home)
			t.Setenv("IRIS_HOME", filepath.Join(home, ".iris"))
			// A workspace-like invoking directory OUTSIDE the home: it has a
			// pipelines/ tree (as the iris source checkout does) but is not the
			// engine home's workspace.
			start := t.TempDir()
			if err := os.Mkdir(filepath.Join(start, "pipelines"), 0o755); err != nil {
				t.Fatalf("create pipelines dir: %v", err)
			}
			t.Chdir(start)
			var out, errb bytes.Buffer
			a := tourApp(&out, &errb, true)
			events := scriptTour(a, proceeds(1), nil)

			code := a.run([]string{"quickstart"})
			if code != exitOK {
				t.Fatalf("exit = %d, want %d\nstderr: %s", code, exitOK, errb.String())
			}
			// The invoking directory has a pipelines/ tree (as the iris source
			// checkout does), yet the default stays the engine home's workspace:
			// the tour never adopts the invoking directory as the workspace answer.
			if got := inputEvents(*events); len(got) != 1 || got[0] != workspacePromptFor("~/.iris/workspace") {
				t.Errorf("workspace question = %q, want [%q] (fixed default, never the cwd)", got, workspacePromptFor("~/.iris/workspace"))
			}
			requireSameDir(t, mustGetwd(t), filepath.Join(home, ".iris", "workspace"))
		})

		t.Run("--yes never prompts and uses the invoking directory unchanged", func(t *testing.T) {
			dir := t.TempDir()
			t.Chdir(dir)
			wd := mustGetwd(t)
			var out, errb bytes.Buffer
			a := tourApp(&out, &errb, false)
			events := scriptTour(a, nil, nil)

			code := a.run([]string{"quickstart", "--yes"})
			if code != exitOK {
				t.Fatalf("exit = %d, want %d\nstderr: %s", code, exitOK, errb.String())
			}
			if got := inputEvents(*events); len(got) != 0 {
				t.Errorf("--yes still asked the workspace question: %q", got)
			}
			requireSameDir(t, mustGetwd(t), wd)
			if _, err := os.Stat(filepath.Join(dir, "pipelines", "hello_iris", "iris-declare.yaml")); err != nil {
				t.Errorf("--yes did not treat the invoking directory as the workspace: %v", err)
			}
		})

		t.Run("~user is refused with a clear fault", func(t *testing.T) {
			t.Chdir(t.TempDir())
			var out, errb bytes.Buffer
			a := tourApp(&out, &errb, true)
			events := scriptTour(a, proceeds(1), nil)
			a.tourInput = func(string, string) (string, error) { return "~somebody/iris", nil }

			code := a.run([]string{"quickstart"})
			if code != exitOpFailed {
				t.Fatalf("exit = %d, want %d\nstderr: %s", code, exitOpFailed, errb.String())
			}
			if !strings.Contains(errb.String(), "~somebody") {
				t.Errorf("fault does not name the rejected ~user path: %q", errb.String())
			}
			if steps := stepEvents(*events); len(steps) != 0 {
				t.Errorf("steps executed despite the workspace fault: %q", steps)
			}
		})

		t.Run("q at the workspace question aborts clean", func(t *testing.T) {
			scratch := t.TempDir()
			t.Chdir(scratch)
			var out, errb bytes.Buffer
			a := tourApp(&out, &errb, true)
			events := scriptTour(a, proceeds(1), nil)
			a.tourInput = func(string, string) (string, error) { return "q", nil }

			code := a.run([]string{"quickstart"})
			if code != exitOK {
				t.Fatalf("exit = %d, want %d (a decline is never a failure)\nstderr: %s", code, exitOK, errb.String())
			}
			if !strings.Contains(out.String(), wantResumeHint) {
				t.Errorf("abort carries no resume hint\nstdout: %s", out.String())
			}
			if steps := stepEvents(*events); len(steps) != 0 {
				t.Errorf("steps executed after the decline: %q", steps)
			}
			requireEmptyDir(t, scratch)
		})
	})
}

// TestQuickstartFromInstallerContinuation proves the installer's continuation
// entry: --from-installer opens directly on the ENGINE chapter (no welcome —
// the installer's banner was the welcome, its Y/n the act's consent) and is
// otherwise the same tour; combined with --json it stays the inert step-list
// envelope, exit 0 — the version-probe guarantee install.sh relies on.
func TestQuickstartFromInstallerContinuation(t *testing.T) {
	clearTargetEnv(t)
	unsetNoColor(t)
	t.Run("quickstart-from-installer-continuation", func(t *testing.T) {
		t.Run("opens on the ENGINE chapter: no welcome, straight to the workspace question", func(t *testing.T) {
			chdirWorkspace(t)
			var out, errb bytes.Buffer
			a := tourApp(&out, &errb, true)
			events := scriptTour(a, proceeds(1), nil)

			code := a.run([]string{"quickstart", "--from-installer"})
			if code != exitOK {
				t.Fatalf("exit = %d, want %d\nstdout: %s\nstderr: %s", code, exitOK, out.String(), errb.String())
			}
			if strings.Contains(out.String(), "Welcome to iris") {
				t.Errorf("--from-installer repeated the welcome (the installer's banner was the welcome):\n%s", out.String())
			}
			if !strings.Contains(stripANSI(out.String()), "── THE ENGINE ") {
				t.Errorf("--from-installer did not open on the ENGINE chapter mark:\n%s", stripANSI(out.String()))
			}
			if got := *events; len(got) == 0 || got[0] != "input "+workspacePromptFor("~/.iris/workspace") {
				t.Errorf("first interaction = %q, want the workspace question %q", got, workspacePromptFor("~/.iris/workspace"))
			}
			if got := stepEvents(*events); !equalStrings(got, canonicalStepArgvs()) {
				t.Errorf("continuation did not run the full tour:\n got %q\nwant %q", got, canonicalStepArgvs())
			}
			// Otherwise the same tour: THE PIPELINE still asks its own pick.
			if picks := pickEvents(*events); len(picks) != 1 || picks[0] != wantPickQuestion {
				t.Errorf("act picks = %q, want exactly [%q]", picks, wantPickQuestion)
			}
		})

		t.Run("--from-installer --json stays the inert envelope, exit 0 (the version probe)", func(t *testing.T) {
			scratch := t.TempDir()
			t.Chdir(scratch)
			var out, errb bytes.Buffer
			a := tourApp(&out, &errb, true)
			events := scriptTour(a, nil, nil)

			code := a.run([]string{"quickstart", "--from-installer", "--json"})
			if code != exitOK {
				t.Fatalf("exit = %d, want %d (install.sh probes exactly this invocation)\nstderr: %s", code, exitOK, errb.String())
			}
			if !looksJSON(out.Bytes()) {
				t.Errorf("probe invocation did not render the envelope: %q", out.String())
			}
			assertNoEsc(t, out.String())
			if len(*events) != 0 {
				t.Errorf("probe invocation prompted or executed: %v", *events)
			}
			requireEmptyDir(t, scratch)
		})

		t.Run("piped without --yes renders the plain guide, executing nothing", func(t *testing.T) {
			scratch := t.TempDir()
			t.Chdir(scratch)
			var out, errb bytes.Buffer
			a := tourApp(&out, &errb, false)
			events := scriptTour(a, nil, nil)

			code := a.run([]string{"quickstart", "--from-installer"})
			if code != exitOK {
				t.Fatalf("exit = %d, want %d\nstderr: %s", code, exitOK, errb.String())
			}
			if !strings.Contains(out.String(), "1. ") || !strings.Contains(out.String(), "iris engine install") {
				t.Errorf("piped continuation is not the plain guide: %q", out.String())
			}
			if len(*events) != 0 {
				t.Errorf("piped continuation prompted or executed: %v", *events)
			}
			requireEmptyDir(t, scratch)
		})
	})
}
