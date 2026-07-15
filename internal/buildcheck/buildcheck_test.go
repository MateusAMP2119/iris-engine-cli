package buildcheck

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"testing"
)

// irisPkg is the import path of the iris command the cgo-free build compiles.
// Building by import path (rather than a relative ./cmd/iris) keeps the build
// independent of the test's working directory within the module.
const irisPkg = "github.com/MateusAMP2119/iris-lakehouse/cmd/iris"

// repoRoot is the module root relative to this package's directory
// (internal/buildcheck): where the tests read go.mod and the CI workflow, and
// from where they drive `go build ./...`.
func repoRoot() string { return filepath.Join("..", "..") }

// hardcodedGoVersionWorkflow is a drift fixture: a test job that still declares
// the {1.25, 1.26} matrix but pins its setup-go to a literal version, so both
// cells would run the target and the floor would never compile. The
// matrix-consumption check must reject it.
const hardcodedGoVersionWorkflow = `
jobs:
  test:
    strategy:
      matrix:
        go: ["1.25", "1.26"]
    steps:
      - uses: actions/checkout@v4
      - uses: actions/setup-go@v5
        with:
          go-version: "1.26"
          cache: true
      - name: unit + integration
        run: go test ./...
`

// TestCGOFreeStaticBinary proves that building the engine with cgo disabled
// succeeds and yields a portable, statically linked binary. The build-settings
// leg (the recorded CGO_ENABLED=0) and the running binary are asserted on every
// platform; the strict "no dynamic dependencies" leg is linux-only, because on
// darwin the Go runtime always links the system libSystem/libresolv dylibs even
// with cgo off, so a zero-dynamic-deps assertion cannot hold there. The CI leg
// pins the same discipline for the cross-compile matrix by requiring the build
// job to set CGO_ENABLED=0, so every shipped artifact is static, not just this
// local build.
func TestCGOFreeStaticBinary(t *testing.T) {
	bin := filepath.Join(t.TempDir(), "iris")

	// Build ./cmd/iris with cgo explicitly disabled.
	build := exec.Command("go", "build", "-o", bin, irisPkg)
	build.Env = append(os.Environ(), "CGO_ENABLED=0")
	if out, err := build.CombinedOutput(); err != nil {
		t.Fatalf("CGO_ENABLED=0 go build failed: %v\n%s", err, out)
	}

	// The build records CGO_ENABLED=0 in the binary's embedded build info: the
	// cross-platform proof that cgo was actually off.
	info, err := exec.Command("go", "version", "-m", bin).Output()
	if err != nil {
		t.Fatalf("go version -m: %v", err)
	}
	if got := parseBuildSettings(info)["CGO_ENABLED"]; got != "0" {
		t.Fatalf("recorded CGO_ENABLED = %q, want 0 (build was not cgo-free)", got)
	}

	// The static binary is a real, runnable executable on this host (portability).
	// Its exit code is not asserted -- only that the OS can start and run it, so
	// this stays robust as the command tree replaces the placeholder main.
	if err := exec.Command(bin).Run(); err != nil {
		var exitErr *exec.ExitError
		if !errors.As(err, &exitErr) {
			t.Fatalf("built static binary did not run: %v", err)
		}
	}

	// Strict linkage: linux only. A cgo-free Go binary on linux is fully static,
	// so ldd reports no dynamic dependencies. On darwin the system dylibs are
	// always present, so this leg does not apply and the build-settings leg above
	// is the platform's proof.
	if runtime.GOOS == "linux" {
		ldd, err := exec.LookPath("ldd")
		if err != nil {
			t.Skipf("ldd unavailable to prove static linkage on linux: %v", err)
		}
		// ldd exits non-zero for a static binary; the reported text is the signal,
		// not the exit code.
		out, _ := exec.Command(ldd, bin).CombinedOutput()
		if !staticLinkageReported(string(out)) {
			t.Fatalf("ldd does not report a static binary (dynamic dependencies present):\n%s", out)
		}
	}

	// CI leg: the cross-compile build job pins CGO_ENABLED=0, so the shipped
	// per-platform artifacts are portable static binaries by construction.
	build2, ok := loadCIWorkflow(t).Jobs["build"]
	if !ok {
		t.Fatal("CI workflow has no build job")
	}
	if !build2.cgoDisabled() {
		t.Error("CI build job does not pin CGO_ENABLED=0; cross-compiled artifacts are not guaranteed cgo-free")
	}
}

// TestGoVersionFloorAndTarget proves the module compiles under the floor
// toolchain (Go 1.25) and the target toolchain (Go 1.26). The genuine
// two-toolchain proof is CI's job matrix, pinned here by reading
// .github/workflows/ci.yml and asserting the unit+integration matrix is exactly
// {1.25, 1.26}. Locally only one toolchain is present, so the honest local legs
// are: go.mod declares the floor (go 1.25), and the whole module builds under the
// one toolchain that is running. Together they bind both ends of the contract --
// the CI matrix (the real cross-version proof) and a real build here -- so a
// regression at either end fails this test.
func TestGoVersionFloorAndTarget(t *testing.T) {
	// go.mod declares the floor toolchain.
	goMod, err := os.ReadFile(filepath.Join(repoRoot(), "go.mod")) //nolint:gosec // G304: path is the repo-controlled go.mod, joined from a constant filename, never user or network input.
	if err != nil {
		t.Fatalf("read go.mod: %v", err)
	}
	if got := goDirective(goMod); got != floorGoVersion {
		t.Errorf("go.mod go directive = %q, want floor %q", got, floorGoVersion)
	}

	// CI's unit+integration matrix builds both the floor and the target: the leg
	// that actually compiles under two toolchains. Pin it so a matrix regression
	// (a dropped or added version) fails this contract's test.
	test, ok := loadCIWorkflow(t).Jobs["test"]
	if !ok {
		t.Fatal("CI workflow has no test job")
	}
	if !equalStringSet(test.Strategy.Matrix.Go, []string{floorGoVersion, targetGoVersion}) {
		t.Errorf("CI test matrix Go = %v, want exactly {%s, %s}",
			test.Strategy.Matrix.Go, floorGoVersion, targetGoVersion)
	}

	// The declared matrix is inert unless the setup-go step consumes it: the step
	// must take go-version from ${{ matrix.go }}. Otherwise a hardcoded version
	// would run every matrix cell on one toolchain and the floor would never
	// compile in CI, even though the matrix still declared it.
	if !test.setupGoConsumesMatrix() {
		t.Error("CI test job's setup-go does not consume the matrix (want go-version: ${{ matrix.go }}); a hardcoded version would leave the floor uncompiled in CI")
	}

	// Negative guard: the same check must REJECT a workflow that still declares the
	// {1.25, 1.26} matrix but pins setup-go to a literal version -- the exact drift
	// vector the positive assertion defends against -- so the check has teeth.
	drifted, err := parseCIWorkflow([]byte(hardcodedGoVersionWorkflow))
	if err != nil {
		t.Fatalf("parse drift fixture: %v", err)
	}
	if drifted.Jobs["test"].setupGoConsumesMatrix() {
		t.Error("setupGoConsumesMatrix accepted a hardcoded go-version; the matrix-consumption check has no teeth")
	}

	// A real compile under the toolchain that is running: the whole module builds.
	compile := exec.Command("go", "build", "./...")
	compile.Dir = repoRoot()
	if out, err := compile.CombinedOutput(); err != nil {
		t.Fatalf("go build ./... under %s failed: %v\n%s", runtime.Version(), err, out)
	}
}

// loadCIWorkflow reads and parses .github/workflows/ci.yml from the repo root.
func loadCIWorkflow(t *testing.T) *ciWorkflow {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join(repoRoot(), ".github", "workflows", "ci.yml")) //nolint:gosec // G304: path is the repo-controlled CI workflow, joined from constant path elements, never user or network input.
	if err != nil {
		t.Fatalf("read CI workflow: %v", err)
	}
	wf, err := parseCIWorkflow(raw)
	if err != nil {
		t.Fatalf("parse CI workflow: %v", err)
	}
	return wf
}

// equalStringSet reports whether got and want hold the same multiset of strings,
// order-independent: the matrix must carry exactly the wanted versions, no more
// and no fewer.
func equalStringSet(got, want []string) bool {
	if len(got) != len(want) {
		return false
	}
	seen := make(map[string]int, len(got))
	for _, g := range got {
		seen[g]++
	}
	for _, w := range want {
		if seen[w] == 0 {
			return false
		}
		seen[w]--
	}
	return true
}
