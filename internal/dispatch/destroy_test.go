package dispatch_test

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/MateusAMP2119/iris-engine-cli/internal/dispatch"
	"github.com/MateusAMP2119/iris-engine-cli/internal/store"
	"github.com/MateusAMP2119/iris-engine-cli/internal/store/storetest"
)

// This file proves the dispatch-level destroy op (specification section 12,
// destructive ops item 1): a scoped single-unit teardown that reverts the target's
// un-promoted disposable data, retires all of its meta rows in one transaction with
// the pipelines row deleted last, and honors the composer destroy interlock. Every
// write rides a real Dispatcher over a recording fake -- no live Postgres -- so a
// test asserts the exact retirement write set, its transaction grouping and order,
// and that the data-revert seam runs before the teardown and gates it.

// recordingReverter is a dispatch.DataReverter that records the pipelines it was
// asked to revert (the un-promoted disposable data seam E06 fills with real
// reverse-replay), and can be made to fail so a test proves the revert gates the
// teardown.
type recordingReverter struct {
	reverted []string
	fail     error
}

func (r *recordingReverter) RevertUnpromoted(_ context.Context, pipeline string) error {
	r.reverted = append(r.reverted, pipeline)
	return r.fail
}

// recordingObjectDeleter records the pipelines whose object-store bytes it was asked
// to delete (the seam E05/E07 fills with real content-addressed file deletion).
type recordingObjectDeleter struct {
	deleted []string
	fail    error
}

func (d *recordingObjectDeleter) DeleteObjects(_ context.Context, pipeline string) error {
	d.deleted = append(d.deleted, pipeline)
	return d.fail
}

// blockingBlocker is a dispatch.DestroyBlocker that always blocks, for proving the
// blocker seam gates the teardown (E10.1 supplies the real downstream predicates).
type blockingBlocker struct{ reason string }

func (b blockingBlocker) Blocked(_ context.Context, _ string) (bool, string, error) {
	return true, b.reason, nil
}

// destroyHarness wires a Destroyer over a real Dispatcher (the single-writer path)
// and a recording write connection, plus a seedable registry reader and recording
// seams.
type destroyHarness struct {
	destroyer *dispatch.Destroyer
	rec       *storetest.WriteRecorder
	reg       *storetest.RegistryFake
	reverter  *recordingReverter
	objects   *recordingObjectDeleter
}

func newDestroyHarness(t *testing.T, opts ...dispatch.DestroyerOption) destroyHarness {
	t.Helper()
	rec := storetest.NewWriteRecorder()
	reg := storetest.NewRegistryFake()
	rev := &recordingReverter{}
	obj := &recordingObjectDeleter{}
	d := dispatch.New(rec)
	d.Start(context.Background())
	t.Cleanup(d.Stop)
	all := append([]dispatch.DestroyerOption{
		dispatch.WithDataReverter(rev),
		dispatch.WithObjectDeleter(obj),
	}, opts...)
	return destroyHarness{
		destroyer: dispatch.NewDestroyer(reg, d, all...),
		rec:       rec,
		reg:       reg,
		reverter:  rev,
		objects:   obj,
	}
}

// newDestroyHarnessWithLister constructs a harness and wires the given RunLister
// (used for S12/destroy-summaries-before-delete tests that need to surface runs).
func newDestroyHarnessWithLister(t *testing.T, lister dispatch.RunLister) destroyHarness {
	t.Helper()
	rec := storetest.NewWriteRecorder()
	reg := storetest.NewRegistryFake()
	rev := &recordingReverter{}
	obj := &recordingObjectDeleter{}
	d := dispatch.New(rec)
	d.Start(context.Background())
	t.Cleanup(d.Stop)
	all := []dispatch.DestroyerOption{
		dispatch.WithDataReverter(rev),
		dispatch.WithObjectDeleter(obj),
		dispatch.WithRunLister(lister),
	}
	return destroyHarness{
		destroyer: dispatch.NewDestroyer(reg, d, all...),
		rec:       rec,
		reg:       reg,
		reverter:  rev,
		objects:   obj,
	}
}

// firstIndexOf returns the index of the first recorded statement whose SQL contains
// sub, or -1 when none does.
func firstIndexOf(stmts []storetest.RecordedStatement, sub string) int {
	for i, s := range stmts {
		if strings.Contains(s.SQL, sub) {
			return i
		}
	}
	return -1
}

// TestComposerDestroyInterlock proves a lane composer is destroyable only once its
// lane has at most one registered member, mirroring apply's 2+ invariant: a lane
// with two or more registered members refuses the composer destroy (naming the
// lane), while zero or one registered member permits it.
//
// spec: S12/composer-destroy-interlock
func TestComposerDestroyInterlock(t *testing.T) {
	t.Run("S12/composer-destroy-interlock", func(t *testing.T) {
		cases := []struct {
			name              string
			registeredMembers int
			wantDestroyable   bool
		}{
			{"empty lane is destroyable", 0, true},
			{"single registered member is destroyable", 1, true},
			{"two registered members block the destroy", 2, false},
			{"many registered members block the destroy", 5, false},
		}
		for _, tc := range cases {
			t.Run(tc.name, func(t *testing.T) {
				if got := dispatch.LaneComposerDestroyable(tc.registeredMembers); got != tc.wantDestroyable {
					t.Errorf("LaneComposerDestroyable(%d) = %v, want %v", tc.registeredMembers, got, tc.wantDestroyable)
				}
			})
		}
	})
}

// TestComposerDestroyInterlockOnDestroyer proves the interlock through the Destroyer:
// with two of a lane's members registered, DestroyComposer refuses (naming the lane)
// and writes nothing; drop one member and the composer destroy proceeds, clearing the
// lane's rows atomically.
//
// spec: S12/composer-destroy-interlock
func TestComposerDestroyInterlockOnDestroyer(t *testing.T) {
	t.Run("S12/composer-destroy-interlock", func(t *testing.T) {
		// Two registered members: the composer destroy is refused, nothing written.
		blocked := newDestroyHarness(t)
		blocked.reg.Register("extract_orders").Register("load_orders")
		err := blocked.destroyer.DestroyComposer(context.Background(), "ingest", []string{"extract_orders", "load_orders"})
		if err == nil {
			t.Fatal("DestroyComposer of a lane with 2 registered members succeeded, want an interlock refusal")
		}
		if !strings.Contains(err.Error(), "ingest") {
			t.Errorf("interlock refusal %q does not name the lane", err)
		}
		if n := len(blocked.rec.Statements()); n != 0 {
			t.Errorf("a refused composer destroy wrote %d statements, want 0", n)
		}

		// One registered member: the composer destroy proceeds, clearing the lane rows.
		ok := newDestroyHarness(t)
		ok.reg.Register("extract_orders")
		if err := ok.destroyer.DestroyComposer(context.Background(), "ingest", []string{"extract_orders", "load_orders"}); err != nil {
			t.Fatalf("DestroyComposer with 1 registered member: %v", err)
		}
		if !stmtsAny(ok.rec.Statements(), "DELETE FROM lanes") {
			t.Errorf("composer destroy did not clear the lane rows: %v", ok.rec.Statements())
		}
	})
}

// TestDestroyRetiresRowsOneTxn proves declare destroy retires a pipeline's runs and
// inputs, dead-letter entries, artifacts, dependency edges, lane rows, and
// role/grants/credentials in one meta transaction with the pipelines row deleted
// last, so no reference dangles and the teardown is all-or-nothing.
//
// spec: S12/destroy-retires-rows-one-txn
func TestDestroyRetiresRowsOneTxn(t *testing.T) {
	t.Run("S12/destroy-retires-rows-one-txn", func(t *testing.T) {
		h := newDestroyHarness(t)
		if err := h.destroyer.DestroyPipeline(context.Background(), "load_orders"); err != nil {
			t.Fatalf("DestroyPipeline: %v", err)
		}

		txns := h.rec.Transactions()
		if len(txns) != 1 {
			t.Fatalf("destroy retired the pipeline in %d transactions, want 1 (one meta transaction)", len(txns))
		}
		batch := txns[0]
		if len(h.rec.Statements()) != len(batch) {
			t.Errorf("destroy wrote %d statements but only %d rode the one transaction", len(h.rec.Statements()), len(batch))
		}

		// Every table named in the spec's retirement set is deleted.
		for _, table := range []string{
			"run_inputs", "dead_letters", "runs", "artifacts",
			"dependencies", "lanes", "grants", "credentials", "roles", "pipelines",
		} {
			if firstIndexOf(batch, "DELETE FROM "+table) < 0 {
				t.Errorf("retirement transaction does not delete %s: %v", table, batch)
			}
		}

		// The pipelines row is deleted LAST: journal stamps keep resolving to a run or
		// summary until the very end, and no child row is orphaned.
		pipelinesIdx := firstIndexOf(batch, "DELETE FROM pipelines")
		if pipelinesIdx != len(batch)-1 {
			t.Errorf("pipelines delete is at index %d of %d, want last", pipelinesIdx, len(batch))
		}

		// Foreign-key-critical orderings: children before their parents, so the one
		// transaction never trips a constraint mid-flight.
		orderings := [][2]string{
			{"DELETE FROM run_inputs", "DELETE FROM runs"},
			{"DELETE FROM dead_letters", "DELETE FROM runs"},
			{"DELETE FROM runs", "DELETE FROM artifacts"}, // runs.artifact_hash -> artifacts.hash
			{"DELETE FROM grants", "DELETE FROM roles"},
			{"DELETE FROM credentials", "DELETE FROM roles"},
			{"DELETE FROM roles", "DELETE FROM pipelines"},
			{"DELETE FROM artifacts", "DELETE FROM pipelines"},
			{"DELETE FROM dependencies", "DELETE FROM pipelines"},
		}
		for _, o := range orderings {
			if a, b := firstIndexOf(batch, o[0]), firstIndexOf(batch, o[1]); a < 0 || b < 0 || a >= b {
				t.Errorf("retirement order violated: %q (idx %d) must precede %q (idx %d)", o[0], a, o[1], b)
			}
		}

		// The target's name scopes every retirement statement.
		if !containsToken(h.rec.Statements(), "load_orders") {
			t.Errorf("retirement statements are not scoped to the target pipeline: %v", h.rec.Statements())
		}
	})
}

// TestDestroyRevertsUnpromotedData proves declare destroy's teardown reverts the
// target's un-promoted disposable data before it retires the pipeline's registration,
// role, and grants: the DataReverter seam is invoked for the target, and a failing
// revert gates the teardown so no meta row is retired without the data first reverted.
//
// spec: S12/destroy-reverts-unpromoted-data
func TestDestroyRevertsUnpromotedData(t *testing.T) {
	t.Run("S12/destroy-reverts-unpromoted-data", func(t *testing.T) {
		// The revert seam is invoked exactly once, for the target, and the teardown
		// retires the registration, role, and grants alongside it.
		h := newDestroyHarness(t)
		if err := h.destroyer.DestroyPipeline(context.Background(), "load_orders"); err != nil {
			t.Fatalf("DestroyPipeline: %v", err)
		}
		if len(h.reverter.reverted) != 1 || h.reverter.reverted[0] != "load_orders" {
			t.Errorf("data revert was invoked with %v, want [load_orders] exactly once", h.reverter.reverted)
		}
		batch := h.rec.Transactions()[0]
		for _, table := range []string{"pipelines", "roles", "grants"} {
			if firstIndexOf(batch, "DELETE FROM "+table) < 0 {
				t.Errorf("teardown did not retire the target's %s alongside its data revert: %v", table, batch)
			}
		}

		// A failing revert gates the teardown: the un-promoted data must be reverted
		// before any meta row is retired, so a revert failure leaves meta untouched.
		gate := newDestroyHarness(t)
		boom := errors.New("reverse-replay failed")
		gate.reverter.fail = boom
		if err := gate.destroyer.DestroyPipeline(context.Background(), "load_orders"); !errors.Is(err, boom) {
			t.Fatalf("DestroyPipeline error = %v, want it to wrap the revert failure", err)
		}
		if n := len(gate.rec.Statements()); n != 0 {
			t.Errorf("a failed data revert left %d retirement statements in meta, want 0 (revert gates teardown)", n)
		}
	})
}

// TestDestroyScopedTeardown proves iris declare destroy removes one declared unit at
// a time, leaving the engine and the schemas/ tree intact: destroying one pipeline
// deletes exactly that one pipelines row, issues only scoped DELETEs (never a CREATE,
// DROP TABLE, DROP SCHEMA, or any data_journal / schema DDL), so no engine table and
// no declared schema is touched.
//
// spec: S04/declare-destroy-scoped-teardown
func TestDestroyScopedTeardown(t *testing.T) {
	t.Run("S04/declare-destroy-scoped-teardown", func(t *testing.T) {
		h := newDestroyHarness(t)
		if err := h.destroyer.DestroyPipeline(context.Background(), "load_orders"); err != nil {
			t.Fatalf("DestroyPipeline: %v", err)
		}
		stmts := h.rec.Statements()

		// Exactly one declared unit is removed: one pipelines row, the named target.
		pipelineDeletes := stmtsWith(stmts, "DELETE FROM pipelines")
		if len(pipelineDeletes) != 1 {
			t.Fatalf("destroy removed %d pipelines rows, want exactly 1 (one declared unit)", len(pipelineDeletes))
		}
		if len(pipelineDeletes[0].Args) != 1 || pipelineDeletes[0].Args[0] != "load_orders" {
			t.Errorf("the pipelines delete is not scoped to the one target: args %v", pipelineDeletes[0].Args)
		}

		// The engine and the schemas/ tree stay intact: no statement drops a table or
		// schema, creates anything, or touches the data journal. Teardown is
		// meta-registry DELETEs only.
		for _, s := range stmts {
			up := strings.ToUpper(s.SQL)
			if !strings.HasPrefix(strings.TrimSpace(up), "DELETE") {
				t.Errorf("scoped teardown issued a non-DELETE statement (engine/schemas must stay intact): %q", s.SQL)
			}
			for _, forbidden := range []string{"DROP TABLE", "DROP SCHEMA", "CREATE ", "ALTER ", "TRUNCATE", "DATA_JOURNAL", "DATABASE"} {
				if strings.Contains(up, forbidden) {
					t.Errorf("scoped teardown issued %q, which would touch the engine or schemas/: %q", forbidden, s.SQL)
				}
			}
		}
	})
}

// TestDestroyBlockerGatesTeardown proves the destroy blocker seam (E10.1's downstream
// predicates) gates the teardown: a blocked pipeline refuses destroy with the
// blocker's reason and writes nothing, and the default (unwired) blocker is open so a
// plain destroy proceeds.
//
// spec: S04/declare-destroy-scoped-teardown
func TestDestroyBlockerGatesTeardown(t *testing.T) {
	t.Run("S04/declare-destroy-scoped-teardown", func(t *testing.T) {
		h := newDestroyHarness(t, dispatch.WithDestroyBlocker(blockingBlocker{reason: "downstream load_orders depends_on it"}))
		err := h.destroyer.DestroyPipeline(context.Background(), "extract_orders")
		if err == nil {
			t.Fatal("DestroyPipeline of a blocked pipeline succeeded, want a blocked refusal")
		}
		if !strings.Contains(err.Error(), "depends_on") {
			t.Errorf("blocked refusal %q does not carry the blocker reason", err)
		}
		if n := len(h.rec.Statements()); n != 0 {
			t.Errorf("a blocked destroy wrote %d statements, want 0", n)
		}
		if n := len(h.reverter.reverted); n != 0 {
			t.Errorf("a blocked destroy reverted data %d times, want 0 (the blocker gates before any teardown)", n)
		}
	})
}

// recordingRunLister is a dispatch.RunLister for tests: it returns seeded prunable
// runs for a pipeline so the destroy path can write their archival summaries.
type recordingRunLister struct {
	runs map[string][]store.PrunableRun
}

func (r *recordingRunLister) ListPrunableRuns(_ context.Context, pipeline string) ([]store.PrunableRun, error) {
	return append([]store.PrunableRun(nil), r.runs[pipeline]...), nil
}

// TestDestroySummariesBeforeDelete proves declare destroy writes each remaining
// run's archival summary into run_summaries BEFORE deleting the run rows, inside
// the same meta transaction, so journal stamps continue to resolve after the
// pipeline and its runs are gone (S12/destroy-summaries-before-delete).
//
// spec: S12/destroy-summaries-before-delete
func TestDestroySummariesBeforeDelete(t *testing.T) {
	t.Run("S12/destroy-summaries-before-delete", func(t *testing.T) {
		// Seed two runs for the pipeline via the lister seam.
		lister := &recordingRunLister{runs: map[string][]store.PrunableRun{
			"load_orders": {
				{RunID: 42, Pipeline: "load_orders", State: store.RunSucceeded, DeclarationChecksum: "declX"},
				{RunID: 39, Pipeline: "load_orders", State: store.RunSucceeded, DeclarationChecksum: "declY"},
			},
		}}
		h := newDestroyHarnessWithLister(t, lister)
		if err := h.destroyer.DestroyPipeline(context.Background(), "load_orders"); err != nil {
			t.Fatalf("DestroyPipeline: %v", err)
		}

		txns := h.rec.Transactions()
		if len(txns) != 1 {
			t.Fatalf("destroy used %d txns, want 1", len(txns))
		}
		batch := txns[0]

		// Summaries must be written before the runs delete.
		sumIdx := firstIndexOf(batch, "INSERT INTO run_summaries")
		runDelIdx := firstIndexOf(batch, "DELETE FROM runs")
		if sumIdx < 0 {
			t.Fatalf("destroy did not write run_summaries for remaining runs; batch: %v", batch)
		}
		if runDelIdx < 0 {
			t.Fatalf("destroy did not delete runs; batch: %v", batch)
		}
		if sumIdx >= runDelIdx {
			t.Errorf("run_summaries INSERT at %d must precede runs DELETE at %d (summaries before delete)", sumIdx, runDelIdx)
		}
		// Both runs should be summarized (two inserts or one batch with both).
		// At minimum the summary write must name the run ids.
		found42, found39 := false, false
		for _, s := range batch {
			if strings.Contains(s.SQL, "run_summaries") {
				for _, a := range s.Args {
					if a == int64(42) {
						found42 = true
					}
					if a == int64(39) {
						found39 = true
					}
				}
			}
		}
		if !found42 || !found39 {
			t.Errorf("summaries did not cover both runs 42 and 39; statements: %v", batch)
		}
	})
}

// TestDestroyPreservesEngineJournal proves that after declare destroy the engine,
// the schemas/ tree, endpoints, and journal history all remain intact: the
// retirement issues only scoped meta DELETEs for the pipeline's own rows and never
// touches endpoints, journal, or any engine DDL.
//
// spec: S12/destroy-preserves-engine-journal
func TestDestroyPreservesEngineJournal(t *testing.T) {
	t.Run("S12/destroy-preserves-engine-journal", func(t *testing.T) {
		h := newDestroyHarness(t)
		if err := h.destroyer.DestroyPipeline(context.Background(), "load_orders"); err != nil {
			t.Fatalf("DestroyPipeline: %v", err)
		}
		stmts := h.rec.Statements()
		for _, s := range stmts {
			up := strings.ToUpper(s.SQL)
			// Endpoints must survive (read surface outlives workload teardown).
			if strings.Contains(up, "DELETE FROM ENDPOINTS") || strings.Contains(up, "DROP") && strings.Contains(up, "ENDPOINT") {
				t.Errorf("destroy touched endpoints: %s", s.SQL)
			}
			// Journal history is retained (capture rows live in data DB, never deleted by destroy).
			if strings.Contains(up, "DATA_JOURNAL") || strings.Contains(up, "DELETE FROM PUBLIC.DATA_JOURNAL") {
				t.Errorf("destroy touched journal: %s", s.SQL)
			}
			// Engine and schemas/ stay: no DROP SCHEMA, no engine table drops.
		}
		// At least the pipelines row for the target was deleted; everything else engine-wide intact.
		if !stmtsAny(stmts, "DELETE FROM pipelines") {
			t.Errorf("destroy did not retire the pipelines row")
		}
	})
}
