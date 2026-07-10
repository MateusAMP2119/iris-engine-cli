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

// TestLintMalformedAnnotation proves a near-miss // spec: annotation -- a spec
// marker whose token is present but not a well-formed contract id (a trailing
// period, an uppercase slug, a malformed shape) -- is surfaced as a lint
// violation rather than silently dropped. Even when the same test carries another
// valid claim (so its claim list is non-empty and the "claims no contract" rule
// stays quiet), the near-miss must be reported so the contract it meant to claim
// can never linger in the backlog unnoticed. The gate fails on it.
//
// spec: S16/test-without-contract-fails-lint
func TestLintMalformedAnnotation(t *testing.T) {
	m := smallManifest()
	files := mustParse(t, "nearmiss_test.go", `package s
import "testing"

// spec: S02/admin-dsn-precedence
// spec: S01/apply-never-builds.
func TestNearMiss(t *testing.T) {}
`)

	// The valid S02 claim keeps the func's claim list non-empty, so only a
	// dedicated malformed-annotation lint can surface the S01 near-miss.
	errs := trace.Lint(m, files)
	var found bool
	for _, e := range errs {
		if strings.Contains(e.Error(), "S01/apply-never-builds.") {
			found = true
		}
	}
	if !found {
		t.Fatalf("malformed // spec: annotation not flagged by lint; got %v", errs)
	}

	content := []byte("body")
	lock := trace.SpecLock{SpecPath: "d", SHA256: trace.Fingerprint(content)}
	rep := trace.Gate(m, files, lock, content, trace.Backlog)
	if !rep.Failed() {
		t.Fatal("backlog gate: Failed() = false, want true from the malformed annotation")
	}
	if rep.Err() == nil || !strings.Contains(rep.Err().Error(), "S01/apply-never-builds.") {
		t.Errorf("gate error = %v, want it to name the malformed token", rep.Err())
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

	// Fingerprint normalizes line endings only -- CRLF and a lone CR both to LF --
	// so it is stable across line-ending churn yet a lone CR is still real content:
	// "a\rb" must not collapse onto "ab".
	if trace.Fingerprint([]byte("a\r\nb")) != trace.Fingerprint([]byte("a\nb")) {
		t.Error("Fingerprint distinguishes CRLF from LF, want them normalized equal")
	}
	if trace.Fingerprint([]byte("a\rb")) != trace.Fingerprint([]byte("a\nb")) {
		t.Error("Fingerprint does not normalize a lone CR to LF")
	}
	if trace.Fingerprint([]byte("a\rb")) == trace.Fingerprint([]byte("ab")) {
		t.Error(`Fingerprint collapsed a lone CR to nothing; "a\rb" and "ab" must differ`)
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

// TestGateMath is the repo-state-independent proof of the gate's gap arithmetic:
// a hand-built manifest with a known claim set -- extracted from fixture source
// through the real ParseTestFile -> ClaimedIDs -> GapList pipeline -- must yield
// exactly the expected unclaimed ids, in manifest order. The expected gaps are
// hard-coded, not recomputed from ClaimedIDs, so an extraction regression that
// over-matches (marking more contracts claimed than the source does) shrinks the
// gap list and fails here loudly. The fixture plants two distractors -- an
// id-shaped string in a non-subtest call and a Run on a local struct -- whose
// contracts must therefore stay in the gap list.
//
// spec: S16/gate-fails-unclaimed-contract
func TestGateMath(t *testing.T) {
	m := &spec.Manifest{Contracts: []spec.Contract{
		{ID: "S01/alpha", Anchor: "d#1", Tier: spec.TierUnit, Status: spec.StatusUnclaimed},
		{ID: "S02/beta", Anchor: "d#2", Tier: spec.TierUnit, Status: spec.StatusUnclaimed},
		{ID: "S03/gamma", Anchor: "d#3", Tier: spec.TierIntegration, Status: spec.StatusUnclaimed},
		{ID: "S04/delta", Anchor: "d#4", Tier: spec.TierUnit, Status: spec.StatusUnclaimed},
		{ID: "S05/epsilon", Anchor: "d#5", Tier: spec.TierUnit, Status: spec.StatusUnclaimed},
		{ID: "S16/doctrine", Anchor: "d#16", Tier: spec.TierExempt, Status: spec.StatusExempt},
	}}
	if err := m.Validate(); err != nil {
		t.Fatalf("synthetic manifest invalid: %v", err)
	}

	// Two of the five testable contracts are claimed: S02 by annotation, S04 by a
	// subtest. The fixture also plants distractors that must NOT claim: an
	// id-shaped string argument (S05) and a Run on a local struct (S01).
	files := mustParse(t, "math_test.go", `package s
import "testing"

type runner struct{}

func (runner) Run(name string, fn func()) {}

// spec: S02/beta
func TestBeta(t *testing.T) {}

func TestDelta(t *testing.T) {
	t.Run("S04/delta", func(t *testing.T) {})
	_ = decode("S05/epsilon")
	var r runner
	r.Run("S01/alpha", func() {})
}
`)

	content := []byte("spec body")
	lock := trace.SpecLock{SpecPath: "d", SHA256: trace.Fingerprint(content)}
	rep := trace.Gate(m, files, lock, content, trace.Backlog)

	if rep.Failed() {
		t.Fatalf("backlog gate over the synthetic repo failed: %v", rep.Err())
	}
	// Exactly the three unclaimed testable ids, in manifest order: the exempt row
	// is absent, the two claimed ids (S02, S04) are absent, and the two distractor
	// ids (S01, S05) remain because their id-shaped strings were never real claims.
	if want := []string{"S01/alpha", "S03/gamma", "S05/epsilon"}; !equalStrings(rep.Gaps, want) {
		t.Errorf("gaps = %v, want %v", rep.Gaps, want)
	}
}

// TestTraceabilityGate is the live smoke test: the gate runs over the seeded
// manifest in backlog-aware mode and is green, with the real claims recognized
// end-to-end and a known-unclaimed far-future contract still surfaced in the gap
// list. The exact gap arithmetic is proven repo-state-independently by
// TestGateMath; here we pin only anchors that do not decay as the backlog shrinks.
//
// spec: S16/gate-fails-unclaimed-contract
// spec: S16/claims-via-subtest-or-annotation
func TestTraceabilityGate(t *testing.T) {
	m, files, lock, content := loadRepo(t)

	rep := trace.Gate(m, files, lock, content, trace.Backlog)
	if rep.Failed() {
		t.Fatalf("backlog-aware gate over the real repo failed: %v", rep.Err())
	}
	claimed := trace.ClaimedIDs(files)
	t.Logf("traceability backlog: %d unclaimed non-exempt contracts", len(rep.Gaps))

	// Claims are recognized end-to-end: the six contracts real tests claim are
	// recognized and excluded from the gap list.
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

	// The live guard against a claim-extraction regression that silently empties the
	// gap list: given a genuinely-unclaimed non-exempt contract, the gate must still
	// surface it. This used to borrow a known-unclaimed far-future manifest row
	// (S13/scenario-passes-unattended, the last epic's acceptance scenario) and assert
	// it stayed in the gaps. E13 (the final task) claims that row -- and the last three
	// S13 acceptance-scenario rows with it -- so the real backlog is now empty and no
	// live row can serve as the probe. Doctrine (specification section 16, the
	// traceability-gate hazard) resolves this by synthesizing the probe rather than
	// leaning on a decaying live row: a fabricated unclaimed non-exempt contract,
	// appended to a copy of the real manifest and run against the REAL test files, must
	// surface in the gap list. No real test can claim a fabricated id, so if the gate
	// still reports it the claim extraction over the real file set is intact; a
	// regression that silently emptied the gaps fails here. This proof never decays: it
	// depends on no contract staying unclaimed.
	const probeID = "S99/gate-reports-unclaimed-probe"
	if _, ok := m.Find(probeID); ok {
		t.Fatalf("probe id %s unexpectedly exists in the real manifest; pick an id no real contract uses", probeID)
	}
	probed := &spec.Manifest{Contracts: append(append([]spec.Contract(nil), m.Contracts...),
		spec.Contract{
			ID:     probeID,
			Anchor: "docs/Iris Specification Inventory.md#16-testing-strategy-spec-driven-conformance",
			Tier:   spec.TierConformance,
			Status: spec.StatusUnclaimed,
		})}
	probeRep := trace.Gate(probed, files, lock, content, trace.Backlog)
	if probeRep.Failed() {
		t.Fatalf("backlog-aware gate over the probed manifest failed: %v", probeRep.Err())
	}
	if claimed[probeID] {
		t.Fatalf("fabricated probe id %s is claimed by a real test; it must be unclaimable", probeID)
	}
	if !contains(probeRep.Gaps, probeID) {
		t.Errorf("fabricated unclaimed contract %s is missing from the gap list; the gate is under-reporting the backlog (claim extraction may be silently emptying the gaps)", probeID)
	}
}

// TestTraceabilityGateStrict is the definition-of-done invocation. E13 (the final
// task) is landed, so the real backlog is empty and strict mode over the real repo
// now PASSES: the spec's definition of done -- "the sample passes that scenario
// unattended" (section 13) -- is met, and the suite is its executable proof (section
// 16, "implementation correct exactly when suite green"). The regression protection
// that strict mode still FAILS on a genuine gap is preserved with a synthesized
// probe, so a claim-extraction regression that silently emptied the gaps cannot make
// strict spuriously pass. Skipped in the ordinary suite (so `go test ./...` never
// runs the strict definition-of-done gate); run explicitly with IRIS_TRACE_STRICT=1.
//
// spec: S16/gate-fails-unclaimed-contract
func TestTraceabilityGateStrict(t *testing.T) {
	if os.Getenv("IRIS_TRACE_STRICT") == "" {
		t.Skip("set IRIS_TRACE_STRICT=1 to run the strict definition-of-done gate")
	}
	m, files, lock, content := loadRepo(t)

	// Definition of done: the real backlog is empty, so strict mode passes over the
	// real repo.
	rep := trace.Gate(m, files, lock, content, trace.Strict)
	if rep.Failed() {
		t.Fatalf("strict gate over the real repo failed, but the backlog should be empty (definition of done met): %d gaps: %v", len(rep.Gaps), rep.Gaps)
	}

	// Regression protection: strict mode still fails on a genuine gap. A fabricated
	// unclaimed non-exempt contract -- unclaimable by any real test -- must make strict
	// fail, so an extraction regression that empties the gaps cannot mask a real gap.
	const probeID = "S99/gate-strict-probe"
	probed := &spec.Manifest{Contracts: append(append([]spec.Contract(nil), m.Contracts...),
		spec.Contract{
			ID:     probeID,
			Anchor: "docs/Iris Specification Inventory.md#16-testing-strategy-spec-driven-conformance",
			Tier:   spec.TierConformance,
			Status: spec.StatusUnclaimed,
		})}
	probeRep := trace.Gate(probed, files, lock, content, trace.Strict)
	if !probeRep.Failed() {
		t.Fatal("strict gate did not fail on a fabricated unclaimed contract; strict mode is not enforcing the backlog")
	}
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
