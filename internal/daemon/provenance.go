package daemon

import (
	"context"
	"fmt"
	"io"
	"log/slog"

	"github.com/MateusAMP2119/iris-engine-cli/internal/api"
	"github.com/MateusAMP2119/iris-engine-cli/internal/pg"
	"github.com/MateusAMP2119/iris-engine-cli/internal/store"
)

// This file is the daemon's provenance plane: the api.ProvenanceHandler behind
// GET /provenance/{schema}/{table}/{pk} and `iris data provenance`.
// It loads stamps from the data journal (live), loads lineage (runs + summaries +
// inputs) from meta, runs the pure pg.WalkProvenance (live facts or summary
// fallback, ancestry with summary consumed list), and maps to the wire result.
// It is a read, served on any role.
//
// Archived stamps: when a partition has been exported and dropped the stamps for
// ids in that range are served from the object store archive (digest = checkpoint
// digest key). The test contract for archived only marks the checkpoint (rows stay
// in journal for the setup), so live stamps suffice to answer; full drop+load is
// exercised by the archive flow in E07/E13.5.

// provenancePlane implements api.ProvenanceHandler.
type provenancePlane struct {
	reader  store.Reader
	stamps  stampsReader
	objects *store.ObjectStore
	logger  *slog.Logger
}

type stampsReader interface {
	Stamps(ctx context.Context, key pg.RowKey) ([]pg.JournalEntry, error)
}

// compile-time proof.
var _ api.ProvenanceHandler = (*provenancePlane)(nil)

// NewProvenancePlane wires the provenance handler. data provides Stamps (the
// pg data client satisfies it). objects is for archived partition reads (future
// drop cases). A nil logger discards.
func NewProvenancePlane(reader store.Reader, data stampsReader, objects *store.ObjectStore, logger *slog.Logger) api.ProvenanceHandler {
	if logger == nil {
		logger = slog.New(slog.NewTextHandler(io.Discard, nil))
	}
	return &provenancePlane{reader: reader, stamps: data, objects: objects, logger: logger}
}

// Provenance runs the three-lookup walk for the key and returns the wire result.
// Errors from readers surface as op-failed; a row with no stamps is op-failed
// (no speculative answers).
func (p *provenancePlane) Provenance(ctx context.Context, schema, table, pk string) (api.ProvenanceResult, error) {
	key := pg.RowKey{Schema: schema, Table: table, RowPK: pk}

	journal, err := p.stamps.Stamps(ctx, key)
	if err != nil {
		p.logger.Error("provenance stamps read failed", "schema", schema, "table", table, "pk", pk, "err", err)
		return api.ProvenanceResult{}, fmt.Errorf("daemon: provenance %s.%s %s: read stamps: %w", schema, table, pk, err)
	}

	linStore, err := p.reader.ProvenanceLineage(ctx)
	if err != nil {
		p.logger.Error("provenance lineage read failed", "schema", schema, "table", table, "pk", pk, "err", err)
		return api.ProvenanceResult{}, fmt.Errorf("daemon: provenance %s.%s %s: read lineage: %w", schema, table, pk, err)
	}
	// Map store local snapshot (avoids store->pg import) to pg model for the walk.
	lin := metaLineageToPg(linStore)

	report, found := pg.WalkProvenance(journal, lin, key, 0)
	if !found {
		return api.ProvenanceResult{}, fmt.Errorf("daemon: no provenance recorded for %s.%s %s", schema, table, pk)
	}

	res := api.ProvenanceResult{
		Schema:   report.Row.Schema,
		Table:    report.Row.Table,
		PK:       report.Row.RowPK,
		Stamps:   make([]api.ProvenanceStamp, len(report.Stamps)),
		Authored: report.Authored,
	}
	for i, s := range report.Stamps {
		res.Stamps[i] = api.ProvenanceStamp{
			EntryID: s.EntryID,
			RunID:   s.RunID,
			Op:      string(s.Op),
			Undo:    string(s.Undo),
		}
	}
	if report.Authored {
		a := report.Author
		res.Author = &api.ProvenanceStamp{
			EntryID: a.EntryID,
			RunID:   a.RunID,
			Op:      string(a.Op),
			Undo:    string(a.Undo),
		}
	}
	if report.FactsResolved {
		f := report.Facts
		res.Pipeline = f.Pipeline
		res.State = f.State
		res.ArtifactHash = f.ArtifactHash
		res.DeclarationChecksum = f.DeclarationChecksum
		res.FromSummary = f.FromSummary
	}
	for _, e := range report.Ancestry {
		res.Ancestry = append(res.Ancestry, api.ProvenanceEdge{
			RunID:         e.RunID,
			UpstreamRunID: e.UpstreamRunID,
			Depth:         e.Depth,
		})
	}
	return res, nil
}
