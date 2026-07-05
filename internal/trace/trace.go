// Package trace is the Iris traceability gate: the executable form of the TDD
// doctrine that binds the spec inventory (the source of truth) to the test
// suite. It reads spec/contracts.yaml (via internal/spec), scans the repo's Go
// test files for contract claims, and walks both directions.
//
// # Claim syntax
//
// A test claims a contract two ways, both recognized here:
//
//   - a `// spec: <id>` annotation on the test's doc comment or in its body, and
//   - a subtest whose name is a contract id, t.Run("Sxx/slug", ...).
//
// A token counts as a claim only when it has contract-id shape (Sxx[.y]/slug),
// so ordinary subtest names and prose never register. Claims are found by
// parsing Go source, so a claim written inside a string literal (as this
// package's own test fixtures are) is invisible to the gate.
//
// # Directions and modes
//
// The gate always computes both directions. The manifest->tests direction is the
// gap list: every non-exempt contract with no claiming test (exempt rows -- the
// naming, rationale, and doctrine entries -- need none). The tests->manifest
// direction is lint: a test claiming an id absent from the manifest, or claiming
// nothing at all, is a violation (no invented behavior).
//
// Two modes resolve the tension between "an unclaimed contract fails the gate"
// and "the repo keeps merging while the seeded backlog is still red":
//
//   - Backlog (default, what CI runs during buildout): the gap list is the
//     declared TDD backlog, printed but not fatal. Failures are inconsistencies
//     only -- lint violations and spec deltas.
//   - Strict (the definition-of-done invocation): any non-exempt contract with no
//     claiming test fails the gate, with the gap list reported.
//
// # Spec-delta detection
//
// spec/inventory.lock records a fingerprint of the spec doc. The gate recomputes
// it and fails on drift: a spec delta without a corresponding test delta. The
// explicit update path (SpecLock.Write, via IRIS_TRACE_UPDATE_LOCK=1) re-records
// the lock, and is taken only alongside the test delta that admits the change.
package trace

import (
	"fmt"
	"strings"

	"github.com/MateusAMP2119/iris-engine-cli/internal/spec"
)

// Mode selects how the gate treats the unclaimed backlog.
type Mode int

// The two gate modes.
const (
	// Backlog treats unclaimed non-exempt contracts as the declared TDD backlog:
	// reported, not fatal. This is what CI runs during buildout.
	Backlog Mode = iota
	// Strict fails the gate on any unclaimed non-exempt contract: the
	// definition-of-done invocation.
	Strict
)

// String names the mode.
func (m Mode) String() string {
	if m == Strict {
		return "strict"
	}
	return "backlog"
}

// LintError is one tests->manifest violation: a test claiming an id absent from
// the manifest, or a test claiming no contract at all (ID is empty then).
type LintError struct {
	File string
	Line int
	Func string
	ID   string
	Msg  string
}

// Error renders the violation with its source location.
func (e LintError) Error() string {
	return fmt.Sprintf("%s:%d: %s: %s", e.File, e.Line, e.Func, e.Msg)
}

// GapList returns the non-exempt manifest contracts not present in claimed, in
// manifest order: the gap list, i.e. the TDD backlog.
func GapList(m *spec.Manifest, claimed map[string]bool) []string {
	var gaps []string
	for _, c := range m.Contracts {
		if c.NeedsClaimingTest() && !claimed[c.ID] {
			gaps = append(gaps, c.ID)
		}
	}
	return gaps
}

// Lint returns the tests->manifest violations across files: every test that
// claims an id with no manifest row, and every test that claims no contract at
// all. It is the "no invented behavior" direction.
func Lint(m *spec.Manifest, files []*TestFile) []LintError {
	var errs []LintError
	for _, tf := range files {
		for _, fn := range tf.TestFuncs {
			if len(fn.Claims) == 0 {
				errs = append(errs, LintError{
					File: fn.File, Line: fn.Line, Func: fn.Name,
					Msg: "test claims no contract (no invented behavior)",
				})
				continue
			}
			for _, c := range fn.Claims {
				if _, ok := m.Find(c.ID); !ok {
					errs = append(errs, LintError{
						File: c.File, Line: c.Line, Func: fn.Name, ID: c.ID,
						Msg: fmt.Sprintf("test claims %s, which is not a manifest contract", c.ID),
					})
				}
			}
		}
	}
	return errs
}

// Report is the outcome of a gate run. Gaps and Lint are always computed;
// SpecDelta is non-nil when the spec fingerprint drifted from the lock.
type Report struct {
	Mode      Mode
	Gaps      []string
	Lint      []LintError
	SpecDelta error
}

// Failed reports whether the gate fails in its mode. Lint violations and spec
// deltas fail both modes; a non-empty gap list fails only Strict.
func (r Report) Failed() bool {
	if len(r.Lint) > 0 || r.SpecDelta != nil {
		return true
	}
	return r.Mode == Strict && len(r.Gaps) > 0
}

// Err aggregates the failing conditions into one error, or nil when the gate
// passes. The gap list is included in the message only in Strict mode; in
// Backlog mode it is surfaced separately as declared backlog, never an error.
func (r Report) Err() error {
	if !r.Failed() {
		return nil
	}
	var b strings.Builder
	fmt.Fprintf(&b, "traceability gate failed (%s mode)", r.Mode)
	if r.SpecDelta != nil {
		fmt.Fprintf(&b, "\n  %s", r.SpecDelta)
	}
	for _, e := range r.Lint {
		fmt.Fprintf(&b, "\n  lint: %s", e)
	}
	if r.Mode == Strict && len(r.Gaps) > 0 {
		fmt.Fprintf(&b, "\n  %d unclaimed contract(s):", len(r.Gaps))
		for _, id := range r.Gaps {
			fmt.Fprintf(&b, "\n    %s", id)
		}
	}
	return fmt.Errorf("%s", b.String())
}

// Gate runs both directions plus the spec-delta check and returns the Report.
// It never fails the gap list in Backlog mode, so the seeded all-unclaimed
// manifest is a green build with the backlog as its reported deliverable.
func Gate(m *spec.Manifest, files []*TestFile, lock SpecLock, specContent []byte, mode Mode) Report {
	r := Report{
		Mode: mode,
		Gaps: GapList(m, ClaimedIDs(files)),
		Lint: Lint(m, files),
	}
	if err := lock.Verify(specContent); err != nil {
		r.SpecDelta = err
	}
	return r
}
