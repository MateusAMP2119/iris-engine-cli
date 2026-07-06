package declare_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/MateusAMP2119/iris-engine-cli/internal/declare"
	"github.com/MateusAMP2119/iris-engine-cli/internal/fixtures"
)

// makePipelineFolder builds a pipeline folder named name under a fresh temp
// root, writing each entry in files (a relative path -> contents map). A path
// ending in "/" creates an empty subdirectory (a would-be stage folder). It
// returns the absolute folder path.
func makePipelineFolder(t *testing.T, name string, files map[string]string) string {
	t.Helper()
	dir := filepath.Join(t.TempDir(), name)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("mkdir %s: %v", dir, err)
	}
	for rel, contents := range files {
		full := filepath.Join(dir, rel)
		if strings.HasSuffix(rel, "/") {
			if err := os.MkdirAll(full, 0o755); err != nil {
				t.Fatalf("mkdir %s: %v", full, err)
			}
			continue
		}
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			t.Fatalf("mkdir parent of %s: %v", full, err)
		}
		if err := os.WriteFile(full, []byte(contents), 0o644); err != nil {
			t.Fatalf("write %s: %v", full, err)
		}
	}
	return dir
}

const validPipelineYAML = "name: %s\nrun: [python, main.py]\n"

// TestPipelineFolderShape proves that a pipeline folder is accepted only as a
// folder named for the pipeline holding iris-declare.yaml plus exactly one
// script, with no internal stage structure.
func TestPipelineFolderShape(t *testing.T) {
	t.Run("S01/pipeline-folder-shape", func(t *testing.T) {
		// Accept: the golden extract_orders folder (iris-declare.yaml + main.py).
		pf, err := declare.ValidatePipelineFolder(
			filepath.Join(fixtures.WorkspaceGolden(), "pipelines", "ingest", "extract_orders"))
		if err != nil {
			t.Fatalf("golden extract_orders folder rejected: %v", err)
		}
		if pf.Script != "main.py" {
			t.Errorf("Script = %q, want main.py", pf.Script)
		}
		if pf.Declaration == nil || pf.Declaration.Name != "extract_orders" {
			t.Errorf("Declaration name = %v, want extract_orders", pf.Declaration)
		}

		// Accept: a minimal folder with one non-.py script counts as the script.
		good := makePipelineFolder(t, "widgets", map[string]string{
			"iris-declare.yaml": "name: widgets\nrun: [bash, run.sh]\n",
			"run.sh":            "#!/bin/sh\n",
		})
		if _, err := declare.ValidatePipelineFolder(good); err != nil {
			t.Errorf("single-script folder rejected: %v", err)
		}

		// Accept: hidden tooling directories and files (.venv, .git, .DS_Store,
		// ...) are skipped, never mistaken for internal stage structure.
		withHidden := makePipelineFolder(t, "widgets", map[string]string{
			"iris-declare.yaml": "name: widgets\nrun: [python, main.py]\n",
			"main.py":           "print(1)\n",
			".venv/":            "",
			".git/":             "",
			".DS_Store":         "junk\n",
		})
		pf2, err := declare.ValidatePipelineFolder(withHidden)
		if err != nil {
			t.Errorf("folder with hidden tooling entries rejected: %v", err)
		} else if pf2.Script != "main.py" {
			t.Errorf("Script = %q, want main.py", pf2.Script)
		}

		reject := []struct {
			name  string
			files map[string]string
		}{
			{"missing-declaration", map[string]string{"main.py": "print(1)\n"}},
			{"missing-script", map[string]string{"iris-declare.yaml": "name: widgets\nrun: [python, main.py]\n"}},
			{"two-scripts", map[string]string{
				"iris-declare.yaml": "name: widgets\nrun: [python, main.py]\n",
				"main.py":           "print(1)\n",
				"helper.py":         "print(2)\n",
			}},
			{"stage-subfolder", map[string]string{
				"iris-declare.yaml": "name: widgets\nrun: [python, main.py]\n",
				"main.py":           "print(1)\n",
				"stage/":            "",
			}},
			{"composer-not-pipeline", map[string]string{
				"iris-declare.yaml": "lane: widgets\norder: [a, b]\n",
				"main.py":           "print(1)\n",
			}},
		}
		for _, tc := range reject {
			t.Run(tc.name, func(t *testing.T) {
				dir := makePipelineFolder(t, "widgets", tc.files)
				if _, err := declare.ValidatePipelineFolder(dir); err == nil {
					t.Errorf("%s folder accepted; expected rejection", tc.name)
				}
			})
		}
	})
}

// TestNameMatchesFolder proves that a pipeline folder whose declaration name
// does not match its folder name is rejected.
func TestNameMatchesFolder(t *testing.T) {
	t.Run("S03/name-matches-folder", func(t *testing.T) {
		// Reject: the name_folder_mismatch fixture (name != folder).
		if _, err := declare.ValidatePipelineFolder(fixtures.InvalidDeclaration("name_folder_mismatch")); err == nil {
			t.Error("name_folder_mismatch fixture accepted; expected rejection")
		}

		// Reject in isolation: a well-shaped folder whose only defect is a name
		// that disagrees with the folder; the error names the disagreement.
		dir := makePipelineFolder(t, "orders", map[string]string{
			"iris-declare.yaml": "name: not_orders\nrun: [python, main.py]\n",
			"main.py":           "print(1)\n",
		})
		_, err := declare.ValidatePipelineFolder(dir)
		if err == nil {
			t.Fatal("folder with mismatched name accepted; expected rejection")
		}
		if !strings.Contains(err.Error(), "orders") {
			t.Errorf("mismatch error %q does not name the folder", err)
		}

		// Accept: the golden load_orders folder, where name matches the folder.
		pf, err := declare.ValidatePipelineFolder(
			filepath.Join(fixtures.WorkspaceGolden(), "pipelines", "ingest", "load_orders"))
		if err != nil {
			t.Fatalf("golden load_orders folder rejected: %v", err)
		}
		if pf.Declaration.Name != "load_orders" {
			t.Errorf("Declaration name = %q, want load_orders", pf.Declaration.Name)
		}
	})
}
