// Package build holds the engine's build decision logic: which toolchain a
// pipeline builds with, chosen from the pipeline's declared run vector and from
// nothing else (specification sections 1, 3, and 9).
//
// The model is deliberately small and closed:
//
//   - The runtime is inferred from the run vector's interpreter position
//     (argv[0]): [python, main.py] is a python pipeline, [go, run, main.go] a Go
//     one, [node, index.js] a Node one. No language, build, recipe, or toolchain
//     field exists in the declaration to consult instead -- the declaration's
//     field whitelist (internal/declare) rejects any attempt to add one, so the
//     recipe is the engine's choice, never declared or overridden.
//   - Each supported runtime has exactly one pinned recipe, never a menu: Go via
//     native go build, Python via PyInstaller one-file, Node via pkg. A runtime
//     with no pinned recipe fails with an "unsupported runtime" error until a
//     recipe is added to the pinned set.
//
// This package is a leaf (specification section 10): pure decision logic with no
// Iris dependencies and no I/O. Actually invoking a recipe's toolchain -- running
// go build, PyInstaller, or pkg and recording the produced binary's content hash
// and bytes -- is the build execution path's job, not this package's.
package build

import (
	"fmt"
	"path/filepath"
	"regexp"
)

// Runtime is a pipeline's language runtime, inferred from the run vector's
// interpreter position. The set is closed: exactly the runtimes with a pinned
// build recipe.
type Runtime string

// The supported runtimes (the pinned-recipe roster of specification section 9).
const (
	// RuntimeGo is a Go pipeline (dev run through the go tool, e.g. go run main.go).
	RuntimeGo Runtime = "go"
	// RuntimePython is a Python pipeline (dev run through a python interpreter).
	RuntimePython Runtime = "python"
	// RuntimeNode is a Node pipeline (dev run through a node interpreter).
	RuntimeNode Runtime = "node"
)

// The pinned toolchains, one per runtime and never a menu (specification
// section 9). These are stable identifiers of the engine's choice; the build
// execution path owns turning them into real tool invocations.
const (
	// ToolchainGoBuild builds a Go pipeline via the native go build.
	ToolchainGoBuild = "go build"
	// ToolchainPyInstallerOneFile builds a Python pipeline via PyInstaller in
	// one-file mode.
	ToolchainPyInstallerOneFile = "pyinstaller --onefile"
	// ToolchainPkg builds a Node pipeline via pkg.
	ToolchainPkg = "pkg"
)

// Recipe is one pinned build recipe: the runtime it is pinned to and the
// toolchain that compiles that runtime's source into one self-contained binary.
// Recipes are values from a closed engine-owned set; there is no way to declare
// or construct an alternative for a runtime.
type Recipe struct {
	// Runtime is the runtime this recipe is pinned to.
	Runtime Runtime
	// Toolchain is the one pinned toolchain for the runtime.
	Toolchain string
}

// pinnedRecipes is the closed recipe set: exactly one recipe per supported
// runtime (specification section 9, "one pinned recipe per language, never a
// menu"). Adding a runtime means adding its one row here.
var pinnedRecipes = map[Runtime]Recipe{
	RuntimeGo:     {Runtime: RuntimeGo, Toolchain: ToolchainGoBuild},
	RuntimePython: {Runtime: RuntimePython, Toolchain: ToolchainPyInstallerOneFile},
	RuntimeNode:   {Runtime: RuntimeNode, Toolchain: ToolchainPkg},
}

// interpreterRuntimes maps an interpreter name pattern to its runtime. The
// interpreter is the run vector's argv[0] basename; a version suffix (python3,
// python3.12, node18) or an absolute/virtualenv path still names its runtime.
// The patterns are anchored: a name that merely contains a runtime's name (e.g.
// python-config) is not that runtime.
var interpreterRuntimes = []struct {
	pattern *regexp.Regexp
	runtime Runtime
}{
	{regexp.MustCompile(`^go([0-9][0-9.]*)?$`), RuntimeGo},
	{regexp.MustCompile(`^python([0-9][0-9.]*)?$`), RuntimePython},
	{regexp.MustCompile(`^node(js)?([0-9][0-9.]*)?$`), RuntimeNode},
}

// InferRuntime infers a pipeline's runtime from its declared run vector, the
// only input the inference consults (specification section 3: no language or
// build field exists). The interpreter position (argv[0]) decides; a runtime
// outside the pinned set is an "unsupported runtime" error.
func InferRuntime(run []string) (Runtime, error) {
	if len(run) == 0 {
		return "", fmt.Errorf("build: empty run vector; the run command names the runtime and there is nothing to infer from")
	}
	interp := filepath.Base(run[0])
	for _, ir := range interpreterRuntimes {
		if ir.pattern.MatchString(interp) {
			return ir.runtime, nil
		}
	}
	return "", fmt.Errorf("build: unsupported runtime %q: no pinned build recipe (supported: go, node, python)", interp)
}

// PinnedRecipe returns the one pinned recipe for a runtime (specification
// section 9). There is no menu of alternatives: a supported runtime has exactly
// one recipe, and a runtime outside the pinned set is an "unsupported runtime"
// error.
func PinnedRecipe(rt Runtime) (Recipe, error) {
	r, ok := pinnedRecipes[rt]
	if !ok {
		return Recipe{}, fmt.Errorf("build: unsupported runtime %q: no pinned build recipe (supported: go, node, python)", rt)
	}
	return r, nil
}

// InferRecipe infers a pipeline's build recipe from its declared run vector:
// the runtime from the interpreter position, then that runtime's one pinned
// recipe. This is the whole recipe decision -- the choice is the engine's,
// driven by run alone, never declared (specification sections 1 and 9).
func InferRecipe(run []string) (Recipe, error) {
	rt, err := InferRuntime(run)
	if err != nil {
		return Recipe{}, err
	}
	return PinnedRecipe(rt)
}
