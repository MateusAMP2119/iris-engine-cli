package cli

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/MateusAMP2119/iris-engine-cli/internal/declare"
)

// The embedded sample's paths inside the quickstartdata tree.
const (
	sampleDeclPath   = quickstartDataDir + "/pipelines/hello_iris/iris-declare.yaml"
	sampleScriptPath = quickstartDataDir + "/pipelines/hello_iris/main.sh"
	sampleTablePath  = quickstartDataDir + "/schemas/demo/colors/table.yaml"
)

// wantSampleFields is the sample's explicit field list on demo.colors, reads
// and writes alike (no implicit all-columns).
var wantSampleFields = []string{"name", "hex", "wavelength_nm", "noted_at"}

// TestQuickstartSampleValidDeclaration proves the embedded hello_iris sample
// parses through the real declare loaders: the declaration is a pipeline whose
// name matches its folder, with explicit fields and no lane, and the table file
// is a demo.colors head whose pk is name (text) with every type inside the
// closed set.
func TestQuickstartSampleValidDeclaration(t *testing.T) {
	// spec: S08/quickstart-sample-valid-declaration
	t.Run("S08/quickstart-sample-valid-declaration", func(t *testing.T) {
		t.Run("declaration", func(t *testing.T) {
			data, err := quickstartData.ReadFile(sampleDeclPath)
			if err != nil {
				t.Fatalf("embedded declaration missing: %v", err)
			}
			decl, err := declare.ParseDeclaration(data)
			if err != nil {
				t.Fatalf("embedded declaration does not parse through the real loader: %v", err)
			}
			if decl.Kind != declare.KindPipeline {
				t.Fatalf("embedded declaration kind = %s, want pipeline", decl.Kind)
			}
			p := decl.Pipeline

			// The folder name is authoritative; the declared name must match it.
			if folder := filepath.Base(filepath.Dir(sampleDeclPath)); p.Name != folder {
				t.Errorf("declared name %q does not match its folder %q", p.Name, folder)
			}
			if want := []string{"sh", "main.sh"}; !equalStringSlices(p.Run, want) {
				t.Errorf("run argv = %v, want %v (a POSIX-sh script)", p.Run, want)
			}
			// No lane: a manual run is immediate and synchronous.
			if p.Lane != "" {
				t.Errorf("sample declares lane %q; it must be laneless", p.Lane)
			}
			for _, accesses := range []struct {
				list string
				a    []declare.Access
			}{{"reads", p.Reads}, {"writes", p.Writes}} {
				if len(accesses.a) != 1 {
					t.Errorf("%s has %d entries, want 1", accesses.list, len(accesses.a))
					continue
				}
				if got := accesses.a[0].Table; got != "demo.colors" {
					t.Errorf("%s table = %q, want demo.colors", accesses.list, got)
				}
				if !equalStringSlices(accesses.a[0].Fields, wantSampleFields) {
					t.Errorf("%s fields = %v, want the explicit %v", accesses.list, accesses.a[0].Fields, wantSampleFields)
				}
			}
		})

		t.Run("table", func(t *testing.T) {
			data, err := quickstartData.ReadFile(sampleTablePath)
			if err != nil {
				t.Fatalf("embedded table file missing: %v", err)
			}
			tbl, err := declare.ParseTable(data)
			if err != nil {
				t.Fatalf("embedded table file does not parse through the real loader: %v", err)
			}
			if tbl.Schema != "demo" || tbl.Table != "colors" {
				t.Errorf("table identity = %s.%s, want demo.colors", tbl.Schema, tbl.Table)
			}
			cols := map[string]declare.Column{}
			for _, c := range tbl.Columns {
				cols[c.Name] = c
				if _, err := declare.ResolveType(c.Type); err != nil {
					t.Errorf("column %s: %v", c.Name, err)
				}
			}
			for _, want := range wantSampleFields {
				if _, ok := cols[want]; !ok {
					t.Errorf("declared field %q has no column in table.yaml", want)
				}
			}
			pk, ok := cols["name"]
			if !ok || !pk.PrimaryKey || pk.Type != "text" {
				t.Errorf("pk column = %+v, want name text primary_key", pk)
			}
		})

		t.Run("script", func(t *testing.T) {
			data, err := quickstartData.ReadFile(sampleScriptPath)
			if err != nil {
				t.Fatalf("embedded script missing: %v", err)
			}
			script := string(data)
			if !strings.HasPrefix(script, "#!/bin/sh\n") {
				t.Errorf("script does not open with a POSIX sh shebang: %q", firstLine(script))
			}
			for _, want := range []string{"set -eu", "IRIS_DB_URL", "ON CONFLICT (name) DO UPDATE", "ON_ERROR_STOP=1"} {
				if !strings.Contains(script, want) {
					t.Errorf("script is missing %q", want)
				}
			}
		})
	})
}

// TestQuickstartSampleMaterializeNeverClobber proves the materializer writes
// sample files only when absent (byte-identical to the embedded golden and
// loadable by the real loaders), silently skips identical files on a re-run,
// and keeps -- with a warning -- a present-but-different file.
func TestQuickstartSampleMaterializeNeverClobber(t *testing.T) {
	// spec: S08/quickstart-sample-materialize-never-clobber
	t.Run("S08/quickstart-sample-materialize-never-clobber", func(t *testing.T) {
		root := t.TempDir()
		relFiles := []string{
			"pipelines/hello_iris/iris-declare.yaml",
			"pipelines/hello_iris/main.sh",
			"schemas/demo/colors/table.yaml",
		}

		t.Run("fresh workspace gets every file", func(t *testing.T) {
			var warn bytes.Buffer
			written, err := materializeQuickstartSample(root, &warn)
			if err != nil {
				t.Fatalf("materialize: %v", err)
			}
			if warn.Len() != 0 {
				t.Errorf("fresh materialize warned: %q", warn.String())
			}
			if len(written) != len(relFiles) {
				t.Errorf("materialize wrote %v, want all of %v", written, relFiles)
			}
			for _, rel := range relFiles {
				want, err := quickstartData.ReadFile(quickstartDataDir + "/" + rel)
				if err != nil {
					t.Fatalf("embedded %s: %v", rel, err)
				}
				got, err := os.ReadFile(filepath.Join(root, filepath.FromSlash(rel))) //nolint:gosec // G304: rel is a fixed sample path under the test's own TempDir, not user input.
				if err != nil {
					t.Fatalf("materialized %s: %v", rel, err)
				}
				if !bytes.Equal(got, want) {
					t.Errorf("materialized %s differs from the embedded golden", rel)
				}
			}

			// The materialized tree satisfies the real workspace loaders.
			folder, err := declare.ValidatePipelineFolder(filepath.Join(root, "pipelines", "hello_iris"))
			if err != nil {
				t.Errorf("materialized pipeline folder does not validate: %v", err)
			} else if folder.Script != "main.sh" {
				t.Errorf("materialized pipeline script = %q, want main.sh", folder.Script)
			}
			if _, err := declare.ValidateSchemaTree(filepath.Join(root, "schemas")); err != nil {
				t.Errorf("materialized schema tree does not validate: %v", err)
			}
		})

		t.Run("identical files are skipped silently", func(t *testing.T) {
			var warn bytes.Buffer
			written, err := materializeQuickstartSample(root, &warn)
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
			written, err := materializeQuickstartSample(root, &warn)
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
