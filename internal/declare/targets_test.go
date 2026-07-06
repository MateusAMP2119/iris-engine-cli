package declare_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/MateusAMP2119/iris-engine-cli/internal/declare"
)

// writeDeclFile writes contents at path, creating its parent directories.
func writeDeclFile(t *testing.T, path, contents string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatalf("mkdir parent of %s: %v", path, err)
	}
	if err := os.WriteFile(path, []byte(contents), 0o644); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
}

const targetPipelineYAML = "name: extract\nrun: [python, main.py]\n"

// TestResolveDeclarationFile proves the target-resolution rule behind iris
// declare apply/destroy (specification sections 3, 6.3, and 8): a file target
// must be named iris-declare.yaml; a folder target resolves to its
// iris-declare.yaml with no further search, so a folder with no top-level
// declaration is a precise error rather than a sweep into nested ones.
func TestResolveDeclarationFile(t *testing.T) {
	// spec: S06.3/apply-single-file-resolution
	t.Run("S06.3/apply-single-file-resolution", func(t *testing.T) {
		t.Run("file target named iris-declare.yaml resolves to itself", func(t *testing.T) {
			dir := t.TempDir()
			p := filepath.Join(dir, "iris-declare.yaml")
			writeDeclFile(t, p, targetPipelineYAML)

			got, err := declare.ResolveDeclarationFile(p)
			if err != nil {
				t.Fatalf("ResolveDeclarationFile(%s): %v", p, err)
			}
			if got != p {
				t.Errorf("resolved = %q, want %q", got, p)
			}
		})

		t.Run("folder target resolves to folder/iris-declare.yaml", func(t *testing.T) {
			dir := t.TempDir()
			p := filepath.Join(dir, "iris-declare.yaml")
			writeDeclFile(t, p, targetPipelineYAML)

			got, err := declare.ResolveDeclarationFile(dir)
			if err != nil {
				t.Fatalf("ResolveDeclarationFile(%s): %v", dir, err)
			}
			if got != p {
				t.Errorf("resolved = %q, want %q", got, p)
			}
		})

		t.Run("folder missing the declaration is a precise error, not a sweep", func(t *testing.T) {
			// Nest a real declaration well below the target folder; the resolver
			// must still fail on the target itself rather than finding the nested
			// one -- proof there is no workspace sweep or transitive chaining.
			dir := t.TempDir()
			nested := filepath.Join(dir, "pipelines", "ingest", "extract", "iris-declare.yaml")
			writeDeclFile(t, nested, targetPipelineYAML)

			_, err := declare.ResolveDeclarationFile(dir)
			if err == nil {
				t.Fatal("want an error for a folder with no top-level iris-declare.yaml")
			}
			if !strings.Contains(err.Error(), "iris-declare.yaml") {
				t.Errorf("error %q does not name iris-declare.yaml", err)
			}
		})

		t.Run("file not named iris-declare.yaml is rejected", func(t *testing.T) {
			dir := t.TempDir()
			p := filepath.Join(dir, "declare.yaml")
			writeDeclFile(t, p, targetPipelineYAML)

			_, err := declare.ResolveDeclarationFile(p)
			if err == nil {
				t.Fatal("want an error for a misnamed declaration file")
			}
		})

		t.Run("nonexistent path is a precise error", func(t *testing.T) {
			missing := filepath.Join(t.TempDir(), "nope")
			if _, err := declare.ResolveDeclarationFile(missing); err == nil {
				t.Fatal("want an error for a nonexistent path")
			}
		})
	})
}

// TestLoadDeclarationFile proves that loading a target resolves it exactly as
// ResolveDeclarationFile does and then parses the single resolved file,
// propagating both resolution and parse failures untouched.
func TestLoadDeclarationFile(t *testing.T) {
	// spec: S03/apply-destroy-single-file
	t.Run("S03/apply-destroy-single-file", func(t *testing.T) {
		t.Run("loads and parses the one resolved file", func(t *testing.T) {
			dir := t.TempDir()
			p := filepath.Join(dir, "iris-declare.yaml")
			writeDeclFile(t, p, targetPipelineYAML)

			resolved, decl, err := declare.LoadDeclarationFile(dir)
			if err != nil {
				t.Fatalf("LoadDeclarationFile(%s): %v", dir, err)
			}
			if resolved != p {
				t.Errorf("resolved = %q, want %q", resolved, p)
			}
			if decl.Kind != declare.KindPipeline || decl.Pipeline == nil || decl.Pipeline.Name != "extract" {
				t.Errorf("decl = %+v, want a parsed pipeline named extract", decl)
			}
		})

		t.Run("propagates a parse error from the single resolved file", func(t *testing.T) {
			dir := t.TempDir()
			p := filepath.Join(dir, "iris-declare.yaml")
			writeDeclFile(t, p, "name: extract\n") // missing required run

			if _, _, err := declare.LoadDeclarationFile(p); err == nil {
				t.Fatal("want a parse error to propagate")
			}
		})

		t.Run("propagates a resolution error for a folder with no declaration", func(t *testing.T) {
			if _, _, err := declare.LoadDeclarationFile(t.TempDir()); err == nil {
				t.Fatal("want a resolution error to propagate")
			}
		})
	})
}
