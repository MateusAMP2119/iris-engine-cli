package daemon

import (
	"context"
	"io"
	"log/slog"

	"github.com/MateusAMP2119/iris-lakehouse/internal/api"
	"github.com/MateusAMP2119/iris-lakehouse/internal/catalog"
	"github.com/MateusAMP2119/iris-lakehouse/internal/store"
)

// This file is the GET /catalog read plane (#219): the pack listing the ps overlay
// renders, served on any role from the embedded catalog (remote sources arrive with
// #220), badged installed when every pack pipeline is currently registered.

// catalogReadPlane is the daemon's api.CatalogListHandler.
type catalogReadPlane struct {
	registry store.RegistryReader
	logger   *slog.Logger
}

// compile-time proof the plane is the mux's catalog listing reader.
var _ api.CatalogListHandler = (*catalogReadPlane)(nil)

// NewCatalogReadPlane builds the pack-listing reader; a nil registry skips the installed badges.
func NewCatalogReadPlane(registry store.RegistryReader, logger *slog.Logger) api.CatalogListHandler {
	if logger == nil {
		logger = slog.New(slog.NewTextHandler(io.Discard, nil))
	}
	return &catalogReadPlane{registry: registry, logger: logger}
}

// ListPacks answers every visible pack with badges and preview material.
func (p *catalogReadPlane) ListPacks(ctx context.Context) (api.CatalogListResult, error) {
	packs, err := catalog.Embedded()
	if err != nil {
		return api.CatalogListResult{}, err
	}
	registered := map[string]bool{}
	if p.registry != nil {
		names, rerr := p.registry.RegisteredPipelines(ctx)
		if rerr != nil {
			p.logger.Warn("catalog list: registry read failed; installed badges skipped", "err", rerr)
		}
		for _, n := range names {
			registered[n] = true
		}
	}
	res := api.CatalogListResult{Packs: make([]api.CatalogPack, 0, len(packs))}
	for _, pk := range packs {
		res.Packs = append(res.Packs, describePack(pk, registered))
	}
	return res, nil
}

// describePack renders one pack's listing entry; derivation errors ride the entry, never fail the listing.
func describePack(pk catalog.Pack, registered map[string]bool) api.CatalogPack {
	entry := api.CatalogPack{
		Name: pk.Name, Description: pk.Description, Tags: pk.Tags,
		Requires: pk.Requires, SHA256: pk.SHA256, Source: pk.Source, Readme: pk.README,
	}
	for _, f := range pk.Files {
		entry.Files = append(entry.Files, f.Path)
	}
	if names, err := catalog.PipelineNames(pk); err == nil {
		entry.Pipelines = names
		entry.Installed = len(names) > 0 && allRegistered(names, registered)
	}
	if order, err := catalog.ApplyOrder(pk); err == nil {
		entry.ApplyOrder = order
	}
	return entry
}

// allRegistered reports whether every name is currently registered.
func allRegistered(names []string, registered map[string]bool) bool {
	for _, n := range names {
		if !registered[n] {
			return false
		}
	}
	return true
}
