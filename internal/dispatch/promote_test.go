package dispatch_test

import (
	"context"
	"strings"
	"testing"

	"github.com/MateusAMP2119/iris-engine-cli/internal/declare"
	"github.com/MateusAMP2119/iris-engine-cli/internal/dispatch"
	"github.com/MateusAMP2119/iris-engine-cli/internal/store"
	"github.com/MateusAMP2119/iris-engine-cli/internal/store/storetest"
)

// This file proves the dispatch-level promote op behind `iris pipeline promote`:
// promotion flips the pipeline's per-pipeline data_mode in meta from disposable to
// permanent through the single writer, is refused whenever the pipeline is not in
// built state (so a source-only pipeline can never hold permanent data), and repeats
// the cross-mode read warning while any upstream read dependency is still disposable.
// The meta facts ride a fake store.PromoteStateReader; the write path is the real
// Dispatcher over a recording write connection.

// promoteState is a canned store.PromoteStateReader: the meta facts the promote
// gate consults, fixed by the test.
type promoteState struct {
	registered bool
	mode       store.DataMode
	built      bool
	upstreams  []store.UpstreamDataMode
}

var _ store.PromoteStateReader = (*promoteState)(nil)

func (s *promoteState) PipelineDataMode(context.Context, string) (store.DataMode, bool, error) {
	return s.mode, s.registered, nil
}

func (s *promoteState) PipelineBuilt(context.Context, string) (bool, error) {
	return s.built, nil
}

func (s *promoteState) UpstreamDataModes(context.Context, string) ([]store.UpstreamDataMode, error) {
	return s.upstreams, nil
}

// journalRecorder is a recording dispatch.JournalPromoter: it counts the live
// journal flips the promote op requested, so a refusal provably released nothing.
type journalRecorder struct{ calls int }

func (j *journalRecorder) PromoteJournal(context.Context, string) error {
	j.calls++
	return nil
}

// newPromoteHarness wires a Promoter over the canned meta state, a real
// Dispatcher (the single-writer path) with a recording write connection, and a
// recording journal-flip seam.
func newPromoteHarness(t *testing.T, state *promoteState) (*dispatch.Promoter, *storetest.WriteRecorder, *journalRecorder) {
	t.Helper()
	rec := storetest.NewWriteRecorder()
	d := dispatch.New(rec)
	d.Start(context.Background())
	t.Cleanup(d.Stop)
	journal := &journalRecorder{}
	return dispatch.NewPromoter(state, d, dispatch.WithJournalPromoter(journal)), rec, journal
}

// TestPromoteRequiresBuilt proves `iris pipeline promote` marks data permanent only
// when the pipeline is built, and is rejected for a source-only pipeline ("marks data
// permanent, only once built"). A source-only pipeline (no recorded artifact) is
// refused before any write; a built pipeline promotes, and the outcome is the
// permanent data mode.
func TestPromoteRequiresBuilt(t *testing.T) {
	t.Run("source-only pipeline is rejected", func(t *testing.T) {
		p, rec, journal := newPromoteHarness(t, &promoteState{
			registered: true, mode: store.DataDisposable, built: false,
		})
		_, err := p.Promote(context.Background(), "etl")
		if err == nil {
			t.Fatal("Promote succeeded for a source-only pipeline, want a refusal")
		}
		if !strings.Contains(err.Error(), "built") {
			t.Errorf("refusal does not name the built requirement: %v", err)
		}
		if n := len(rec.Statements()); n != 0 {
			t.Errorf("refused promote issued %d meta statements, want 0", n)
		}
		if journal.calls != 0 {
			t.Errorf("refused promote flipped the journal %d times, want 0", journal.calls)
		}
	})

	t.Run("built pipeline is marked permanent", func(t *testing.T) {
		p, rec, journal := newPromoteHarness(t, &promoteState{
			registered: true, mode: store.DataDisposable, built: true,
		})
		out, err := p.Promote(context.Background(), "etl")
		if err != nil {
			t.Fatalf("Promote: %v", err)
		}
		if out.DataMode != store.DataPermanent {
			t.Errorf("outcome data mode = %q, want %q", out.DataMode, store.DataPermanent)
		}
		if n := len(rec.Statements()); n != 1 {
			t.Fatalf("promote issued %d meta statements, want exactly 1 (the data_mode flip)", n)
		}
		if journal.calls != 1 {
			t.Errorf("promote flipped the journal %d times, want 1", journal.calls)
		}
	})
}

// TestPromoteGatedOnBuilt proves `iris pipeline promote` refuses when the pipeline is
// not in built state (the data_mode flip is gated on built), and refuses an
// unregistered pipeline outright -- neither path reaches the single writer.
func TestPromoteGatedOnBuilt(t *testing.T) {
	t.Run("un-built pipeline is refused", func(t *testing.T) {
		p, rec, _ := newPromoteHarness(t, &promoteState{
			registered: true, mode: store.DataDisposable, built: false,
		})
		_, err := p.Promote(context.Background(), "etl")
		if err == nil {
			t.Fatal("Promote succeeded for an un-built pipeline, want a refusal")
		}
		if !strings.Contains(err.Error(), "built") {
			t.Errorf("refusal does not name the built requirement: %v", err)
		}
		if n := len(rec.Statements()); n != 0 {
			t.Errorf("refused promote issued %d meta statements, want 0", n)
		}
	})

	t.Run("unregistered pipeline is refused", func(t *testing.T) {
		p, rec, _ := newPromoteHarness(t, &promoteState{registered: false})
		_, err := p.Promote(context.Background(), "ghost")
		if err == nil {
			t.Fatal("Promote succeeded for an unregistered pipeline, want a refusal")
		}
		if !strings.Contains(err.Error(), "not registered") {
			t.Errorf("refusal does not say the pipeline is unregistered: %v", err)
		}
		if n := len(rec.Statements()); n != 0 {
			t.Errorf("refused promote issued %d meta statements, want 0", n)
		}
	})
}

// TestPromoteFlipsDataMode proves the promote op's meta effect is exactly the
// per-pipeline data_mode flip from disposable to permanent, issued through the single
// writer (control truth is per-pipeline data_mode in meta; promote flips it
// permanent), and that re-promoting an already-permanent pipeline is an idempotent
// no-op.
func TestPromoteFlipsDataMode(t *testing.T) {
	t.Run("disposable flips to permanent through the single writer", func(t *testing.T) {
		p, rec, _ := newPromoteHarness(t, &promoteState{
			registered: true, mode: store.DataDisposable, built: true,
		})
		if _, err := p.Promote(context.Background(), "etl"); err != nil {
			t.Fatalf("Promote: %v", err)
		}
		stmts := rec.Statements()
		if len(stmts) != 1 {
			t.Fatalf("promote issued %d meta statements, want exactly 1", len(stmts))
		}
		sql := stmts[0].SQL
		if !strings.Contains(sql, "UPDATE pipelines") || !strings.Contains(sql, "data_mode") || !strings.Contains(sql, "'permanent'") {
			t.Errorf("flip statement is not the pipelines data_mode flip to permanent:\n%s", sql)
		}
		if len(stmts[0].Args) != 1 || stmts[0].Args[0] != "etl" {
			t.Errorf("flip statement args = %v, want exactly the pipeline name", stmts[0].Args)
		}
	})

	t.Run("already permanent is an idempotent no-op", func(t *testing.T) {
		p, rec, _ := newPromoteHarness(t, &promoteState{
			registered: true, mode: store.DataPermanent, built: true,
		})
		out, err := p.Promote(context.Background(), "etl")
		if err != nil {
			t.Fatalf("re-promote of a permanent pipeline: %v", err)
		}
		if out.DataMode != store.DataPermanent {
			t.Errorf("outcome data mode = %q, want %q", out.DataMode, store.DataPermanent)
		}
		if n := len(rec.Statements()); n != 0 {
			t.Errorf("re-promote issued %d meta statements, want 0 (the mode is already permanent)", n)
		}
	})
}

// TestPromoteRepeatsCrossModeWarning proves promote repeats the cross-mode read
// warning while an upstream read dependency remains in disposable data_mode (apply
// warns, and promote repeats it while the upstream stays disposable). The warning is
// advisory -- it never blocks the promote -- it repeats on every invocation while the
// upstream is disposable, and it stops once the upstream itself is promoted.
func TestPromoteRepeatsCrossModeWarning(t *testing.T) {
	state := &promoteState{
		registered: true, mode: store.DataPermanent, built: true,
		upstreams: []store.UpstreamDataMode{
			{Pipeline: "raw_orders", Mode: store.DataDisposable},
			{Pipeline: "dims", Mode: store.DataPermanent},
		},
	}
	p, _, _ := newPromoteHarness(t, state)

	assertWarns := func(t *testing.T, out dispatch.PromoteOutcome) {
		t.Helper()
		if len(out.Warnings) != 1 {
			t.Fatalf("promote surfaced %d warnings, want exactly 1 (one disposable upstream)", len(out.Warnings))
		}
		w := out.Warnings[0]
		if w.Kind != declare.WarnCrossModeRead {
			t.Errorf("warning kind = %q, want %q", w.Kind, declare.WarnCrossModeRead)
		}
		if !strings.Contains(w.Message, "raw_orders") {
			t.Errorf("warning does not name the disposable upstream:\n%s", w.Message)
		}
	}

	out, err := p.Promote(context.Background(), "etl")
	if err != nil {
		t.Fatalf("Promote: %v", err)
	}
	assertWarns(t, out)

	// A second promote while the upstream is still disposable repeats the warning.
	out, err = p.Promote(context.Background(), "etl")
	if err != nil {
		t.Fatalf("re-Promote: %v", err)
	}
	assertWarns(t, out)

	// Once the upstream is promoted, the warning stops.
	state.upstreams[0].Mode = store.DataPermanent
	out, err = p.Promote(context.Background(), "etl")
	if err != nil {
		t.Fatalf("Promote after upstream promotion: %v", err)
	}
	if len(out.Warnings) != 0 {
		t.Errorf("promote still warns after every upstream is permanent: %v", out.Warnings)
	}
}
