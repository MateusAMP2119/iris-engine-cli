package dispatch

// This file is the promote op: the leader-side path behind `iris pipeline promote
// <name>`. Promotion marks a pipeline's data permanent, and it is gated on built: the
// flip is refused whenever the pipeline is not in built state, so a source-only
// pipeline can never hold permanent data (the same blocked matrix cell
// store.ValidateModeMatrix and pg.PermanentRequiresBuilt enforce at their own
// boundaries -- the gate here reuses pg.PermanentRequiresBuilt so the rule can never
// drift). An admitted promote has exactly two effects, in order: the journal-side
// marker flip on the data database (open entries retired to promoted; internal/pg's
// promote.go owns that model, reached through the JournalPromoter seam), then the
// per-pipeline data_mode flip in meta through the single writer -- the control truth
// wipe scope is decided from. Nothing is copied, moved, or deleted; capture and
// provenance continue unchanged.
//
// Promote also repeats the cross-mode read warning apply raised: while any depends_on
// upstream is still in disposable data_mode, the outcome carries one advisory warning
// per such upstream (legitimate mid-promotion; warn, never refuse). The warnings are
// computed by the same declare.CheckCrossModeReads rule apply uses, over the upstream
// data modes read from meta, so apply and promote can never disagree on the warning.

import (
	"context"
	"fmt"

	"github.com/MateusAMP2119/iris-lakehouse/internal/declare"
	"github.com/MateusAMP2119/iris-lakehouse/internal/pg"
	"github.com/MateusAMP2119/iris-lakehouse/internal/store"
)

// JournalPromoter releases a pipeline's open journal entries to promoted on the
// data database: the marker-only flip pg.ExecutePromotionFlip issues (promotion
// ends wipe eligibility only; it moves no data). The daemon's composition wires
// the live flip over the pipeline's run ids; the default is a no-op so the
// promote op composes before that wiring lands, exactly like the destroy op's
// DataReverter seam.
type JournalPromoter interface {
	// PromoteJournal retires the pipeline's open journal entries to promoted.
	PromoteJournal(ctx context.Context, pipeline string) error
}

// noopJournalPromoter is the default JournalPromoter: no journal to flip.
type noopJournalPromoter struct{}

func (noopJournalPromoter) PromoteJournal(context.Context, string) error { return nil }

// PromoterOption configures a Promoter at construction.
type PromoterOption func(*Promoter)

// WithJournalPromoter sets the data-database journal-flip seam. A nil promoter
// keeps the no-op default.
func WithJournalPromoter(j JournalPromoter) PromoterOption {
	return func(p *Promoter) {
		if j != nil {
			p.journal = j
		}
	}
}

// PromoteOutcome is the decided result of one admitted promote: the pipeline,
// its data mode after the flip (always permanent), and the advisory cross-mode
// read warnings the promote repeats. Warnings accompany the outcome, never
// block it.
type PromoteOutcome struct {
	// Pipeline is the promoted pipeline.
	Pipeline string
	// DataMode is the pipeline's data mode after the promote: always permanent.
	DataMode store.DataMode
	// Warnings are the repeated cross-mode read advisories: one per depends_on
	// upstream still in disposable data_mode, empty once every upstream is
	// permanent.
	Warnings []declare.Warning
}

// Promoter is the promote op. It holds only seams -- the promote-gate read
// surface, the single-writer submitter, and the journal-flip seam -- so it is
// composed with fakes or the real meta+data stack alike. The daemon composes it
// onto POST /pipeline/promote for the leader.
type Promoter struct {
	state   store.PromoteStateReader
	submit  Submitter
	journal JournalPromoter
}

// NewPromoter builds the promote op over the promote-gate read seam and the
// single-writer submission seam. The journal flip defaults to a no-op until the
// daemon wires the live pg.ExecutePromotionFlip composition.
func NewPromoter(state store.PromoteStateReader, submit Submitter, opts ...PromoterOption) *Promoter {
	p := &Promoter{state: state, submit: submit, journal: noopJournalPromoter{}}
	for _, o := range opts {
		o(p)
	}
	return p
}

// Promote marks the named pipeline's data permanent. The gate runs first, against
// meta facts read through the plain-MVCC seam: an unregistered pipeline is refused
// outright, and an un-built pipeline is refused by the durability gate
// (pg.PermanentRequiresBuilt) -- promote is gated on built, so a source-only pipeline
// can never hold permanent data. An admitted promote flips the journal's open entries
// to promoted (the data database), then flips the pipeline's data_mode in meta from
// disposable to permanent through the single writer; a pipeline already permanent
// skips the meta write (the flip is one-way and idempotent). The outcome repeats the
// cross-mode read warning for every upstream still disposable, on every invocation,
// until the upstream itself is promoted.
func (p *Promoter) Promote(ctx context.Context, name string) (PromoteOutcome, error) {
	mode, found, err := p.state.PipelineDataMode(ctx, name)
	if err != nil {
		return PromoteOutcome{}, fmt.Errorf("dispatch: promote %q: read data mode: %w", name, err)
	}
	if !found {
		return PromoteOutcome{}, fmt.Errorf("dispatch: promote %q: pipeline is not registered", name)
	}

	built, err := p.state.PipelineBuilt(ctx, name)
	if err != nil {
		return PromoteOutcome{}, fmt.Errorf("dispatch: promote %q: read built state: %w", name, err)
	}
	if err := pg.PermanentRequiresBuilt(declare.DataPermanent, built); err != nil {
		return PromoteOutcome{}, fmt.Errorf(
			"dispatch: promote %q refused: pipeline is not in built state (run `iris pipeline build %s` first): %w",
			name, name, err)
	}

	// The repeated cross-mode read warning: the same rule apply runs
	// (declare.CheckCrossModeReads), fed the upstreams' data modes from meta. The
	// promote-time read grain in meta is the depends_on upstream pipeline, so the
	// warning names the upstream pipeline whose tables stay disposable.
	upstreams, err := p.state.UpstreamDataModes(ctx, name)
	if err != nil {
		return PromoteOutcome{}, fmt.Errorf("dispatch: promote %q: read upstream data modes: %w", name, err)
	}
	reads := make([]declare.UpstreamRead, 0, len(upstreams))
	for _, u := range upstreams {
		reads = append(reads, declare.UpstreamRead{Table: u.Pipeline, Mode: declare.DataMode(u.Mode)})
	}
	warnings := declare.CheckCrossModeReads(declare.DataPermanent, reads)

	// Journal first, meta second: if the meta flip fails after the journal flip,
	// the pipeline stays disposable and a re-issued promote re-runs the
	// idempotent, open-guarded journal flip; the reverse order could leave a
	// permanent-mode pipeline whose old entries were still wipe-eligible.
	if err := p.journal.PromoteJournal(ctx, name); err != nil {
		return PromoteOutcome{}, fmt.Errorf("dispatch: promote %q: flip journal entries: %w", name, err)
	}
	if mode != store.DataPermanent {
		if err := p.submit.Submit(ctx, func(w *store.Writer) error {
			return w.PromotePipeline(ctx, name)
		}); err != nil {
			return PromoteOutcome{}, fmt.Errorf("dispatch: promote %q: flip data mode: %w", name, err)
		}
	}

	return PromoteOutcome{Pipeline: name, DataMode: store.DataPermanent, Warnings: warnings}, nil
}
