package build_test

import (
	"reflect"
	"strings"
	"testing"

	"github.com/MateusAMP2119/iris-lakehouse/internal/build"
	"github.com/MateusAMP2119/iris-lakehouse/internal/declare"
)

// TestBuildToolchainInferredFromRun proves the build recipe's toolchain is
// inferred from the pipeline's run command alone. InferRecipe takes exactly the
// declared argv vector -- there is no separate toolchain declaration anywhere in
// its inputs -- and the interpreter position (argv[0]) decides the recipe: a
// version-suffixed or path-qualified interpreter still names its runtime.
func TestBuildToolchainInferredFromRun(t *testing.T) {
	cases := []struct {
		name string
		run  []string
		want build.Runtime
	}{
		{"go run", []string{"go", "run", "main.go"}, build.RuntimeGo},
		{"python script", []string{"python", "main.py"}, build.RuntimePython},
		{"python3 script", []string{"python3", "main.py"}, build.RuntimePython},
		{"versioned python", []string{"python3.12", "etl.py"}, build.RuntimePython},
		{"path-qualified python", []string{"/usr/bin/python3", "main.py"}, build.RuntimePython},
		{"venv python", []string{"./.venv/bin/python", "main.py"}, build.RuntimePython},
		{"node script", []string{"node", "index.js"}, build.RuntimeNode},
		{"nodejs alias", []string{"nodejs", "app.js"}, build.RuntimeNode},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rec, err := build.InferRecipe(tc.run)
			if err != nil {
				t.Fatalf("InferRecipe(%v): %v", tc.run, err)
			}
			if rec.Runtime != tc.want {
				t.Errorf("InferRecipe(%v).Runtime = %q, want %q", tc.run, rec.Runtime, tc.want)
			}
			// The recipe carries a toolchain; the run vector is where it came from.
			if rec.Toolchain == "" {
				t.Errorf("InferRecipe(%v) returned an empty toolchain; the recipe's toolchain must be pinned", tc.run)
			}
		})
	}
}

// TestToolchainInferredFromRunVector proves the engine infers the build
// toolchain from the declared run vector with no language or build field
// consulted: the canonical [python, main.py] example selects the python recipe
// when fed straight from a parsed declaration, and the declaration model itself
// carries no language, build, recipe, or toolchain field the inference could
// have consulted instead.
func TestToolchainInferredFromRunVector(t *testing.T) {
	// The canonical example, parsed as a real declaration: the run vector is the
	// only inference input, straight off the declared model.
	decl, err := declare.ParseDeclaration([]byte("name: extract_orders\nrun: [python, main.py]\n"))
	if err != nil {
		t.Fatalf("ParseDeclaration: %v", err)
	}
	rec, err := build.InferRecipe(decl.Pipeline.Run)
	if err != nil {
		t.Fatalf("InferRecipe(%v): %v", decl.Pipeline.Run, err)
	}
	if rec.Runtime != build.RuntimePython {
		t.Errorf("InferRecipe([python, main.py]).Runtime = %q, want %q (the python recipe)", rec.Runtime, build.RuntimePython)
	}

	// No language or build field exists to consult: the pipeline declaration model
	// carries none of the fields a toolchain choice could hide in -- language and
	// build are among the deliberately absent fields.
	pt := reflect.TypeOf(declare.Pipeline{})
	for i := 0; i < pt.NumField(); i++ {
		tag := strings.Split(pt.Field(i).Tag.Get("yaml"), ",")[0]
		switch tag {
		case "language", "build", "recipe", "toolchain":
			t.Errorf("declare.Pipeline carries a %q field; the toolchain is inferred from run, never declared", tag)
		}
	}
}

// TestBuildRecipeNotDeclarable proves the build recipe is the engine's choice
// from the pipeline's runtime and cannot be declared or overridden in the
// pipeline declaration: a declaration carrying a build, recipe, language, or
// toolchain field is rejected outright as an unknown field.
func TestBuildRecipeNotDeclarable(t *testing.T) {
	// Positive control: the same declaration without the offending field parses.
	if _, err := declare.ParseDeclaration([]byte("name: extract_orders\nrun: [python, main.py]\n")); err != nil {
		t.Fatalf("control declaration failed to parse: %v", err)
	}

	cases := []struct {
		field string
		doc   string
	}{
		{"build", "name: extract_orders\nrun: [python, main.py]\nbuild: pyinstaller\n"},
		{"recipe", "name: extract_orders\nrun: [python, main.py]\nrecipe: onefile\n"},
		{"language", "name: extract_orders\nrun: [python, main.py]\nlanguage: python\n"},
		{"toolchain", "name: extract_orders\nrun: [go, run, main.go]\ntoolchain: go build\n"},
	}
	for _, tc := range cases {
		t.Run(tc.field, func(t *testing.T) {
			_, err := declare.ParseDeclaration([]byte(tc.doc))
			if err == nil {
				t.Fatalf("declaration carrying %q parsed; the build recipe must not be declarable", tc.field)
			}
			if !strings.Contains(err.Error(), tc.field) {
				t.Errorf("rejection does not name the offending field %q: %v", tc.field, err)
			}
		})
	}
}

// TestPinnedBuildRecipePerRuntime proves each supported runtime has exactly one
// pinned recipe, never a menu: Go builds via native go build, Python via
// PyInstaller one-file, Node via pkg. PinnedRecipe returns a single Recipe --
// there is no alternatives list to choose from -- and repeated calls return the
// identical pinned value.
func TestPinnedBuildRecipePerRuntime(t *testing.T) {
	cases := []struct {
		runtime   build.Runtime
		toolchain string
	}{
		{build.RuntimeGo, build.ToolchainGoBuild},
		{build.RuntimePython, build.ToolchainPyInstallerOneFile},
		{build.RuntimeNode, build.ToolchainPkg},
	}
	for _, tc := range cases {
		t.Run(string(tc.runtime), func(t *testing.T) {
			rec, err := build.PinnedRecipe(tc.runtime)
			if err != nil {
				t.Fatalf("PinnedRecipe(%q): %v", tc.runtime, err)
			}
			if rec.Runtime != tc.runtime {
				t.Errorf("PinnedRecipe(%q).Runtime = %q, want %q", tc.runtime, rec.Runtime, tc.runtime)
			}
			if rec.Toolchain != tc.toolchain {
				t.Errorf("PinnedRecipe(%q).Toolchain = %q, want the one pinned toolchain %q", tc.runtime, rec.Toolchain, tc.toolchain)
			}
			// No menu: asking again yields the identical pinned recipe, not an
			// alternative.
			again, err := build.PinnedRecipe(tc.runtime)
			if err != nil {
				t.Fatalf("PinnedRecipe(%q) second call: %v", tc.runtime, err)
			}
			if again != rec {
				t.Errorf("PinnedRecipe(%q) is not stable: %+v then %+v; one pinned recipe per runtime, never a menu", tc.runtime, rec, again)
			}
		})
	}

	// The three pinned toolchains are pairwise distinct: one recipe per runtime
	// also means one runtime per recipe.
	seen := map[string]build.Runtime{}
	for _, tc := range cases {
		if prev, dup := seen[tc.toolchain]; dup {
			t.Errorf("toolchain %q is pinned to both %q and %q", tc.toolchain, prev, tc.runtime)
		}
		seen[tc.toolchain] = tc.runtime
	}
}

// TestUnsupportedRuntimeBuildError proves a run vector whose runtime has no
// pinned recipe fails with an "unsupported runtime" error: any runtime outside
// the pinned set fails that way until a recipe is added.
func TestUnsupportedRuntimeBuildError(t *testing.T) {
	cases := []struct {
		name string
		run  []string
	}{
		{"ruby", []string{"ruby", "main.rb"}},
		{"bash", []string{"bash", "run.sh"}},
		{"java", []string{"java", "-jar", "etl.jar"}},
		{"rscript", []string{"Rscript", "model.R"}},
		{"direct script", []string{"./main.py"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := build.InferRecipe(tc.run)
			if err == nil {
				t.Fatalf("InferRecipe(%v) succeeded; a runtime with no pinned recipe must fail", tc.run)
			}
			if !strings.Contains(err.Error(), "unsupported runtime") {
				t.Errorf("InferRecipe(%v) error does not say %q: %v", tc.run, "unsupported runtime", err)
			}
		})
	}

	// An empty run vector cannot name a runtime at all; it must fail rather than
	// silently pick a recipe. (Declaration validation already rejects it upstream;
	// the inference is safe standalone.)
	if _, err := build.InferRecipe(nil); err == nil {
		t.Error("InferRecipe(nil) succeeded; an empty run vector names no runtime")
	}
}
