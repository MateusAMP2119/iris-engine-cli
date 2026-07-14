package cli

import (
	"bytes"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/MateusAMP2119/iris-lakehouse/internal/declare"
)

// starterScriptMarks are the starter-constraint markers every catalog entry's
// script must carry: a POSIX-sh shebang plus set -eu opening, psql over the
// engine-injected IRIS_DB_URL with ON_ERROR_STOP, and fixed-pk upserts so a
// re-run layers a second provenance stamp on the same rows.
var starterScriptMarks = []string{"set -eu", "IRIS_DB_URL", "ON_ERROR_STOP=1", "ON CONFLICT", "DO UPDATE"}

// TestQuickstartCatalogEntriesValid proves the embedded catalog registry: the
// ordered catalog.yaml index lists exactly the entry folders with unique ids
// and hello_iris first, and every entry carries complete metadata whose
// declaration, table files, and script satisfy the starter constraints through
// the real declare loaders.
func TestQuickstartCatalogEntriesValid(t *testing.T) {
	t.Run("quickstart-catalog-entries-valid", func(t *testing.T) {
		cat, err := loadCatalog()
		if err != nil {
			t.Fatalf("loadCatalog: %v", err)
		}
		if len(cat.Entries) == 0 {
			t.Fatal("catalog is empty")
		}

		t.Run("index lists exactly the entry folders, ids unique, hello_iris first", func(t *testing.T) {
			if got := cat.Entries[0].ID; got != "hello_iris" {
				t.Errorf("entry 1 = %q, want hello_iris (the default pick)", got)
			}
			seen := map[string]bool{}
			for _, e := range cat.Entries {
				if seen[e.ID] {
					t.Errorf("duplicate catalog id %q", e.ID)
				}
				seen[e.ID] = true
			}

			// The index and the embedded entry folders must agree exactly.
			dirs, err := fs.ReadDir(catalogData, catalogDataDir)
			if err != nil {
				t.Fatalf("read embedded catalog root: %v", err)
			}
			folders := map[string]bool{}
			for _, d := range dirs {
				if d.IsDir() {
					folders[d.Name()] = true
				}
			}
			for id := range seen {
				if !folders[id] {
					t.Errorf("catalog.yaml lists %q but no entry folder exists", id)
				}
			}
			for folder := range folders {
				if !seen[folder] {
					t.Errorf("entry folder %q is not listed in catalog.yaml", folder)
				}
			}
		})

		t.Run("the starter entries ship", func(t *testing.T) {
			for _, want := range []string{"system_snapshot", "word_frequency"} {
				if _, err := catalogEntryByID(want); err != nil {
					t.Errorf("starter entry %q missing: %v", want, err)
				}
			}
		})

		t.Run("an unknown id is refused naming the available ids", func(t *testing.T) {
			_, err := catalogEntryByID("no_such_pipeline")
			if err == nil {
				t.Fatal("catalogEntryByID accepted an unknown id")
			}
			for _, e := range cat.Entries {
				if !strings.Contains(err.Error(), e.ID) {
					t.Errorf("unknown-id error does not name available id %q: %v", e.ID, err)
				}
			}
		})

		for _, e := range cat.Entries {
			t.Run(e.ID, func(t *testing.T) {
				t.Run("metadata is complete", func(t *testing.T) {
					for name, v := range map[string]string{
						"name": e.Name, "pitch": e.Pitch, "description": e.Description,
						"run_note": e.RunNote, "showcase.table": e.Showcase.Table, "showcase.pk": e.Showcase.PK,
					} {
						if strings.TrimSpace(v) == "" {
							t.Errorf("entry %s: %s is empty", e.ID, name)
						}
					}
				})

				declPath := catalogDataDir + "/" + e.ID + "/workspace/pipelines/" + e.ID + "/iris-declare.yaml"
				data, err := catalogData.ReadFile(declPath)
				if err != nil {
					t.Fatalf("embedded declaration missing at %s: %v", declPath, err)
				}
				decl, err := declare.ParseDeclaration(data)
				if err != nil {
					t.Fatalf("declaration does not parse through the real loader: %v", err)
				}
				if decl.Kind != declare.KindPipeline {
					t.Fatalf("declaration kind = %s, want pipeline", decl.Kind)
				}
				p := decl.Pipeline

				t.Run("declaration obeys the starter constraints", func(t *testing.T) {
					if p.Name != e.ID {
						t.Errorf("declared name %q does not match the entry id %q", p.Name, e.ID)
					}
					if want := []string{"sh", "main.sh"}; !equalStringSlices(p.Run, want) {
						t.Errorf("run argv = %v, want %v (a POSIX-sh script)", p.Run, want)
					}
					if p.Lane != "" {
						t.Errorf("entry declares lane %q; starters are laneless", p.Lane)
					}
					showcased := false
					for _, w := range p.Writes {
						if w.Table == e.Showcase.Table {
							showcased = true
						}
					}
					if !showcased {
						t.Errorf("showcase.table %q is not among the declared writes", e.Showcase.Table)
					}
				})

				t.Run("every declared table parses with closed-set types", func(t *testing.T) {
					for _, acc := range append(append([]declare.Access(nil), p.Reads...), p.Writes...) {
						parts := strings.SplitN(acc.Table, ".", 2)
						if len(parts) != 2 {
							t.Fatalf("access table %q is not schema.table", acc.Table)
						}
						tblPath := catalogDataDir + "/" + e.ID + "/workspace/schemas/" + parts[0] + "/" + parts[1] + "/table.yaml"
						data, err := catalogData.ReadFile(tblPath)
						if err != nil {
							t.Fatalf("embedded table file missing at %s: %v", tblPath, err)
						}
						tbl, err := declare.ParseTable(data)
						if err != nil {
							t.Fatalf("table file does not parse through the real loader: %v", err)
						}
						if tbl.Schema != parts[0] || tbl.Table != parts[1] {
							t.Errorf("table identity = %s.%s, want %s", tbl.Schema, tbl.Table, acc.Table)
						}
						cols := map[string]declare.Column{}
						for _, c := range tbl.Columns {
							cols[c.Name] = c
							if _, err := declare.ResolveType(c.Type); err != nil {
								t.Errorf("column %s: %v", c.Name, err)
							}
						}
						for _, f := range acc.Fields {
							if _, ok := cols[f]; !ok {
								t.Errorf("declared field %q has no column in %s", f, tblPath)
							}
						}
						if acc.Table == e.Showcase.Table {
							pkOK := false
							for _, c := range tbl.Columns {
								if c.PrimaryKey && c.Type == "text" {
									pkOK = true
								}
							}
							if !pkOK {
								t.Errorf("showcase table %s has no text primary key for the finale pk %q", acc.Table, e.Showcase.PK)
							}
						}
					}
				})

				t.Run("script obeys the starter constraints", func(t *testing.T) {
					scriptPath := catalogDataDir + "/" + e.ID + "/workspace/pipelines/" + e.ID + "/main.sh"
					data, err := catalogData.ReadFile(scriptPath)
					if err != nil {
						t.Fatalf("embedded script missing at %s: %v", scriptPath, err)
					}
					script := string(data)
					if !strings.HasPrefix(script, "#!/bin/sh\n") {
						t.Errorf("script does not open with a POSIX sh shebang: %q", firstLine(script))
					}
					for _, want := range starterScriptMarks {
						if !strings.Contains(script, want) {
							t.Errorf("script is missing %q", want)
						}
					}
				})
			})
		}
	})
}

// TestQuickstartSampleValidDeclaration proves every embedded catalog entry's
// workspace subtree satisfies the real workspace loaders once materialized:
// the pipeline folder validates, the schema tree validates, and the files land
// byte-identical to the embedded goldens.
func TestQuickstartSampleValidDeclaration(t *testing.T) {
	t.Run("quickstart-sample-valid-declaration", func(t *testing.T) {
		cat, err := loadCatalog()
		if err != nil {
			t.Fatalf("loadCatalog: %v", err)
		}
		for _, e := range cat.Entries {
			t.Run(e.ID, func(t *testing.T) {
				root := t.TempDir()
				var warn bytes.Buffer
				written, err := materializeCatalogEntry(e.ID, root, &warn)
				if err != nil {
					t.Fatalf("materialize %s: %v", e.ID, err)
				}
				if warn.Len() != 0 {
					t.Errorf("fresh materialize warned: %q", warn.String())
				}
				if len(written) == 0 {
					t.Fatalf("materialize %s wrote nothing", e.ID)
				}
				for _, rel := range written {
					want, err := catalogData.ReadFile(catalogDataDir + "/" + e.ID + "/workspace/" + rel)
					if err != nil {
						t.Fatalf("embedded %s: %v", rel, err)
					}
					got, err := os.ReadFile(filepath.Join(root, filepath.FromSlash(rel))) //nolint:gosec // G304: rel is an embedded catalog path under the test's own TempDir, not user input.
					if err != nil {
						t.Fatalf("materialized %s: %v", rel, err)
					}
					if !bytes.Equal(got, want) {
						t.Errorf("materialized %s differs from the embedded golden", rel)
					}
				}

				folder, err := declare.ValidatePipelineFolder(filepath.Join(root, "pipelines", e.ID))
				if err != nil {
					t.Errorf("materialized pipeline folder does not validate: %v", err)
				} else if folder.Script != "main.sh" {
					t.Errorf("materialized pipeline script = %q, want main.sh", folder.Script)
				}
				if _, err := declare.ValidateSchemaTree(filepath.Join(root, "schemas")); err != nil {
					t.Errorf("materialized schema tree does not validate: %v", err)
				}
			})
		}
	})
}

// TestQuickstartSampleMaterializeNeverClobber proves materializeCatalogEntry
// writes one entry's files only when absent, writes nothing belonging to other
// entries, silently skips identical files on a re-run, and keeps -- with a
// warning -- a present-but-different file.
func TestQuickstartSampleMaterializeNeverClobber(t *testing.T) {
	t.Run("quickstart-sample-materialize-never-clobber", func(t *testing.T) {
		root := t.TempDir()
		relFiles := []string{
			"pipelines/hello_iris/iris-declare.yaml",
			"pipelines/hello_iris/main.sh",
			"schemas/demo/colors/table.yaml",
		}

		t.Run("fresh workspace gets exactly the entry's files", func(t *testing.T) {
			var warn bytes.Buffer
			written, err := materializeCatalogEntry("hello_iris", root, &warn)
			if err != nil {
				t.Fatalf("materialize: %v", err)
			}
			if warn.Len() != 0 {
				t.Errorf("fresh materialize warned: %q", warn.String())
			}
			if !equalStringSlices(written, relFiles) {
				t.Errorf("materialize wrote %v, want exactly %v", written, relFiles)
			}
			// Only the picked entry lands: no other entry's pipeline folder.
			if _, err := os.Stat(filepath.Join(root, "pipelines", "word_frequency")); !os.IsNotExist(err) {
				t.Errorf("materialize leaked another entry's files (word_frequency): %v", err)
			}
		})

		t.Run("identical files are skipped silently", func(t *testing.T) {
			var warn bytes.Buffer
			written, err := materializeCatalogEntry("hello_iris", root, &warn)
			if err != nil {
				t.Fatalf("re-materialize: %v", err)
			}
			if len(written) != 0 {
				t.Errorf("re-materialize rewrote %v; identical files must be skipped", written)
			}
			if warn.Len() != 0 {
				t.Errorf("re-materialize warned about identical files: %q", warn.String())
			}
		})

		t.Run("a different file is kept and warned about", func(t *testing.T) {
			custom := []byte("#!/bin/sh\necho mine\n")
			scriptPath := filepath.Join(root, "pipelines", "hello_iris", "main.sh")
			if err := os.WriteFile(scriptPath, custom, 0o644); err != nil {
				t.Fatalf("plant custom script: %v", err)
			}
			var warn bytes.Buffer
			written, err := materializeCatalogEntry("hello_iris", root, &warn)
			if err != nil {
				t.Fatalf("materialize over custom file: %v", err)
			}
			if len(written) != 0 {
				t.Errorf("materialize rewrote %v over a present workspace", written)
			}
			got, err := os.ReadFile(scriptPath) //nolint:gosec // G304: scriptPath is a fixed path under the test's own TempDir, not user input.
			if err != nil {
				t.Fatalf("read custom script back: %v", err)
			}
			if !bytes.Equal(got, custom) {
				t.Errorf("materialize clobbered the operator's file: %q", got)
			}
			if w := warn.String(); !strings.Contains(w, "pipelines/hello_iris/main.sh") || !strings.Contains(w, "keeping your file") {
				t.Errorf("kept-file warning missing or unnamed: %q", w)
			}
		})

		t.Run("an unknown entry materializes nothing", func(t *testing.T) {
			scratch := t.TempDir()
			if _, err := materializeCatalogEntry("no_such_pipeline", scratch, io.Discard); err == nil {
				t.Fatal("materializeCatalogEntry accepted an unknown id")
			}
			requireEmptyDir(t, scratch)
		})
	})
}

// equalStringSlices reports element-wise equality.
func equalStringSlices(got, want []string) bool {
	if len(got) != len(want) {
		return false
	}
	for i := range got {
		if got[i] != want[i] {
			return false
		}
	}
	return true
}

// firstLine returns s up to its first newline.
func firstLine(s string) string {
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		return s[:i]
	}
	return s
}
