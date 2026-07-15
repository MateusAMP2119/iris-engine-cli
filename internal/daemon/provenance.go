package daemon

import (
	"context"
	"fmt"
	"io"
	"log/slog"

	"github.com/MateusAMP2119/iris-lakehouse/internal/api"
	"github.com/MateusAMP2119/iris-lakehouse/internal/archive"
	"github.com/MateusAMP2119/iris-lakehouse/internal/pg"
	"github.com/MateusAMP2119/iris-lakehouse/internal/store"
)

// This file is the daemon's provenance plane: the api.ProvenanceHandler behind
// GET /provenance/{schema}/{table}/{pk} and `iris data provenance`.
// It loads stamps from the data journal (live), loads lineage (runs + summaries +
// inputs) from meta, runs the pure pg.WalkProvenance (live facts or summary
// fallback, ancestry with summary consumed list), and maps to the wire result.
// It is a read, served on any role.
//
// Archived stamps: once a partition has been exported and dropped, the stamps
// for ids in that range live only in the object-store archive (keyed by the
// checkpoint digest). When the resident journal yields no stamps for a key, the
// plane falls back to the archived partitions: it walks the archived checkpoint
// chain, reads each exported partition from the object store (archive.Read),
// decodes the canonical rows, and feeds the key's recovered stamps to the same
// walk -- so a row whose history was sealed and dropped still answers instead of
// reading as "no provenance recorded". The export half is the seal step's
// (seal.go, writing through internal/archive).

// provenancePlane implements api.ProvenanceHandler.
type provenancePlane struct {
	reader  store.Reader
	stamps  stampsReader
	objects *store.ObjectStore
	chain   store.CheckpointChainReader
	logger  *slog.Logger
}

type stampsReader interface {
	Stamps(ctx context.Context, key pg.RowKey) ([]pg.JournalEntry, error)
}

// compile-time proof.
var _ api.ProvenanceHandler = (*provenancePlane)(nil)

// NewProvenancePlane wires the provenance handler. data provides Stamps (the pg
// data client satisfies it); objects and chain are the archived-stamp fallback
// (the exported partitions under the object store, named by the archived
// checkpoints). A nil chain or objects disables the fallback (shape tests). A
// nil logger discards.
func NewProvenancePlane(reader store.Reader, data stampsReader, objects *store.ObjectStore, chain store.CheckpointChainReader, logger *slog.Logger) api.ProvenanceHandler {
	if logger == nil {
		logger = slog.New(slog.NewTextHandler(io.Discard, nil))
	}
	return &provenancePlane{reader: reader, stamps: data, objects: objects, chain: chain, logger: logger}
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
	if len(journal) == 0 {
		// The resident journal holds nothing for this key: the row's stamps may
		// have been sealed, exported, and dropped. Recover them from the archived
		// partitions before concluding the row has no history.
		journal, err = p.archivedStamps(ctx, key)
		if err != nil {
			p.logger.Error("provenance archived-stamps read failed", "schema", schema, "table", table, "pk", pk, "err", err)
			return api.ProvenanceResult{}, fmt.Errorf("daemon: provenance %s.%s %s: read archived stamps: %w", schema, table, pk, err)
		}
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

	return p.render(report), nil
}

// archivedStamps recovers a key's stamps from the archived partitions: it walks
// the archived checkpoint chain in seq order and reads each exported partition
// from the object store under its digest key, decoding the canonical rows and
// keeping the key's own. A missing or unreadable archive is an error -- the
// checkpoint says the history exists, so failing loudly beats reporting "no
// provenance recorded" over durably-written stamps. The fallback runs only when
// the resident journal held nothing for the key, so the full-archive scan rides
// the rare path, never every query.
func (p *provenancePlane) archivedStamps(ctx context.Context, key pg.RowKey) ([]pg.JournalEntry, error) {
	if p.objects == nil || p.chain == nil {
		return nil, nil // fallback not wired (shape-test composition)
	}
	cps, err := p.chain.ArchivedCheckpoints(ctx)
	if err != nil {
		return nil, err
	}
	var out []pg.JournalEntry
	for _, cp := range cps {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		path := p.objects.Path(fmt.Sprintf("%x", cp.Digest))
		_, rows, err := archive.Read(path)
		if err != nil {
			return nil, fmt.Errorf("archived partition %x (checkpoint seq %d): %w", cp.Digest, cp.Seq, err)
		}
		for _, row := range rows {
			entry, ok := pg.ParseCompactedRow(row)
			if ok && entry.Key() == key {
				out = append(out, entry)
			}
		}
	}
	return out, nil
}

// render maps a provenance report to the wire result.
func (p *provenancePlane) render(report pg.ProvenanceReport) api.ProvenanceResult {
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
	return res
}
