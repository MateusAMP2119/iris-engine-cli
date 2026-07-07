package dispatch

// This file is the explicit pipeline build op: the leader-side path behind
// `iris pipeline build <name>` (specification sections 1, 4, and 9). Building is
// never implicit -- declare apply registers state and nothing else ("Build never
// folds into apply"); only this op compiles anything. It executes the recipe
// decision internal/build owns: infer the pipeline's runtime from its declared
// run vector, take that runtime's one pinned recipe, and drive the recipe's
// toolchain through the exec seam to compile the source into ONE self-contained
// binary. A successful build then records the binary twice, and exactly twice:
// its bytes go into the content-addressed object store under their SHA-256
// content hash, and that hash goes into the artifacts table as an immutable
// index row through the single meta writer. A failed compile (non-zero exit, or
// a toolchain that produced no binary) records neither, so meta and the object
// store never name bytes that do not exist.
//
// The toolchain runs as a direct exec in the pipeline's folder -- never a shell
// -- via the same exec.Runner seam every subprocess rides, so integration tests
// drive builds with a fake toolchain while the real PyInstaller/pkg/go
// invocations are conformance work (E13).

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/MateusAMP2119/iris-engine-cli/internal/build"
	"github.com/MateusAMP2119/iris-engine-cli/internal/exec"
	"github.com/MateusAMP2119/iris-engine-cli/internal/store"
)

// BuildTarget is one registered pipeline's build input: its name (the artifacts
// row owner), the folder its source lives in (the toolchain's working
// directory), and its declared run vector -- the only input the recipe decision
// consults (specification section 3: no language or build field exists).
type BuildTarget struct {
	// Pipeline is the registered pipeline's name.
	Pipeline string
	// Dir is the pipeline folder the toolchain runs in (the source root).
	Dir string
	// Run is the declared dev-run argv the recipe is inferred from.
	Run []string
}

// ObjectPutter stores immutable content-addressed bytes and returns their
// content hash and size. The production implementation is the object store at
// objects_path (store.ObjectStore); the seam exists so a test can inject a
// failing store.
type ObjectPutter interface {
	// Put stores the bytes read from r under their SHA-256 content hash.
	Put(r io.Reader) (hash string, size int64, err error)
}

// Builder is the explicit build op. It holds only seams -- the single-writer
// submitter, the object store, and the process runner -- so it is composed with
// fakes or the real meta+exec stack alike. The daemon composes it onto POST
// /pipeline/build for the leader.
type Builder struct {
	submit  Submitter
	objects ObjectPutter
	runner  exec.Runner
}

// NewBuilder builds the explicit build op over the single-writer submission
// seam, the content-addressed object store, and the subprocess runner.
func NewBuilder(submit Submitter, objects ObjectPutter, runner exec.Runner) *Builder {
	return &Builder{submit: submit, objects: objects, runner: runner}
}

// Build compiles target's source into one self-contained binary and records it:
// bytes into the object store under their content hash, the hash into artifacts
// as a new immutable row through the single meta writer (specification section
// 9). The recipe is the engine's choice, inferred from the run vector alone; a
// runtime with no pinned recipe fails with the "unsupported runtime" error
// before anything runs. The returned row is the pipeline's newest -- and
// therefore current -- artifact.
func (b *Builder) Build(ctx context.Context, target BuildTarget) (store.ArtifactRow, error) {
	recipe, err := build.InferRecipe(target.Run)
	if err != nil {
		return store.ArtifactRow{}, fmt.Errorf("dispatch: build %q: %w", target.Pipeline, err)
	}

	// The single source the recipe compiles, validated from the run vector before
	// anything runs: the Go package the run vector names, or the interpreted entry
	// script -- never a flag or module name. An unbuildable vector fails here, so the
	// toolchain is never handed a non-source argument.
	src, err := sourceTarget(recipe, target.Run)
	if err != nil {
		return store.ArtifactRow{}, fmt.Errorf("dispatch: build %q: %w", target.Pipeline, err)
	}

	// The binary is compiled into a private staging directory and only its bytes
	// survive -- ingested into the object store under their hash -- so no mutable
	// "latest binary" path ever exists to run from. The toolchain's own scratch
	// (PyInstaller's work and .spec dirs, the dist dir) is pinned under here too, so a
	// real build never writes into the pipeline's source folder.
	stage, err := os.MkdirTemp("", "iris-build-")
	if err != nil {
		return store.ArtifactRow{}, fmt.Errorf("dispatch: build %q: create staging dir: %w", target.Pipeline, err)
	}
	defer func() { _ = os.RemoveAll(stage) }()

	dist := filepath.Join(stage, "dist")
	work := filepath.Join(stage, "work")
	spec := filepath.Join(stage, "spec")
	for _, d := range []string{dist, work, spec} {
		if err := os.MkdirAll(d, 0o750); err != nil {
			return store.ArtifactRow{}, fmt.Errorf("dispatch: build %q: create staging dir: %w", target.Pipeline, err)
		}
	}
	argv, binPath := toolchainArgv(recipe, target, src, dist, work, spec)

	// Direct exec of the pinned toolchain in the pipeline folder, output captured
	// so a failed compile reports the toolchain's own diagnostics.
	var stdout, stderr bytes.Buffer
	h, err := b.runner.Start(ctx, exec.Spec{
		Dir:    target.Dir,
		Argv:   argv,
		Stdout: &stdout,
		Stderr: &stderr,
	})
	if err != nil {
		return store.ArtifactRow{}, fmt.Errorf("dispatch: build %q: start %s: %w", target.Pipeline, recipe.Toolchain, err)
	}
	status, waitErr := h.Wait()
	if status.Signaled || status.Code != 0 {
		failed := fmt.Errorf("dispatch: build %q: %s failed (%s)%s",
			target.Pipeline, recipe.Toolchain, exitString(status), diagnosticTail(&stderr, &stdout))
		// A bounded-output-capture fault coincident with a non-zero exit is joined,
		// not swallowed: both the compile failure and the capture error are reported.
		if waitErr != nil {
			return store.ArtifactRow{}, errors.Join(failed,
				fmt.Errorf("dispatch: build %q: %s output capture: %w", target.Pipeline, recipe.Toolchain, waitErr))
		}
		return store.ArtifactRow{}, failed
	}
	if waitErr != nil {
		return store.ArtifactRow{}, fmt.Errorf("dispatch: build %q: %s output capture: %w", target.Pipeline, recipe.Toolchain, waitErr)
	}

	// One self-contained binary at the staged path; a toolchain that exited zero
	// without producing it is a failed build, never a silent success.
	f, err := os.Open(binPath) //nolint:gosec // G304: the path is engine-composed under the engine's own staging dir.
	if err != nil {
		return store.ArtifactRow{}, fmt.Errorf("dispatch: build %q: %s produced no binary at %s: %w",
			target.Pipeline, recipe.Toolchain, binPath, err)
	}
	hash, size, err := b.objects.Put(f)
	_ = f.Close()
	if err != nil {
		return store.ArtifactRow{}, fmt.Errorf("dispatch: build %q: store binary bytes: %w", target.Pipeline, err)
	}

	// The index row rides the single writer: hash, owner, size; recorded_at is
	// stamped database-side. Insert-only -- a rebuild is a new row, never a
	// rewrite (specification section 4).
	row := store.ArtifactRow{Hash: hash, Pipeline: target.Pipeline, SizeBytes: size}
	if err := b.submit.Submit(ctx, func(w *store.Writer) error {
		return w.InsertArtifact(ctx, row)
	}); err != nil {
		return store.ArtifactRow{}, fmt.Errorf("dispatch: build %q: record artifact %s: %w", target.Pipeline, hash, err)
	}
	return row, nil
}

// toolchainArgv composes the pinned recipe's direct-exec argv and the staged path
// the self-contained binary lands at. The mapping is closed, one invocation shape
// per pinned toolchain (specification section 9): Go native go build, Python via
// PyInstaller one-file, Node via pkg. src is the validated build source
// (sourceTarget): the Go package the run vector names, or the interpreted entry
// script. dist/work/spec are staged scratch dirs under the build staging root, so
// the toolchain never writes into the pipeline's source folder; the staged binary
// is always named after the pipeline, so every recipe yields exactly one output.
func toolchainArgv(r build.Recipe, target BuildTarget, src, dist, work, spec string) (argv []string, binPath string) {
	binPath = filepath.Join(dist, target.Pipeline)
	switch r.Toolchain {
	case build.ToolchainGoBuild:
		return []string{"go", "build", "-o", binPath, src}, binPath
	case build.ToolchainPyInstallerOneFile:
		// --distpath, --workpath, and --specpath are all pinned into the staging tree
		// so PyInstaller's dist/, build/, and <name>.spec never land in the source dir.
		return []string{
			"pyinstaller", "--onefile",
			"--distpath", dist, "--workpath", work, "--specpath", spec,
			"--name", target.Pipeline, src,
		}, binPath
	case build.ToolchainPkg:
		return []string{"pkg", src, "--output", binPath}, binPath
	default:
		// Unreachable through Build: InferRecipe only yields pinned recipes. Kept
		// total so a future recipe added to internal/build without an invocation
		// here fails loudly at the exec seam rather than silently.
		return []string{r.Toolchain}, binPath
	}
}

// sourceTarget validates target.Run's shape for recipe's runtime and returns the
// single source the toolchain compiles, or an error for an unbuildable vector
// (specification sections 1, 3, and 9). It is the guard that keeps a flag or module
// name from ever reaching the toolchain as if it were the entry source.
func sourceTarget(r build.Recipe, run []string) (string, error) {
	switch r.Runtime {
	case build.RuntimeGo:
		return goPackage(run)
	case build.RuntimePython, build.RuntimeNode:
		return entryScript(r.Runtime, run)
	default:
		// Unreachable through Build: InferRecipe only yields pinned runtimes.
		return "", fmt.Errorf("unsupported runtime %q: no build source rule", r.Runtime)
	}
}

// goPackage extracts the package a Go dev-run vector names -- `go run <package>`,
// e.g. [go, run, ./cmd/etl] -> ./cmd/etl -- so go build compiles that package, not
// the folder root. It requires the `go run` shape and a package argument; the
// package is the first non-flag token after `run` (leading build flags are skipped,
// trailing program args never reached). A vector that is not `go run <package>` is
// unbuildable.
func goPackage(run []string) (string, error) {
	if len(run) < 3 || run[1] != "run" {
		return "", fmt.Errorf("unbuildable go run vector %v: expected `go run <package>`", run)
	}
	for _, a := range run[2:] {
		if !strings.HasPrefix(a, "-") {
			return a, nil
		}
	}
	return "", fmt.Errorf("unbuildable go run vector %v: no package argument after `go run`", run)
}

// entryScript extracts the interpreted entry file a script-runtime dev-run vector
// compiles: the first argument after the interpreter ([python, main.py, --verbose]
// -> main.py, [node, index.js] -> index.js), so trailing program args are never
// mistaken for the entry. An interpreter option in the entry position (`-m module`,
// `-c code`, `-e code`) has no single source file to compile and is unbuildable, as
// is an interpreter with no script at all.
func entryScript(rt build.Runtime, run []string) (string, error) {
	if len(run) < 2 {
		return "", fmt.Errorf("unbuildable %s run vector %v: no entry script after the interpreter", rt, run)
	}
	script := run[1]
	if strings.HasPrefix(script, "-") {
		return "", fmt.Errorf("unbuildable %s run vector %v: %q is an interpreter option, not a source file (module/inline forms have no single source file to compile)", rt, run, script)
	}
	return script, nil
}

// exitString renders a toolchain's terminal status for the build error.
func exitString(status exec.ExitStatus) string {
	if status.Signaled {
		return fmt.Sprintf("killed by signal %v", status.Signal)
	}
	return fmt.Sprintf("exit code %d", status.Code)
}

// diagnosticTail renders the toolchain's captured output for a failed build's
// error message, preferring stderr and bounding the tail so a chatty compiler
// cannot flood the error.
func diagnosticTail(stderr, stdout *bytes.Buffer) string {
	out := strings.TrimSpace(stderr.String())
	if out == "" {
		out = strings.TrimSpace(stdout.String())
	}
	if out == "" {
		return ""
	}
	// Bound the tail on a rune boundary so a multi-byte character is never split.
	const bound = 512
	if r := []rune(out); len(r) > bound {
		out = "..." + string(r[len(r)-bound:])
	}
	return ": " + out
}
