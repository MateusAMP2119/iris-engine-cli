package trace_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/MateusAMP2119/iris-engine-cli/internal/spec"
	"github.com/MateusAMP2119/iris-engine-cli/internal/trace"
)

// smallManifest is a hand-built manifest for the pure gate-logic tests: two
// behavioral (non-exempt) contracts and one exempt doctrine row.
func smallManifest() *spec.Manifest {
	return &spec.Manifest{Contracts: []spec.Contract{
		{ID: "S01/apply-never-builds", Anchor: "d#1", Tier: spec.TierIntegration, Status: spec.StatusUnclaimed},
		{ID: "S02/admin-dsn-precedence", Anchor: "d#2", Tier: spec.TierUnit, Status: spec.StatusUnclaimed},
		{ID: "S16/spec-driven-doctrine", Anchor: "d#16", Tier: spec.TierExempt, Status: spec.StatusExempt},
	}}
}

// TestGapList proves the manifest->tests direction: given a non-exempt contract
// no test claims, it appears in the gap list (the TDD backlog), while claimed
// contracts and exempt rows never do.
//
// spec: S16/gate-fails-unclaimed-contract
func TestGapList(t *testing.T) {
	m := smallManifest()

	// Nothing claimed: both behavioral rows are gaps, the exempt row is not.
	gaps := trace.GapList(m, map[string]bool{})
	if want := []string{"S01/apply-never-builds", "S02/admin-dsn-precedence"}; !equalStrings(gaps, want) {
		t.Fatalf("empty-claim gaps = %v, want %v", gaps, want)
	}

	// One claimed: it drops out of the gap list, the other remains.
	gaps = trace.GapList(m, map[string]bool{"S02/admin-dsn-precedence": true})
	if want := []string{"S01/apply-never-builds"}; !equalStrings(gaps, want) {
		t.Fatalf("partial-claim gaps = %v, want %v", gaps, want)
	}

	// An exempt contract never demands a claim, so claiming nothing leaves it
	// out of the gap list regardless.
	for _, id := range gaps {
		if id == "S16/spec-driven-doctrine" {
			t.Errorf("exempt row %s appeared in the gap list", id)
		}
	}
}

// TestGateStrictVsBacklog proves the two-mode resolution: an unclaimed non-exempt
// contract fails the gate in strict mode (the definition-of-done invocation) but
// is only surfaced as backlog -- never a failure -- in the default backlog-aware
// mode CI runs during buildout.
//
// spec: S16/gate-fails-unclaimed-contract
func TestGateStrictVsBacklog(t *testing.T) {
	m := smallManifest()

	// A test claims one of the two behavioral contracts; the other is backlog.
	files := mustParse(t, "cover_test.go", `package s
import "testing"
// spec: S02/admin-dsn-precedence
func TestCover(t *testing.T) {}
`)
	content := []byte("spec body")
	lock := trace.SpecLock{SpecPath: "d", SHA256: trace.Fingerprint(content)}

	strict := trace.Gate(m, files, lock, content, trace.Strict)
	if !strict.Failed() {
		t.Fatal("strict gate: Failed() = false, want true with a live gap")
	}
	if !contains(strict.Gaps, "S01/apply-never-builds") {
		t.Errorf("strict gaps = %v, want it to contain S01/apply-never-builds", strict.Gaps)
	}
	if strict.Err() == nil {
		t.Error("strict gate: Err() = nil, want a failure naming the gap")
	}

	backlog := trace.Gate(m, files, lock, content, trace.Backlog)
	if backlog.Failed() {
		t.Fatalf("backlog gate: Failed() = true, want false; err = %v", backlog.Err())
	}
	// The gap is still reported as backlog, just not fatal.
	if !contains(backlog.Gaps, "S01/apply-never-builds") {
		t.Errorf("backlog gaps = %v, want it to still list the backlog contract", backlog.Gaps)
	}
}

// TestLint proves the tests->manifest direction: a test claiming an id absent
// from the manifest, and a test claiming nothing at all, both fail the lint
// direction (no invented behavior), while a test claiming a real contract passes.
//
// spec: S16/test-without-contract-fails-lint
func TestLint(t *testing.T) {
	m := smallManifest()

	files := mustParse(t, "lint_test.go", `package s
import "testing"

// spec: S02/admin-dsn-precedence
func TestGood(t *testing.T) {}

// spec: S99/invented-behavior
func TestInvented(t *testing.T) {}

func TestSilent(t *testing.T) {
	_ = 1
}
`)
	errs := trace.Lint(m, files)

	byFunc := map[string]trace.LintError{}
	for _, e := range errs {
		byFunc[e.Func] = e
	}
	if _, ok := byFunc["TestGood"]; ok {
		t.Errorf("TestGood claims a real contract but was flagged: %v", byFunc["TestGood"])
	}
	inv, ok := byFunc["TestInvented"]
	if !ok {
		t.Error("TestInvented claims an unknown contract but was not flagged")
	} else if inv.ID != "S99/invented-behavior" {
		t.Errorf("TestInvented lint id = %q, want S99/invented-behavior", inv.ID)
	}
	if _, ok := byFunc["TestSilent"]; !ok {
		t.Error("TestSilent claims no contract but was not flagged")
	}
}

// TestGateLintFails proves the gate fails (in either mode) when lint finds a test
// claiming no manifest contract, independent of the backlog gap list.
//
// spec: S16/test-without-contract-fails-lint
func TestGateLintFails(t *testing.T) {
	m := smallManifest()
	// Claim everything behavioral so the gap list is empty, then add an invented
	// claim: only lint can fail the gate here.
	files := mustParse(t, "x_test.go", `package s
import "testing"
// spec: S01/apply-never-builds
func TestA(t *testing.T) {}
// spec: S02/admin-dsn-precedence
func TestB(t *testing.T) {}
// spec: S99/not-real
func TestC(t *testing.T) {}
`)
	content := []byte("body")
	lock := trace.SpecLock{SpecPath: "d", SHA256: trace.Fingerprint(content)}

	rep := trace.Gate(m, files, lock, content, trace.Backlog)
	if len(rep.Gaps) != 0 {
		t.Fatalf("gaps = %v, want none (all behavioral rows claimed)", rep.Gaps)
	}
	if !rep.Failed() {
		t.Fatal("backlog gate: Failed() = false, want true from the lint violation")
	}
	if rep.Err() == nil || !strings.Contains(rep.Err().Error(), "S99/not-real") {
		t.Errorf("gate error = %v, want it to name the invented claim S99/not-real", rep.Err())
	}
}

// TestSpecDelta proves the spec-delta mechanism as pure logic: a fingerprint
// mismatch (the spec doc changed) fails verification, a match passes, and after
// the lock is re-recorded -- the machine-visible proxy for the accompanying test
// delta -- the same changed doc verifies clean again.
//
// spec: S16/spec-delta-without-test-fails-gate
func TestSpecDelta(t *testing.T) {
	v1 := []byte("**Q - Wipe scope?** A: lane-scoped.\n")
	v2 := []byte("**Q - Wipe scope?** A: workload-scoped.\n") // a behavioral delta

	lock := trace.SpecLock{SpecPath: "spec.md", SHA256: trace.Fingerprint(v1)}

	// No delta: the doc still hashes to the locked fingerprint.
	if err := lock.Verify(v1); err != nil {
		t.Errorf("Verify(unchanged) = %v, want nil", err)
	}
	// Spec delta without test delta: the lock still points at v1, so the changed
	// doc fails the gate.
	if err := lock.Verify(v2); err == nil {
		t.Error("Verify(changed) = nil, want a spec-delta error")
	}
	// Re-record the lock (the explicit update path taken alongside a test delta):
	// the same changed doc now verifies clean.
	relocked := trace.SpecLock{SpecPath: "spec.md", SHA256: trace.Fingerprint(v2)}
	if err := relocked.Verify(v2); err != nil {
		t.Errorf("Verify(changed, re-locked) = %v, want nil", err)
	}

	// Fingerprint ignores CR so it is stable across line-ending churn but changes
	// with real content.
	if trace.Fingerprint([]byte("a\r\nb")) != trace.Fingerprint([]byte("a\nb")) {
		t.Error("Fingerprint is sensitive to CR line endings, want it normalized")
	}
	if trace.Fingerprint(v1) == trace.Fingerprint(v2) {
		t.Error("Fingerprint collided on two different specs")
	}
}

// TestGateSpecDeltaFails proves the gate itself fails when the spec fingerprint
// drifts from the lock, with an otherwise clean manifest and suite.
//
// spec: S16/spec-delta-without-test-fails-gate
func TestGateSpecDeltaFails(t *testing.T) {
	m := smallManifest()
	files := mustParse(t, "y_test.go", `package s
import "testing"
// spec: S01/apply-never-builds
func TestA(t *testing.T) {}
// spec: S02/admin-dsn-precedence
func TestB(t *testing.T) {}
`)
	locked := []byte("original spec")
	changed := []byte("edited spec")
	lock := trace.SpecLock{SpecPath: "d", SHA256: trace.Fingerprint(locked)}

	rep := trace.Gate(m, files, lock, changed, trace.Backlog)
	if len(rep.Gaps) != 0 {
		t.Fatalf("gaps = %v, want none", rep.Gaps)
	}
	if len(rep.Lint) != 0 {
		t.Fatalf("lint = %v, want none", rep.Lint)
	}
	if !rep.Failed() {
		t.Fatal("gate: Failed() = false, want true from the spec delta")
	}
	if rep.SpecDelta == nil {
		t.Error("gate: SpecDelta = nil, want a spec-delta error")
	}
}

// --- The gate running for real over the seeded repo -------------------------

func repoRoot() string { return filepath.Join("..", "..") }

func loadRepo(t *testing.T) (*spec.Manifest, []*trace.TestFile, trace.SpecLock, []byte) {
	t.Helper()
	root := repoRoot()
	m, err := spec.Load(filepath.Join(root, "spec", "contracts.yaml"))
	if err != nil {
		t.Fatalf("load manifest: %v", err)
	}
	if err := m.Validate(); err != nil {
		t.Fatalf("validate manifest: %v", err)
	}
	files, err := trace.ParseTestDir(root)
	if err != nil {
		t.Fatalf("parse test dir: %v", err)
	}
	lock, err := trace.LoadSpecLock(filepath.Join(root, "spec", "inventory.lock"))
	if err != nil {
		t.Fatalf("load spec lock: %v", err)
	}
	content, err := os.ReadFile(filepath.Join(root, "docs", "Iris Specification Inventory.md"))
	if err != nil {
		t.Fatalf("read spec doc: %v", err)
	}
	return m, files, lock, content
}

// TestTraceabilityGate is the first green build: the gate runs over the seeded
// manifest in backlog-aware mode and is green, with the whole unclaimed backlog
// surfaced as the gap list and every real claim (this task's four plus E00.1's
// two) recognized end-to-end and excluded from it.
//
// spec: S16/gate-fails-unclaimed-contract
// spec: S16/claims-via-subtest-or-annotation
func TestTraceabilityGate(t *testing.T) {
	m, files, lock, content := loadRepo(t)

	rep := trace.Gate(m, files, lock, content, trace.Backlog)
	if rep.Failed() {
		t.Fatalf("backlog-aware gate over the real repo failed: %v", rep.Err())
	}

	// The full red backlog is the deliverable: hundreds of unclaimed contracts.
	if len(rep.Gaps) < 400 {
		t.Errorf("gap list = %d contracts, want the big unclaimed backlog", len(rep.Gaps))
	}
	t.Logf("traceability backlog: %d unclaimed non-exempt contracts", len(rep.Gaps))

	// Claims are recognized end-to-end: the six contracts real tests claim are
	// not in the gap list.
	claimed := trace.ClaimedIDs(files)
	for _, id := range []string{
		"S16/manifest-row-schema",
		"S16/exempt-needs-no-test",
		"S16/claims-via-subtest-or-annotation",
		"S16/gate-fails-unclaimed-contract",
		"S16/spec-delta-without-test-fails-gate",
		"S16/test-without-contract-fails-lint",
	} {
		if !claimed[id] {
			t.Errorf("contract %s is claimed by a real test but was not recognized", id)
		}
		if contains(rep.Gaps, id) {
			t.Errorf("contract %s is claimed but still appears in the gap list", id)
		}
	}
}

// TestTraceabilityGateStrict is the definition-of-done invocation: strict mode
// over the real repo fails while the backlog is non-empty. It is skipped in the
// ordinary suite (so `go test ./...` stays green while 480+ contracts are still
// backlog) and run explicitly by setting IRIS_TRACE_STRICT=1.
//
// spec: S16/gate-fails-unclaimed-contract
func TestTraceabilityGateStrict(t *testing.T) {
	if os.Getenv("IRIS_TRACE_STRICT") == "" {
		t.Skip("set IRIS_TRACE_STRICT=1 to run the strict definition-of-done gate")
	}
	m, files, lock, content := loadRepo(t)
	rep := trace.Gate(m, files, lock, content, trace.Strict)
	if !rep.Failed() {
		t.Fatal("strict gate over the real repo passed, want failure while the backlog is non-empty")
	}
	t.Logf("strict gate reports %d gaps", len(rep.Gaps))
}

// TestSpecLockUpdate is the explicit spec-lock update path: run with
// IRIS_TRACE_UPDATE_LOCK=1 to re-record spec/inventory.lock from the current
// spec doc (the step taken alongside a test delta after amending the spec).
// Skipped otherwise so it never mutates the tree during a normal run.
//
// spec: S16/spec-delta-without-test-fails-gate
func TestSpecLockUpdate(t *testing.T) {
	if os.Getenv("IRIS_TRACE_UPDATE_LOCK") == "" {
		t.Skip("set IRIS_TRACE_UPDATE_LOCK=1 to re-record spec/inventory.lock")
	}
	root := repoRoot()
	rel := filepath.Join("docs", "Iris Specification Inventory.md")
	content, err := os.ReadFile(filepath.Join(root, rel))
	if err != nil {
		t.Fatalf("read spec doc: %v", err)
	}
	lock := trace.SpecLock{SpecPath: rel, SHA256: trace.Fingerprint(content)}
	if err := lock.Write(filepath.Join(root, "spec", "inventory.lock")); err != nil {
		t.Fatalf("write spec lock: %v", err)
	}
	t.Logf("re-recorded spec/inventory.lock: %s", lock.SHA256)
}

// --- small test-only helpers -------------------------------------------------

func mustParse(t *testing.T, name, src string) []*trace.TestFile {
	t.Helper()
	tf, err := trace.ParseTestFile(name, []byte(src))
	if err != nil {
		t.Fatalf("ParseTestFile(%s): %v", name, err)
	}
	return []*trace.TestFile{tf}
}

func contains(xs []string, want string) bool {
	for _, x := range xs {
		if x == want {
			return true
		}
	}
	return false
}

func equalStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
