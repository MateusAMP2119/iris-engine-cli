// Package buildcheck is the Iris integration-tier build harness: it proves the
// engine's two build-configuration contracts by driving the Go toolchain as a
// local subprocess (no Postgres, no daemon) and by reading the module's own build
// wiring -- go.mod and the CI workflow.
//
//   - Cgo-free static binary: building ./cmd/iris with cgo disabled succeeds
//     and yields a portable, statically linked binary. Static linkage is asserted
//     platform-conditionally: on linux the binary must have no dynamic
//     dependencies (ldd reports "not a dynamic executable"); on darwin the Go
//     runtime always links the system libSystem/libresolv dylibs even cgo-free, so
//     a zero-dynamic-deps assertion cannot hold there and the honest proof is the
//     recorded CGO_ENABLED=0 build setting plus a running binary.
//   - Go version floor and target: the module compiles under the floor
//     toolchain Go 1.25 and the target toolchain Go 1.26. The two-toolchain proof
//     is CI's job matrix (pinned here by reading .github/workflows/ci.yml);
//     locally the honest legs are go.mod's floor directive plus a real build under
//     the one toolchain present.
//
// This is test-support scaffolding (a harness package): shipped code never
// imports it. The parsing helpers here are pure, so the tests can feed them the
// real toolchain and file output they capture.
package buildcheck

import (
	"fmt"
	"strings"

	"github.com/goccy/go-yaml"
)

// floorGoVersion is the module's floor toolchain: the minimum Go the engine
// compiles under and the lower end of the CI build matrix. Floor 1.25, target
// 1.26; CI builds both.
const floorGoVersion = "1.25"

// targetGoVersion is the module's target toolchain: the upper end of the CI
// build matrix.
const targetGoVersion = "1.26"

// parseBuildSettings extracts the `build` key=value settings embedded in a
// binary's build info, as printed by `go version -m <binary>`. Each such line is
// tab-indented "build<TAB>KEY=VALUE"; a leading key without a '=' is ignored. The
// returned map lets a test assert, for example, CGO_ENABLED=0 -- the recorded
// proof that a build was actually cgo-free.
func parseBuildSettings(raw []byte) map[string]string {
	settings := make(map[string]string)
	for _, line := range strings.Split(string(raw), "\n") {
		fields := strings.Split(strings.TrimSpace(line), "\t")
		if len(fields) < 2 || fields[0] != "build" {
			continue
		}
		k, v, ok := strings.Cut(fields[1], "=")
		if !ok {
			continue
		}
		settings[k] = v
	}
	return settings
}

// staticLinkageReported reports whether ldd output describes a binary with no
// dynamic dependencies. GNU ldd prints "not a dynamic executable" for a fully
// static binary (and exits non-zero, which the caller treats as data, not
// failure); some ldd variants instead print "statically linked".
func staticLinkageReported(lddOutput string) bool {
	l := strings.ToLower(lddOutput)
	return strings.Contains(l, "not a dynamic executable") ||
		strings.Contains(l, "statically linked")
}

// goDirective returns the version named by go.mod's `go` directive (e.g.
// "1.25"), or "" when absent. The directive is the module's floor toolchain.
func goDirective(goMod []byte) string {
	for _, line := range strings.Split(string(goMod), "\n") {
		fields := strings.Fields(line)
		if len(fields) >= 2 && fields[0] == "go" {
			return fields[1]
		}
	}
	return ""
}

// ciWorkflow is the slice of a GitHub Actions workflow this harness reads: each
// job's env, its build-matrix Go versions, and its steps' env. It captures only
// the fields the build contracts pin; everything else in the YAML is ignored.
type ciWorkflow struct {
	Jobs map[string]ciJob `yaml:"jobs"`
}

// ciJob is one workflow job: the fields the build contracts inspect.
type ciJob struct {
	Env      map[string]string `yaml:"env"`
	Strategy struct {
		Matrix struct {
			Go []string `yaml:"go"`
		} `yaml:"matrix"`
	} `yaml:"strategy"`
	Steps []ciStep `yaml:"steps"`
}

// ciStep is one job step: the action it invokes (uses), the inputs passed to that
// action (with), and its env -- the fields the build contracts inspect.
type ciStep struct {
	Uses string `yaml:"uses"`
	With struct {
		GoVersion string `yaml:"go-version"`
	} `yaml:"with"`
	Env map[string]string `yaml:"env"`
}

// parseCIWorkflow parses a GitHub Actions workflow YAML into the slice the build
// contracts inspect.
func parseCIWorkflow(raw []byte) (*ciWorkflow, error) {
	var wf ciWorkflow
	if err := yaml.Unmarshal(raw, &wf); err != nil {
		return nil, fmt.Errorf("buildcheck: parse CI workflow: %w", err)
	}
	return &wf, nil
}

// cgoDisabled reports whether the job pins CGO_ENABLED=0, at the job level or on
// any of its steps: the pin that keeps every cross-compiled artifact cgo-free and
// therefore a portable static binary, rather than leaving it to the toolchain's
// implicit cross-compile default.
func (j ciJob) cgoDisabled() bool {
	if j.Env["CGO_ENABLED"] == "0" {
		return true
	}
	for _, s := range j.Steps {
		if s.Env["CGO_ENABLED"] == "0" {
			return true
		}
	}
	return false
}

// matrixGoTemplate is the GitHub Actions expression a setup-go step must pass to
// go-version so the declared build matrix actually drives the installed
// toolchain.
const matrixGoTemplate = "${{ matrix.go }}"

// setupGoConsumesMatrix reports whether the job's actions/setup-go step takes its
// toolchain from the build matrix (go-version: ${{ matrix.go }}), binding the
// declared matrix to the step that installs Go. Without this binding a hardcoded
// go-version would run every matrix cell on one toolchain, so the floor would
// never compile in CI even though the matrix still declared it.
func (j ciJob) setupGoConsumesMatrix() bool {
	for _, s := range j.Steps {
		if strings.HasPrefix(s.Uses, "actions/setup-go") && s.With.GoVersion == matrixGoTemplate {
			return true
		}
	}
	return false
}
