package daemon

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"sync"

	"github.com/MateusAMP2119/iris-lakehouse/internal/api"
	"github.com/MateusAMP2119/iris-lakehouse/internal/dispatch"
	"github.com/MateusAMP2119/iris-lakehouse/internal/pg"
	"github.com/MateusAMP2119/iris-lakehouse/internal/store"
)

// promotePlane is the daemon's api.PromoteHandler, installed on leadership.
type promotePlane struct {
	mu   sync.RWMutex
	live *promoteOrchestrator
}

var _ api.PromoteHandler = (*promotePlane)(nil)

func newPromotePlane(_ *slog.Logger) *promotePlane {
	return &promotePlane{}
}

func (p *promotePlane) install(o *promoteOrchestrator) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.live = o
}

func (p *promotePlane) clear() {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.live = nil
}

func (p *promotePlane) orchestrator() *promoteOrchestrator {
	p.mu.RLock()
	defer p.mu.RUnlock()
	return p.live
}

func (p *promotePlane) PromotePipeline(ctx context.Context, req api.PipelinePromoteRequest) (api.PipelinePromoteResult, error) {
	o := p.orchestrator()
	if o == nil {
		return api.PipelinePromoteResult{}, api.ErrControlUnavailable
	}
	return o.promote(ctx, req)
}

type promoteOrchestrator struct {
	promoter *dispatch.Promoter
	logger   *slog.Logger
}

func newPromoteOrchestrator(submit dispatch.Submitter, state store.PromoteStateReader, journal dispatch.JournalPromoter, logger *slog.Logger) *promoteOrchestrator {
	if logger == nil {
		logger = slog.New(slog.NewTextHandler(io.Discard, nil))
	}
	return &promoteOrchestrator{
		promoter: dispatch.NewPromoter(state, submit, dispatch.WithJournalPromoter(journal)),
		logger:   logger,
	}
}

func (o *promoteOrchestrator) promote(ctx context.Context, req api.PipelinePromoteRequest) (api.PipelinePromoteResult, error) {
	if req.Pipeline == "" {
		return api.PipelinePromoteResult{}, fmt.Errorf("pipeline promote: missing pipeline name")
	}
	out, err := o.promoter.Promote(ctx, req.Pipeline)
	if err != nil {
		return api.PipelinePromoteResult{}, err
	}
	return api.PipelinePromoteResult{
		Pipeline: out.Pipeline,
		DataMode: string(out.DataMode),
		Warnings: out.Warnings,
	}, nil
}

// submitShim satisfies dispatch.Submitter for the WithPromotePlane wiring
// argument (the submitter is not used by WithPromotePlane; the live dispatcher
// is passed at lead time inside the candidate).
type submitShim struct{}

func (submitShim) Submit(context.Context, func(*store.Writer) error) error { return nil }

// liveJournalPromoter implements dispatch.JournalPromoter for the real daemon
// wiring. It resolves run attribution from the meta reader (plain MVCC) and
// issues the marker flip through pg.ExecutePromotionFlip on the data DB.
type liveJournalPromoter struct {
	reader store.Reader
	db     pg.DB
}

func (p *liveJournalPromoter) PromoteJournal(ctx context.Context, pipeline string) error {
	runs, err := p.reader.Runs(ctx, store.RunFilter{})
	if err != nil {
		return fmt.Errorf("promote journal: read runs: %w", err)
	}
	var ids []int64
	for _, r := range runs {
		if r.Pipeline == pipeline {
			if id := parseRunID(r.ID); id != 0 {
				ids = append(ids, id)
			}
		}
	}
	if err := pg.ExecutePromotionFlip(ctx, p.db, ids); err != nil {
		return fmt.Errorf("promote journal: flip: %w", err)
	}
	return nil
}
