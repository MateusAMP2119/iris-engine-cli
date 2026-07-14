package daemon

import (
	"github.com/MateusAMP2119/iris-lakehouse/internal/pg"
	"github.com/MateusAMP2119/iris-lakehouse/internal/store"
)

// This file holds the one mapping from the store's meta-side lineage snapshot
// (store.ProvenanceLineage: live runs, archival summaries, consumption edges) to
// the pure pg.Lineage the provenance and trace walks run over. Both the row
// provenance walk (provenance.go) and the run-ancestry trace walk (runtraceplane.go)
// read the same meta rows and feed the same pure model, so the mapping lives here
// once: store avoids importing pg (it returns its own local snapshot), and the
// daemon maps that snapshot to pg at the composition edge.

// metaLineageToPg maps the store's meta lineage snapshot to the pure pg.Lineage the
// provenance and ancestry walks consume: live run rows as RunRecords, archival
// summaries (with their preserved consumed-upstream lists) as ArchivalSummaries, and
// the run_inputs rows as RunInputs. It is a straight field mapping -- no reads, no
// I/O -- so both walks share one honest translation.
func metaLineageToPg(linStore store.ProvenanceLineage) pg.Lineage {
	var lin pg.Lineage
	for _, r := range linStore.Runs {
		lin.Runs = append(lin.Runs, pg.RunRecord{
			RunID: r.RunID, Pipeline: r.Pipeline, State: r.State,
			ArtifactHash: r.ArtifactHash, DeclarationChecksum: r.DeclarationChecksum,
			Pin: pg.SnapshotPin{SnapshotLSN: r.SnapshotLSN, JournalFloor: r.JournalFloor, JournalCeiling: r.JournalCeiling},
		})
	}
	for _, s := range linStore.Summaries {
		lin.Summaries = append(lin.Summaries, pg.ArchivalSummary{
			RunID: s.RunID, Pipeline: s.Pipeline, State: s.State,
			ArtifactHash: s.ArtifactHash, DeclarationChecksum: s.DeclarationChecksum,
			ConsumedUpstreamRunIDs: s.ConsumedUpstreamRunIDs,
			Pin:                    pg.SnapshotPin{SnapshotLSN: s.SnapshotLSN, JournalFloor: s.JournalFloor, JournalCeiling: s.JournalCeiling},
		})
	}
	for _, in := range linStore.Inputs {
		lin.Inputs = append(lin.Inputs, pg.RunInput{RunID: in.RunID, UpstreamRunID: in.UpstreamRunID})
	}
	return lin
}
