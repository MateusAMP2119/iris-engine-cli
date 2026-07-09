package dispatch_test

import (
	"context"
	"errors"
	"reflect"
	"testing"

	"github.com/MateusAMP2119/iris-engine-cli/internal/dispatch"
)

// succeeded builds an edge whose upstream's most recent run is an awaited success
// (AwaitedFrom left at zero, so every run is awaited unless the case overrides it).
func succeeded(upstream string, runID int64) dispatch.Edge {
	return dispatch.Edge{Upstream: upstream, Latest: dispatch.UpstreamSucceeded, LatestRunID: runID}
}

// fakeConsumed is an in-memory ConsumedReader: it answers the run_inputs
// already-consumed check from a set of consumed upstream run ids and records the
// lookups it was asked, so a test can prove the gate resolved consumption through the
// reader rather than a stored cursor. It performs no I/O, so the gate tests stay unit.
type fakeConsumed struct {
	consumed map[int64]bool
	calls    []int64
	err      error
}

func newFakeConsumed(ids ...int64) *fakeConsumed {
	set := map[int64]bool{}
	for _, id := range ids {
		set[id] = true
	}
	return &fakeConsumed{consumed: set}
}

func (f *fakeConsumed) Consumed(_ context.Context, _ string, upstreamRunID int64) (bool, error) {
	f.calls = append(f.calls, upstreamRunID)
	if f.err != nil {
		return false, f.err
	}
	return f.consumed[upstreamRunID], nil
}

// notConsumed is the parallel all-false consumed slice for a set of edges: the
// dependent has consumed none of their latest runs.
func notConsumed(edges []dispatch.Edge) []bool { return make([]bool, len(edges)) }

// TestGateEligibilityOnUpstreamOutput proves depends_on is an eligibility gate on the
// upstream's OUTPUT and nothing else: with an edge, the dependent is eligible (the
// gate opens) exactly when the upstream has produced a consumable success, and
// ineligible when it has not; with no depends_on edges at all, the pipeline is
// ungated and always eligible. The decision imposes no execution order -- it carries
// no ordering field -- so eligibility is a pure data check, never a sequence.
//
// spec: S01/depends-on-eligibility-gate
func TestGateEligibilityOnUpstreamOutput(t *testing.T) {
	// Upstream has output (a success): the gate opens, the dependent is eligible.
	edges := []dispatch.Edge{succeeded("extract_orders", 42)}
	if d := dispatch.Decide(edges, notConsumed(edges)); !d.Run {
		t.Errorf("upstream produced a success but the gate did not open: %+v", d)
	}

	// Upstream has NO output yet: the dependent is ineligible, no run row.
	noOutput := []dispatch.Edge{{Upstream: "extract_orders", Latest: dispatch.UpstreamNone}}
	if d := dispatch.Decide(noOutput, notConsumed(noOutput)); d.Run {
		t.Errorf("upstream produced no output but the gate opened: %+v", d)
	}

	// No depends_on edges: the pipeline is ungated and runs every pass on composer
	// order alone -- depends_on only ever RESTRICTS eligibility, never grants it.
	if d := dispatch.Decide(nil, nil); !d.Run {
		t.Errorf("a pipeline with no depends_on must be ungated (always eligible): %+v", d)
	}

	// Eligibility imposes no order: the decision carries no ordering/position field.
	assertNoOrderingField(t, reflect.TypeOf(dispatch.Decision{}))
}

// TestCrossLaneGateIsDataNotOrder proves a depends_on reference acts as a data gate --
// eligibility and failure propagation -- with no serial ordering, so the same rule
// holds whether the upstream sits in the same lane or another. The gate takes no lane
// or order input and returns no ordering field, so a cross-lane edge can only gate on
// data (the upstream's run), never sequence the lanes.
//
// spec: S03/cross-lane-gate-not-order
func TestCrossLaneGateIsDataNotOrder(t *testing.T) {
	// The gate's inputs carry no lane: an edge gates on upstream output alone.
	assertNoLaneField(t, reflect.TypeOf(dispatch.Edge{}))
	assertNoLaneField(t, reflect.TypeOf(dispatch.Decision{}))
	assertNoOrderingField(t, reflect.TypeOf(dispatch.Decision{}))

	// A cross-lane edge is eligibility: an upstream success opens the gate.
	crossLane := []dispatch.Edge{succeeded("upstream_in_other_lane", 7)}
	if d := dispatch.Decide(crossLane, notConsumed(crossLane)); !d.Run {
		t.Errorf("cross-lane upstream success did not open the eligibility gate: %+v", d)
	}

	// A cross-lane edge is also failure propagation: a dead-lettered upstream poisons
	// the dependent (rejection propagates) rather than being a mere ordering hint.
	poison := []dispatch.Edge{{Upstream: "upstream_in_other_lane", Latest: dispatch.UpstreamDeadLettered, LatestRunID: 8}}
	d := dispatch.Decide(poison, notConsumed(poison))
	if d.Run || !d.Poisoned {
		t.Errorf("cross-lane dead-lettered upstream did not propagate as poison: %+v", d)
	}
}

// TestGateNeverReordersWalk proves the gate decides only run-or-skip at the pipeline's
// composer-assigned turn and never changes its walk position: the Decision exposes no
// walk-position/order field, and the decision is a pure function of the edges, so it
// cannot depend on or alter any position in the walk.
//
// spec: S06.1/gate-never-reorders-walk
func TestGateNeverReordersWalk(t *testing.T) {
	// No position, order, turn, or index field: the gate returns run-or-skip, nothing
	// that could move a pipeline in the walk.
	assertNoOrderingField(t, reflect.TypeOf(dispatch.Decision{}))

	// The decision is a pure function of the edges: evaluating the same edges twice
	// yields the identical run/skip and consume list, independent of any turn.
	edges := []dispatch.Edge{succeeded("a", 1), succeeded("b", 2)}
	first := dispatch.Decide(edges, notConsumed(edges))
	second := dispatch.Decide(edges, notConsumed(edges))
	if !reflect.DeepEqual(first, second) {
		t.Errorf("gate decision is not a pure function of its edges: %+v vs %+v", first, second)
	}
	if !first.Run {
		t.Fatalf("expected the gate to open on two upstream successes: %+v", first)
	}
}

// TestGateConsumedCheckViaRunInputs proves the gate decides whether an upstream's
// latest success was already consumed by querying run_inputs (the ConsumedReader
// seam), with no mutable cursor: flipping the run_inputs answer flips the verdict
// between open and up_to_date, the reader is actually consulted, and the Gate holds no
// cursor/watermark field of its own.
//
// spec: S04/gate-consumed-check-via-run-inputs
func TestGateConsumedCheckViaRunInputs(t *testing.T) {
	ctx := context.Background()
	edges := []dispatch.Edge{succeeded("extract_orders", 42)}

	// run_inputs has no row for run 42: the gate opens (an unconsumed success).
	unconsumed := newFakeConsumed()
	d, err := dispatch.NewGate(unconsumed).Evaluate(ctx, "load_orders", edges)
	if err != nil {
		t.Fatalf("Evaluate: %v", err)
	}
	if !d.Run {
		t.Errorf("run_inputs lacks the row but the gate did not open: %+v", d)
	}
	if len(unconsumed.calls) == 0 {
		t.Error("gate did not query the run_inputs reader for the already-consumed check")
	}

	// run_inputs already records run 42: the same upstream latest is now up_to_date,
	// so the gate mints no run row. Only the run_inputs answer changed.
	consumed := newFakeConsumed(42)
	d, err = dispatch.NewGate(consumed).Evaluate(ctx, "load_orders", edges)
	if err != nil {
		t.Fatalf("Evaluate: %v", err)
	}
	if d.Run {
		t.Errorf("run_inputs records the consumed run but the gate opened again: %+v", d)
	}
	if d.Ledger[0].Verdict != dispatch.VerdictUpToDate {
		t.Errorf("already-consumed edge verdict = %v, want up_to_date", d.Ledger[0].Verdict)
	}

	// A reader error aborts the evaluation rather than deciding on a half-read check.
	failing := &fakeConsumed{err: errors.New("meta unreachable")}
	if _, err := dispatch.NewGate(failing).Evaluate(ctx, "load_orders", edges); err == nil {
		t.Error("a run_inputs read failure did not abort the gate evaluation")
	}

	// Structural: the gate holds no mutable cursor/watermark -- only the run_inputs
	// read seam. The consumed check is a query, never stored per-consumer state.
	assertNoCursorField(t, reflect.TypeOf(dispatch.Gate{}))
}

// TestGateAwaitsLatestSuccess proves the dependent's gate opens on an upstream success
// not yet recorded in its run_inputs and records exactly that run 1:1, with the same
// rule regardless of lane. A success already consumed is up_to_date, a still-pending
// upstream is pending, and a dead-lettered upstream is poisoned -- the gate opens on a
// SUCCESS and only a success.
//
// spec: S06.2/gate-awaits-latest-success
func TestGateAwaitsLatestSuccess(t *testing.T) {
	cases := []struct {
		name        string
		edge        dispatch.Edge
		consumed    bool
		wantRun     bool
		wantVerdict dispatch.Verdict
	}{
		{"unconsumed success opens", succeeded("a", 9), false, true, dispatch.VerdictOpen},
		{"consumed success is up_to_date", succeeded("a", 9), true, false, dispatch.VerdictUpToDate},
		{"pending upstream is pending", dispatch.Edge{Upstream: "a", Latest: dispatch.UpstreamPending, LatestRunID: 9}, false, false, dispatch.VerdictPending},
		{"no upstream run is pending", dispatch.Edge{Upstream: "a", Latest: dispatch.UpstreamNone}, false, false, dispatch.VerdictPending},
		{"dead-lettered upstream poisons", dispatch.Edge{Upstream: "a", Latest: dispatch.UpstreamDeadLettered, LatestRunID: 9}, false, false, dispatch.VerdictPoisoned},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			edges := []dispatch.Edge{tc.edge}
			d := dispatch.Decide(edges, []bool{tc.consumed})
			if d.Run != tc.wantRun {
				t.Errorf("Run = %v, want %v (%+v)", d.Run, tc.wantRun, d)
			}
			if d.Ledger[0].Verdict != tc.wantVerdict {
				t.Errorf("verdict = %v, want %v", d.Ledger[0].Verdict, tc.wantVerdict)
			}
			// On an open gate the dependent records exactly the awaited run, 1:1.
			if tc.wantRun {
				if want := []int64{tc.edge.LatestRunID}; !reflect.DeepEqual(d.Consume, want) {
					t.Errorf("consumed = %v, want exactly the awaited run %v (1:1)", d.Consume, want)
				}
			}
		})
	}
}

// TestMultiEdgeAllResolve proves that with several upstreams the dependent runs only
// when every depends_on edge resolves to an available success, and then records one
// run_inputs row per edge (each edge's latest success). A single unresolved edge
// (pending) blocks the run; an edge already up_to_date does not block a run another
// edge's new success triggers, but it is still recorded for the new run.
//
// spec: S06.2/multi-edge-all-resolve
func TestMultiEdgeAllResolve(t *testing.T) {
	// Every edge resolves to an unconsumed success: the dependent runs, one row per
	// edge, in edge order.
	all := []dispatch.Edge{succeeded("a", 10), succeeded("b", 11), succeeded("c", 12)}
	d := dispatch.Decide(all, notConsumed(all))
	if !d.Run {
		t.Fatalf("every edge resolved but the gate did not open: %+v", d)
	}
	if want := []int64{10, 11, 12}; !reflect.DeepEqual(d.Consume, want) {
		t.Errorf("consumed = %v, want one run per edge %v", d.Consume, want)
	}

	// One edge unresolved (pending): every edge must resolve, so no run row this pass.
	oneUnresolved := []dispatch.Edge{succeeded("a", 10), {Upstream: "b", Latest: dispatch.UpstreamPending, LatestRunID: 11}}
	if d := dispatch.Decide(oneUnresolved, notConsumed(oneUnresolved)); d.Run {
		t.Errorf("an unresolved edge did not block the run: %+v", d)
	}

	// One edge open (new), one already up_to_date: the new success triggers a run, and
	// the run records BOTH edges' latest -- one row per edge.
	mixed := []dispatch.Edge{succeeded("a", 20), succeeded("b", 21)}
	d = dispatch.Decide(mixed, []bool{false, true}) // b's latest already consumed
	if !d.Run {
		t.Fatalf("a new success on one edge did not trigger a run: %+v", d)
	}
	if want := []int64{20, 21}; !reflect.DeepEqual(d.Consume, want) {
		t.Errorf("consumed = %v, want one row per edge %v (including the up_to_date edge)", d.Consume, want)
	}
}

// TestNewEdgeAwaitsNextSuccess proves a newly added depends_on edge awaits the
// upstream's NEXT success from pass one and never consumes a run that predates the
// edge: an upstream success at or below the edge's establishment baseline is history
// (pending, no run row), and only a success minted after the edge opens the gate.
//
// spec: S06.2/new-edge-awaits-next-success
func TestNewEdgeAwaitsNextSuccess(t *testing.T) {
	// Edge established when the upstream's tip was run 50: run 50 is history.
	historical := []dispatch.Edge{{Upstream: "a", Latest: dispatch.UpstreamSucceeded, LatestRunID: 50, AwaitedFrom: 50}}
	d := dispatch.Decide(historical, notConsumed(historical))
	if d.Run {
		t.Errorf("a new edge consumed the upstream's pre-edge (historical) run: %+v", d)
	}
	if d.Ledger[0].Verdict != dispatch.VerdictPending {
		t.Errorf("pre-edge success verdict = %v, want pending (awaiting the next success)", d.Ledger[0].Verdict)
	}

	// The upstream's NEXT success (run 51, past the baseline) opens the gate.
	next := []dispatch.Edge{{Upstream: "a", Latest: dispatch.UpstreamSucceeded, LatestRunID: 51, AwaitedFrom: 50}}
	d = dispatch.Decide(next, notConsumed(next))
	if !d.Run {
		t.Fatalf("the upstream's next success did not open the new edge's gate: %+v", d)
	}
	if want := []int64{51}; !reflect.DeepEqual(d.Consume, want) {
		t.Errorf("consumed = %v, want only the post-edge run %v (never the historical run)", d.Consume, want)
	}
}

// TestNoNewUpstreamSkips proves the dependent gets no run row on a pass when the
// awaited upstream run is pending or when nothing new exists since it last consumed:
// both a still-in-flight upstream and an already-consumed latest success mint no run.
//
// spec: S06.2/no-new-upstream-skips
func TestNoNewUpstreamSkips(t *testing.T) {
	// Nothing new: the latest success was already consumed.
	upToDate := []dispatch.Edge{succeeded("a", 30)}
	if d := dispatch.Decide(upToDate, []bool{true}); d.Run {
		t.Errorf("nothing new since last consumed but a run row was minted: %+v", d)
	}

	// Awaited upstream still pending: no run row.
	pending := []dispatch.Edge{{Upstream: "a", Latest: dispatch.UpstreamPending, LatestRunID: 31}}
	if d := dispatch.Decide(pending, notConsumed(pending)); d.Run {
		t.Errorf("pending upstream minted a run row: %+v", d)
	}
}

// TestSupersededNotOwed proves never-awaited (and superseded) upstream successes are
// not owed: the gate reads only the upstream's LATEST run and consumes exactly it,
// with no backlog of the older successes it skipped. Structurally, an edge carries a
// single latest run, not a queue -- there is no buffer or head-of-line blocking.
//
// spec: S06.2/superseded-not-owed
func TestSupersededNotOwed(t *testing.T) {
	// Runs 61 and 62 succeeded and were never consumed, but 63 is now the latest: the
	// gate reads only 63 and consumes only 63; 61 and 62 are superseded, not queued.
	latest := []dispatch.Edge{succeeded("a", 63)}
	d := dispatch.Decide(latest, notConsumed(latest))
	if !d.Run {
		t.Fatalf("the latest success did not open the gate: %+v", d)
	}
	if want := []int64{63}; !reflect.DeepEqual(d.Consume, want) {
		t.Errorf("consumed = %v, want only the latest run %v -- older successes are superseded, not owed", d.Consume, want)
	}

	// No backlog: an edge holds a single latest run, not a slice/queue of pending
	// upstream runs, so the gate cannot buffer or head-of-line-block.
	assertNoBacklogField(t, reflect.TypeOf(dispatch.Edge{}))
}

// --- structural assertions --------------------------------------------------

// assertNoOrderingField fails when the struct carries a field whose name suggests a
// walk position, order, turn, or index -- the gate must impose none.
func assertNoOrderingField(t *testing.T, typ reflect.Type) {
	t.Helper()
	banned := map[string]bool{"pos": true, "position": true, "order": true, "turn": true, "index": true, "seq": true, "sequence": true}
	for i := 0; i < typ.NumField(); i++ {
		name := typ.Field(i).Name
		lower := toLower(name)
		if banned[lower] {
			t.Errorf("%s carries an ordering field %q; the gate must never impose or change walk position", typ.Name(), name)
		}
	}
}

// assertNoLaneField fails when the struct carries a lane field: the gate resolves on
// upstream output alone, so a lane input could only introduce ordering.
func assertNoLaneField(t *testing.T, typ reflect.Type) {
	t.Helper()
	for i := 0; i < typ.NumField(); i++ {
		if toLower(typ.Field(i).Name) == "lane" {
			t.Errorf("%s carries a lane field; a cross-lane edge must gate on data, never lane order", typ.Name())
		}
	}
}

// assertNoCursorField fails when the struct carries a mutable cursor/watermark field:
// the gate's already-consumed check is a run_inputs query, never stored state.
func assertNoCursorField(t *testing.T, typ reflect.Type) {
	t.Helper()
	for i := 0; i < typ.NumField(); i++ {
		f := typ.Field(i)
		lower := toLower(f.Name)
		if lower == "cursor" || lower == "watermark" || lower == "consumed" || lower == "highwater" {
			t.Errorf("%s carries a mutable cursor field %q; the consumed check must query run_inputs", typ.Name(), f.Name)
		}
		switch f.Type.Kind() {
		case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64,
			reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64:
			t.Errorf("%s carries an integer field %q of type %s; the gate must hold no cursor/watermark state", typ.Name(), f.Name, f.Type)
		}
	}
}

// assertNoBacklogField fails when the edge carries a slice of upstream runs: the gate
// reads only the latest run, so a backlog slice would be a buffer the spec forbids.
func assertNoBacklogField(t *testing.T, typ reflect.Type) {
	t.Helper()
	for i := 0; i < typ.NumField(); i++ {
		f := typ.Field(i)
		if f.Type.Kind() == reflect.Slice {
			t.Errorf("%s carries a slice field %q of type %s; the gate reads only the latest run, never a backlog", typ.Name(), f.Name, f.Type)
		}
	}
}

// toLower lowercases an ASCII identifier for case-insensitive field-name matching.
func toLower(s string) string {
	b := []byte(s)
	for i := range b {
		if b[i] >= 'A' && b[i] <= 'Z' {
			b[i] += 'a' - 'A'
		}
	}
	return string(b)
}
