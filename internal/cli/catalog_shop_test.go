package cli

import (
	"bytes"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// wantPickQuestion is the pinned shop prompt for the three embedded entries.
const wantPickQuestion = "Pick a pipeline (1-3, Enter=1):"

// pickEvents filters the shop-pick entries out of a tour event log, stripping
// the "pick " tag.
func pickEvents(events []string) []string {
	var out []string
	for _, e := range events {
		if rest, ok := strings.CutPrefix(e, "pick "); ok {
			out = append(out, rest)
		}
	}
	return out
}

// pickEntry overrides the scripted tourPick to answer the shop with one fixed
// choice.
func pickEntry(a *app, events *[]string, choice int) {
	a.tourPick = func(question string, _ int) (int, promptAnswer, error) {
		*events = append(*events, "pick "+question)
		return choice, answerProceed, nil
	}
}

// mustCatalog loads the embedded catalog or fails the test.
func mustCatalog(t *testing.T) *pipelineCatalog {
	t.Helper()
	cat, err := loadCatalog()
	if err != nil {
		t.Fatalf("loadCatalog: %v", err)
	}
	return cat
}

// TestQuickstartCatalogBrowseRender proves the shop inside THE PIPELINE act:
// the numbered name-and-pitch list painted (number and name cyan, pitch dim),
// the pinned pick prompt, the empty answer taking entry 1, the picked entry's
// description and finale preview rendering before the apply step, and any
// invalid answer aborting clean -- a typo never runs a command.
func TestQuickstartCatalogBrowseRender(t *testing.T) {
	clearTargetEnv(t)
	unsetNoColor(t)
	// spec: S08/quickstart-catalog-browse-render
	t.Run("S08/quickstart-catalog-browse-render", func(t *testing.T) {
		t.Run("the numbered shop paints names and pitches, then asks the pinned pick", func(t *testing.T) {
			chdirWorkspace(t)
			var out, errb bytes.Buffer
			a := tourApp(&out, &errb, true)
			events := scriptTour(a, proceeds(1), nil)

			code := a.run([]string{"quickstart"})
			if code != exitOK {
				t.Fatalf("exit = %d, want %d\nstdout: %s\nstderr: %s", code, exitOK, out.String(), errb.String())
			}
			if picks := pickEvents(*events); len(picks) != 1 || picks[0] != wantPickQuestion {
				t.Fatalf("pick questions = %q, want exactly [%q]", picks, wantPickQuestion)
			}

			plain := stripANSI(out.String())
			cat := mustCatalog(t)
			for i, e := range cat.Entries {
				if !strings.Contains(plain, e.Name) || !strings.Contains(plain, e.Pitch) {
					t.Errorf("shop does not list entry %d (%s — %s):\n%s", i+1, e.Name, e.Pitch, plain)
				}
			}
			if !strings.Contains(out.String(), ansiCyan) || !strings.Contains(out.String(), ansiDim) {
				t.Errorf("shop is not painted (cyan names, dim pitches):\n%q", out.String())
			}
			// The browse renders under THE PIPELINE's chapter mark.
			pipelineAt := strings.Index(plain, "── THE PIPELINE ")
			shopAt := strings.Index(plain, cat.Entries[1].Name)
			if pipelineAt < 0 || shopAt < pipelineAt {
				t.Errorf("shop not under THE PIPELINE mark (mark %d, shop %d):\n%s", pipelineAt, shopAt, plain)
			}
		})

		t.Run("the empty answer takes entry 1 and its detail renders before apply", func(t *testing.T) {
			chdirWorkspace(t)
			var out, errb bytes.Buffer
			a := tourApp(&out, &errb, true)
			events := scriptTour(a, proceeds(1), nil) // scripted pick: the default, entry 1

			code := a.run([]string{"quickstart"})
			if code != exitOK {
				t.Fatalf("exit = %d, want %d\nstderr: %s", code, exitOK, errb.String())
			}
			def := mustCatalog(t).defaultEntry()
			plain := stripANSI(out.String())
			finale := "Finale: iris data provenance " + def.Showcase.Table + " " + def.Showcase.PK
			finaleAt := strings.Index(plain, finale)
			applyAt := strings.Index(plain, "iris declare apply pipelines/"+def.ID)
			if finaleAt < 0 {
				t.Fatalf("picked entry's finale preview missing (want %q):\n%s", finale, plain)
			}
			if applyAt < 0 || finaleAt > applyAt {
				t.Errorf("detail did not render before the apply step (finale %d, apply %d)", finaleAt, applyAt)
			}
			if !strings.Contains(plain, strings.TrimSpace(strings.Split(def.Description, "\n")[0])) {
				t.Errorf("picked entry's description missing:\n%s", plain)
			}
			if got := stepEvents(*events); len(got) != 6 || !strings.HasPrefix(got[3], "declare apply pipelines/"+def.ID) {
				t.Errorf("empty answer did not drive the default entry's steps: %q", got)
			}
		})

		t.Run("an invalid pick aborts clean: exit 0, resume hint, nothing run", func(t *testing.T) {
			for _, tc := range []struct{ name, answer string }{
				{"non-numeric", "banana"},
				{"out of range", "9"},
				{"zero", "0"},
				{"negative", "-1"},
			} {
				t.Run(tc.name, func(t *testing.T) {
					choice, ans := parsePickAnswer(tc.answer, 3)
					if ans != answerQuit {
						t.Errorf("parsePickAnswer(%q, 3) = (%d, %v), want a quit (a typo never runs a command)", tc.answer, choice, ans)
					}
				})
			}
			for _, tc := range []struct {
				name   string
				answer string
				want   int
			}{
				{"empty is entry 1", "", 1},
				{"in range", "3", 3},
				{"whitespace trimmed", " 2 ", 2},
			} {
				t.Run(tc.name, func(t *testing.T) {
					choice, ans := parsePickAnswer(tc.answer, 3)
					if ans != answerProceed || choice != tc.want {
						t.Errorf("parsePickAnswer(%q, 3) = (%d, %v), want (%d, proceed)", tc.answer, choice, ans, tc.want)
					}
				})
			}

			// Through the sequencer: a quit at the pick is the clean abort.
			chdirWorkspace(t)
			var out, errb bytes.Buffer
			a := tourApp(&out, &errb, true)
			events := scriptTour(a, nil, nil) // answers exhausted: the pick quits
			code := a.run([]string{"quickstart"})
			if code != exitOK {
				t.Fatalf("exit = %d, want %d (an invalid pick is a decline, never a failure)\nstderr: %s", code, exitOK, errb.String())
			}
			if !strings.Contains(out.String(), wantResumeHint) {
				t.Errorf("abort carries no resume hint\nstdout: %s", out.String())
			}
			if got, want := stepEvents(*events), canonicalStepArgvs()[:3]; !equalStrings(got, want) {
				t.Errorf("steps past the declined pick executed:\n got %q\nwant %q (the ENGINE act only)", got, want)
			}
		})

		t.Run("EOF at the pick aborts clean", func(t *testing.T) {
			chdirWorkspace(t)
			var out, errb bytes.Buffer
			a := tourApp(&out, &errb, true)
			events := scriptTour(a, nil, nil)
			a.tourPick = func(string, int) (int, promptAnswer, error) { return 0, answerQuit, io.EOF }

			code := a.run([]string{"quickstart"})
			if code != exitOK {
				t.Fatalf("exit = %d, want %d (EOF is a clean abort)\nstderr: %s", code, exitOK, errb.String())
			}
			if !strings.Contains(out.String(), wantResumeHint) {
				t.Errorf("EOF abort carries no resume hint\nstdout: %s", out.String())
			}
			if got, want := stepEvents(*events), canonicalStepArgvs()[:3]; !equalStrings(got, want) {
				t.Errorf("steps past the EOF pick executed:\n got %q\nwant %q", got, want)
			}
		})
	})
}

// TestQuickstartCatalogPickMaterializeRun proves a non-default pick drives the
// whole act with that entry alone: only its files materialize, the
// apply/run/provenance argvs carry its id and showcase, the wrap-up names it,
// and the dead-letter lesson (exit 5) names the picked pipeline.
func TestQuickstartCatalogPickMaterializeRun(t *testing.T) {
	clearTargetEnv(t)
	unsetNoColor(t)
	// spec: S08/quickstart-catalog-pick-materialize-run
	t.Run("S08/quickstart-catalog-pick-materialize-run", func(t *testing.T) {
		t.Run("picking word_frequency runs word_frequency only", func(t *testing.T) {
			dir := chdirWorkspace(t)
			var out, errb bytes.Buffer
			a := tourApp(&out, &errb, true)
			events := scriptTour(a, nil, nil)
			pickEntry(a, events, 3) // word_frequency is entry 3

			code := a.run([]string{"quickstart"})
			if code != exitOK {
				t.Fatalf("exit = %d, want %d\nstdout: %s\nstderr: %s", code, exitOK, out.String(), errb.String())
			}

			steps := stepEvents(*events)
			if len(steps) != 6 {
				t.Fatalf("tour executed %d steps %q, want 6", len(steps), steps)
			}
			for i, prefix := range []string{
				"declare apply pipelines/word_frequency",
				"pipeline run word_frequency",
				"data provenance demo.word_counts hope",
			} {
				if !strings.HasPrefix(steps[3+i], prefix) {
					t.Errorf("step[%d] = %q, want prefix %q (the picked entry's argv)", 3+i, steps[3+i], prefix)
				}
			}

			// Only the picked entry's files landed in the workspace.
			if _, err := os.Stat(filepath.Join(dir, "pipelines", "word_frequency", "iris-declare.yaml")); err != nil {
				t.Errorf("picked entry not materialized: %v", err)
			}
			for _, other := range []string{"hello_iris", "system_snapshot"} {
				if _, err := os.Stat(filepath.Join(dir, "pipelines", other)); !os.IsNotExist(err) {
					t.Errorf("unpicked entry %s materialized (err %v); only the pick lands", other, err)
				}
			}

			// The wrap-up cheat-sheet is parameterized by the pick.
			plain := stripANSI(out.String())
			for _, want := range []string{"iris pipeline run word_frequency", "iris data provenance demo.word_counts hope"} {
				if !strings.Contains(plain, want) {
					t.Errorf("wrap-up does not carry the picked entry's command %q:\n%s", want, plain)
				}
			}
		})

		t.Run("the dead-letter lesson names the picked pipeline", func(t *testing.T) {
			chdirWorkspace(t)
			var out, errb bytes.Buffer
			a := tourApp(&out, &errb, true)
			events := scriptTour(a, nil, map[string]int{"pipeline run word_frequency": exitDeadLettered})
			pickEntry(a, events, 3)

			code := a.run([]string{"quickstart"})
			if code != exitDeadLettered {
				t.Fatalf("exit = %d, want %d (the failing step's own category)\nstderr: %s", code, exitDeadLettered, errb.String())
			}
			if e := errb.String(); !strings.Contains(e, "iris deadletter show word_frequency") || !strings.Contains(e, "iris deadletter replay word_frequency") {
				t.Errorf("dead-letter lesson does not name the picked pipeline:\n%q", e)
			}
		})
	})
}

// TestQuickstartCatalogPipelineFlag proves --pipeline <id> selects the entry
// explicitly in every rendering -- the interactive tour skips the browse but
// still prints the detail, --yes honors it unattended -- and an unknown id is
// a usage error (exit 2) naming the available ids, in every rendering.
func TestQuickstartCatalogPipelineFlag(t *testing.T) {
	clearTargetEnv(t)
	unsetNoColor(t)
	// spec: S08/quickstart-catalog-pipeline-flag
	t.Run("S08/quickstart-catalog-pipeline-flag", func(t *testing.T) {
		t.Run("interactive: browse skipped, detail still prints, steps carry the pick", func(t *testing.T) {
			chdirWorkspace(t)
			var out, errb bytes.Buffer
			a := tourApp(&out, &errb, true)
			events := scriptTour(a, nil, nil)

			code := a.run([]string{"quickstart", "--pipeline", "system_snapshot"})
			if code != exitOK {
				t.Fatalf("exit = %d, want %d\nstdout: %s\nstderr: %s", code, exitOK, out.String(), errb.String())
			}
			if picks := pickEvents(*events); len(picks) != 0 {
				t.Errorf("--pipeline still asked the shop pick: %q", picks)
			}
			plain := stripANSI(out.String())
			if !strings.Contains(plain, "Finale: iris data provenance demo.machine_facts hostname") {
				t.Errorf("explicit pick's detail did not render:\n%s", plain)
			}
			if steps := stepEvents(*events); len(steps) != 6 || !strings.HasPrefix(steps[3], "declare apply pipelines/system_snapshot") {
				t.Errorf("steps do not carry the explicit pick: %q", steps)
			}
			// The unpicked entries never paint their pitches: the browse was skipped.
			if pitch := mustCatalog(t).defaultEntry().Pitch; strings.Contains(plain, pitch) {
				t.Errorf("browse list rendered despite --pipeline:\n%s", plain)
			}
		})

		t.Run("--yes --pipeline runs the pick unattended", func(t *testing.T) {
			dir := t.TempDir()
			t.Chdir(dir)
			var out, errb bytes.Buffer
			a := tourApp(&out, &errb, false)
			events := scriptTour(a, nil, nil)

			code := a.run([]string{"quickstart", "--yes", "--pipeline", "word_frequency"})
			if code != exitOK {
				t.Fatalf("exit = %d, want %d\nstderr: %s", code, exitOK, errb.String())
			}
			if picks := pickEvents(*events); len(picks) != 0 {
				t.Errorf("--yes still asked the shop pick: %q", picks)
			}
			if steps := stepEvents(*events); len(steps) != 6 || !strings.HasPrefix(steps[5], "data provenance demo.word_counts hope") {
				t.Errorf("--yes --pipeline steps = %q, want the word_frequency tour", steps)
			}
			if _, err := os.Stat(filepath.Join(dir, "pipelines", "word_frequency", "main.sh")); err != nil {
				t.Errorf("--yes --pipeline did not materialize the pick: %v", err)
			}
		})

		t.Run("an unknown id is a usage error naming the available ids, every rendering", func(t *testing.T) {
			ids := make([]string, 0, 3)
			for _, e := range mustCatalog(t).Entries {
				ids = append(ids, e.ID)
			}
			for _, tc := range []struct {
				name string
				tty  bool
				args []string
			}{
				{"interactive tour", true, []string{"quickstart", "--pipeline", "nope"}},
				{"unattended", false, []string{"quickstart", "--yes", "--pipeline", "nope"}},
				{"plain guide", false, []string{"quickstart", "--pipeline", "nope"}},
				{"json envelope", true, []string{"quickstart", "--json", "--pipeline", "nope"}},
			} {
				t.Run(tc.name, func(t *testing.T) {
					scratch := t.TempDir()
					t.Chdir(scratch)
					var out, errb bytes.Buffer
					a := tourApp(&out, &errb, tc.tty)
					events := scriptTour(a, nil, nil)

					code := a.run(tc.args)
					if code != exitUsage {
						t.Fatalf("exit = %d, want %d\nstdout: %s\nstderr: %s", code, exitUsage, out.String(), errb.String())
					}
					msg := errb.String() + out.String()
					for _, id := range ids {
						if !strings.Contains(msg, id) {
							t.Errorf("unknown-id error does not name available id %q: %q", id, msg)
						}
					}
					if len(*events) != 0 {
						t.Errorf("refused quickstart still prompted or executed: %v", *events)
					}
					requireEmptyDir(t, scratch)
				})
			}
		})
	})
}

// TestQuickstartCatalogInGuides proves the inert renderings carry the catalog:
// the plain guide gains the catalog block and renders the selected entry's
// steps; the --json envelope gains catalog{default, selected, entries} beside
// the selected entry's steps -- both executing nothing.
func TestQuickstartCatalogInGuides(t *testing.T) {
	clearTargetEnv(t)
	unsetNoColor(t)
	// spec: S08/quickstart-catalog-in-guides
	t.Run("S08/quickstart-catalog-in-guides", func(t *testing.T) {
		t.Run("the plain guide lists the catalog and the selected entry's steps", func(t *testing.T) {
			scratch := t.TempDir()
			t.Chdir(scratch)
			out, errb, code := runQuickstart(t, false, false, "--pipeline", "word_frequency")
			if code != exitOK {
				t.Fatalf("exit = %d, want %d\nstderr: %s", code, exitOK, errb)
			}
			for _, e := range mustCatalog(t).Entries {
				if !strings.Contains(out, e.ID) || !strings.Contains(out, e.Pitch) {
					t.Errorf("plain guide misses catalog entry %s:\n%s", e.ID, out)
				}
			}
			for _, want := range []string{
				"iris declare apply pipelines/word_frequency",
				"iris pipeline run word_frequency",
				"iris data provenance demo.word_counts hope",
			} {
				if !strings.Contains(out, want) {
					t.Errorf("plain guide does not render the selected entry's step %q:\n%s", want, out)
				}
			}
			assertNoEsc(t, out)
			requireEmptyDir(t, scratch)
		})

		t.Run("the --json envelope carries catalog{default, selected, entries}", func(t *testing.T) {
			scratch := t.TempDir()
			t.Chdir(scratch)
			out, errb, code := runQuickstart(t, true, true, "--json", "--pipeline", "system_snapshot")
			if code != exitOK {
				t.Fatalf("exit = %d, want %d\nstderr: %s", code, exitOK, errb)
			}
			var env struct {
				Data struct {
					Steps []struct {
						ID   string   `json:"id"`
						Argv []string `json:"argv"`
					} `json:"steps"`
					Catalog struct {
						Default  string `json:"default"`
						Selected string `json:"selected"`
						Entries  []struct {
							ID       string `json:"id"`
							Name     string `json:"name"`
							Pitch    string `json:"pitch"`
							Showcase struct {
								Table string `json:"table"`
								PK    string `json:"pk"`
							} `json:"showcase"`
						} `json:"entries"`
					} `json:"catalog"`
				} `json:"data"`
			}
			if err := json.Unmarshal([]byte(out), &env); err != nil {
				t.Fatalf("envelope does not decode: %v\n%s", err, out)
			}
			c := env.Data.Catalog
			if c.Default != "hello_iris" || c.Selected != "system_snapshot" {
				t.Errorf("catalog default/selected = %q/%q, want hello_iris/system_snapshot", c.Default, c.Selected)
			}
			cat := mustCatalog(t)
			if len(c.Entries) != len(cat.Entries) {
				t.Fatalf("catalog carries %d entries, want %d", len(c.Entries), len(cat.Entries))
			}
			for i, e := range cat.Entries {
				got := c.Entries[i]
				if got.ID != e.ID || got.Name != e.Name || got.Pitch != e.Pitch ||
					got.Showcase.Table != e.Showcase.Table || got.Showcase.PK != e.Showcase.PK {
					t.Errorf("catalog entry[%d] = %+v, want %s's metadata", i, got, e.ID)
				}
			}
			// The steps are the selected entry's.
			last := env.Data.Steps[len(env.Data.Steps)-1]
			if want := []string{"iris", "data", "provenance", "demo.machine_facts", "hostname"}; !equalStringSlices(last.Argv, want) {
				t.Errorf("envelope finale argv = %v, want %v (the selected entry's showcase)", last.Argv, want)
			}
			requireEmptyDir(t, scratch)
		})

		t.Run("--yes without --pipeline selects entry 1", func(t *testing.T) {
			scratch := t.TempDir()
			t.Chdir(scratch)
			var out, errb bytes.Buffer
			a := tourApp(&out, &errb, false)
			events := scriptTour(a, nil, nil)
			code := a.run([]string{"quickstart", "--yes"})
			if code != exitOK {
				t.Fatalf("exit = %d, want %d\nstderr: %s", code, exitOK, errb.String())
			}
			if steps := stepEvents(*events); len(steps) != 6 || !strings.HasPrefix(steps[3], "declare apply pipelines/hello_iris") {
				t.Errorf("--yes did not take the default entry: %q", steps)
			}
			if picks := pickEvents(*events); len(picks) != 0 {
				t.Errorf("--yes asked the shop: %q", picks)
			}
		})
	})
}
