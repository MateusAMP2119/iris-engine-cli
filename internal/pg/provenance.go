package pg

import "sort"

// This file owns the pure provenance walk: the query logic behind
// `iris data provenance <schema.table> <pk>` and the read model behind
// GET /provenance/{schema}/{table}/{pk} (specification sections 4 and 14).
// The walk is three indexed lookups on plain relational tables -- no graph
// store, no extension -- modeled here entirely over in-memory fixtures so
// every rule is unit-testable and the live wiring stays a dumb reader:
//
//  1. Row -> run. The provenance key (schema, table, row_pk) returns the
//     row's stamps; the latest SURVIVING stamp names the current author
//     (a wiped write is no longer in the row's value; a skipped one is),
//     and wiped layers stay listed, never hidden. The full list is the
//     layered write history (S14/provenance-row-to-run,
//     S14/provenance-current-author-surviving).
//
//  2. Run -> facts. The run id resolves to pipeline, state, artifact hash,
//     declaration checksum, and the snapshot pin, from the live run row or,
//     once the row is pruned, from the archival summary -- same facts,
//     equally true (S14/provenance-run-facts-summary-fallback,
//     S03/provenance-survives-pruning).
//
//  3. Run -> ancestry. run_inputs is walked upward, one edge per consumed
//     upstream (fan-in = several edges), at depth 1 by default, with full
//     recursive ancestry available from a single walk -- live SQL: one
//     WITH RECURSIVE statement over run_inputs, acyclic by apply-time
//     validation. A pruned run's own ledger rows are gone; its parents come
//     from the summary's consumed-upstream list, so lineage never dangles
//     (S14/provenance-ancestry-recursive).
//
// The walk returns lineage only -- stamps, run facts, ancestry edges -- and
// never a row image: Stamp is the image-free projection of a journal entry,
// and no type in the report graph carries a pre-image field
// (S14/provenance-lineage-never-images).

// DefaultAncestryDepth is how far the ancestry walk climbs when the caller
// does not say: one level, the run's directly consumed upstreams
// (specification section 14, "depth 1 default").
const DefaultAncestryDepth = 1

// FullAncestry asks the ancestry walk for the whole upward DAG in one call:
// the pure model of the single WITH RECURSIVE query
// (`iris run show <run> --trace`).
const FullAncestry = -1

// Stamp is one journal layer of a row's write history as provenance returns
// it: the image-free projection of a JournalEntry. The pre-image never rides
// along -- provenance returns lineage, never images.
type Stamp struct {
	// EntryID is the journal entry's id: the layer's position in the row's
	// commit-ordered history.
	EntryID int64
	// RunID is the writing run (the row -> run link).
	RunID int64
	// Op is the captured write operation.
	Op WriteOp
	// Undo is the layer's undo lifecycle state; wiped layers stay listed.
	Undo UndoState
}

// Surviving reports whether the stamp's write is still in the row's value:
// everything but a wiped layer is (a conflict-skipped write was left in
// place; promoted and open writes were never reverted).
func (s Stamp) Surviving() bool {
	return s.Undo != UndoWiped
}

// RowStamps is lookup one of the walk: the provenance key's stamps, newest
// first (descending journal id -- ids per row are strictly commit-ordered).
// The full list is the row's layered write history, wiped layers included.
func RowStamps(journal []JournalEntry, key RowKey) []Stamp {
	var stamps []Stamp
	for _, e := range journal {
		if e.Key() == key {
			stamps = append(stamps, Stamp{EntryID: e.ID, RunID: e.RunID, Op: e.Op, Undo: e.Undo})
		}
	}
	sort.Slice(stamps, func(i, j int) bool { return stamps[i].EntryID > stamps[j].EntryID })
	return stamps
}

// CurrentAuthor resolves a row's current authoring write: the latest
// surviving stamp. A row whose newest layers are all wiped resolves to the
// latest non-wiped one; a row with no surviving layer has no current author
// (ok = false) while its history stays listed.
func CurrentAuthor(stamps []Stamp) (Stamp, bool) {
	best, ok := Stamp{}, false
	for _, s := range stamps {
		if s.Surviving() && (!ok || s.EntryID > best.EntryID) {
			best, ok = s, true
		}
	}
	return best, ok
}

// SnapshotPin is the three-value pin naming a run's input state
// (specification section 14): the data database's LSN and journal high id at
// dispatch, and the journal high id at terminal transition. Nil models SQL
// NULL (e.g. a run pinned before its terminal transition has no ceiling).
type SnapshotPin struct {
	// SnapshotLSN is the data database's LSN at dispatch.
	SnapshotLSN *string
	// JournalFloor is the journal high id at dispatch.
	JournalFloor *int64
	// JournalCeiling is the journal high id at terminal transition.
	JournalCeiling *int64
}

// RunRecord is the live runs-row projection the walk reads: exactly the
// fields lookup two returns (specification section 4, runs). Fields the walk
// never returns (handle, log_ref, exit_code, ...) are omitted: this is the
// provenance projection of a run row, not a second schema.
type RunRecord struct {
	// RunID is the run's meta identity (runs.id).
	RunID int64
	// Pipeline is the run's pipeline name.
	Pipeline string
	// State is the run's state (runs.state value set).
	State string
	// ArtifactHash is the built binary's content hash, nil for a dev run.
	ArtifactHash *string
	// DeclarationChecksum is the hash of the declaration the run executed.
	DeclarationChecksum string
	// Pin is the run's snapshot pin.
	Pin SnapshotPin
}

// ArchivalSummary is the run_summaries projection the walk falls back to once
// a run row is pruned (specification section 4, run_summaries): the same
// facts as RunRecord plus the consumed-upstream list the pruner copied out of
// run_inputs, so ancestry never dangles.
type ArchivalSummary struct {
	// RunID is the summarized run's identity (run_summaries.run_id).
	RunID int64
	// Pipeline is the run's pipeline name (copied, never an FK).
	Pipeline string
	// State is the run's terminal state.
	State string
	// ArtifactHash is the built binary's content hash, nil for a dev run.
	ArtifactHash *string
	// DeclarationChecksum is the declaration hash the run executed.
	DeclarationChecksum string
	// ConsumedUpstreamRunIDs are the upstream run ids the run consumed: the
	// pruned run's run_inputs rows, preserved in the summary.
	ConsumedUpstreamRunIDs []int64
	// Pin is the run's snapshot pin, surviving pruning.
	Pin SnapshotPin
}

// RunInput is one run_inputs consumption-ledger row: run RunID consumed
// upstream run UpstreamRunID (one row per consumed upstream, written once at
// run start, never mutated).
type RunInput struct {
	// RunID is the consuming run.
	RunID int64
	// UpstreamRunID is the consumed upstream run.
	UpstreamRunID int64
}

// Lineage is the meta-side fixture the walk's run lookups read: the
// provenance projections of runs, run_summaries, and run_inputs. The live
// wiring fills it from meta; the model never cares which.
type Lineage struct {
	// Runs are the live run rows.
	Runs []RunRecord
	// Summaries are the archival summaries of pruned runs.
	Summaries []ArchivalSummary
	// Inputs are the consumption-ledger rows of the surviving runs.
	Inputs []RunInput
}

// RunFacts is lookup two's answer: what provenance names about a run --
// pipeline, state, artifact hash, declaration checksum, pin -- and which tier
// answered.
type RunFacts struct {
	// RunID is the resolved run.
	RunID int64
	// Pipeline is the run's pipeline name.
	Pipeline string
	// State is the run's state.
	State string
	// ArtifactHash is the built binary's exact content hash, nil for a dev run.
	ArtifactHash *string
	// DeclarationChecksum is the exact declaration hash the run executed.
	DeclarationChecksum string
	// Pin is the run's snapshot pin.
	Pin SnapshotPin
	// FromSummary reports that the run row was pruned and the archival
	// summary answered: slower to have needed, equally true.
	FromSummary bool
}

// RunFacts resolves a run id to its facts: the live run row when present,
// else the archival summary (FromSummary set), else nothing -- a reference
// resolves to a run or its summary, never to a hole, so ok = false only for
// an id no tier has ever known.
func (l Lineage) RunFacts(runID int64) (RunFacts, bool) {
	for _, r := range l.Runs {
		if r.RunID == runID {
			return RunFacts{
				RunID: r.RunID, Pipeline: r.Pipeline, State: r.State,
				ArtifactHash: r.ArtifactHash, DeclarationChecksum: r.DeclarationChecksum,
				Pin: r.Pin,
			}, true
		}
	}
	for _, s := range l.Summaries {
		if s.RunID == runID {
			return RunFacts{
				RunID: s.RunID, Pipeline: s.Pipeline, State: s.State,
				ArtifactHash: s.ArtifactHash, DeclarationChecksum: s.DeclarationChecksum,
				Pin: s.Pin, FromSummary: true,
			}, true
		}
	}
	return RunFacts{}, false
}

// AncestryEdge is one upward step of run ancestry: run RunID consumed
// upstream run UpstreamRunID, reached Depth levels above the walk's root
// (direct upstreams are depth 1). One edge per consumed upstream: fan-in is
// several edges, and a diamond ancestor is listed once per consumer.
type AncestryEdge struct {
	// RunID is the consuming run.
	RunID int64
	// UpstreamRunID is the consumed upstream run.
	UpstreamRunID int64
	// Depth is how many levels above the root the edge sits.
	Depth int
}

// parents returns a run's consumed upstream ids: its run_inputs rows when it
// has any, else the archival summary's consumed-upstream list (a pruned run's
// own ledger rows are cascaded away; the summary preserves them, so ancestry
// never dangles). A run with no rows in either tier consumed nothing.
func (l Lineage) parents(runID int64) []int64 {
	var ups []int64
	for _, in := range l.Inputs {
		if in.RunID == runID {
			ups = append(ups, in.UpstreamRunID)
		}
	}
	if len(ups) > 0 {
		return ups
	}
	for _, s := range l.Summaries {
		if s.RunID == runID {
			return s.ConsumedUpstreamRunIDs
		}
	}
	return nil
}

// Ancestry is lookup three of the walk: run ancestry climbed upward from
// root via run_inputs (summary fallback for pruned runs), one edge per
// consumed upstream. depth bounds the climb: DefaultAncestryDepth when zero
// or unspecified, FullAncestry (or any negative) for the whole upward DAG in
// one call -- the pure model of the single WITH RECURSIVE query
// (RenderAncestryTrace). Each run's own fan-out is expanded once (the graph
// is acyclic by apply-time validation; a diamond still lists one edge per
// consumer). Edges come back in walk order: by depth, then consumer, then
// upstream.
func (l Lineage) Ancestry(root int64, depth int) []AncestryEdge {
	if depth == 0 {
		depth = DefaultAncestryDepth
	}
	var edges []AncestryEdge
	expanded := map[int64]bool{}
	frontier := []int64{root}
	for level := 1; len(frontier) > 0 && (depth < 0 || level <= depth); level++ {
		var next []int64
		var levelEdges []AncestryEdge
		for _, run := range frontier {
			if expanded[run] {
				continue
			}
			expanded[run] = true
			for _, up := range l.parents(run) {
				levelEdges = append(levelEdges, AncestryEdge{RunID: run, UpstreamRunID: up, Depth: level})
				next = append(next, up)
			}
		}
		sort.Slice(levelEdges, func(i, j int) bool {
			if levelEdges[i].RunID != levelEdges[j].RunID {
				return levelEdges[i].RunID < levelEdges[j].RunID
			}
			return levelEdges[i].UpstreamRunID < levelEdges[j].UpstreamRunID
		})
		edges = append(edges, levelEdges...)
		frontier = next
	}
	return edges
}

// RenderAncestryTrace returns the single WITH RECURSIVE statement that
// answers full ancestry live (specification section 14: "full ancestry one
// WITH RECURSIVE"; surfaced as `iris run show <run> --trace`). One statement,
// parameterized on the root run id ($1), walking run_inputs upward over its
// primary key; the graph is acyclic by apply-time validation. The model
// (Lineage.Ancestry) and this rendering answer identically for surviving
// runs; the wiring task executes it against meta.
func RenderAncestryTrace() string {
	return `WITH RECURSIVE ancestry (run_id, upstream_run_id, depth) AS (
    SELECT run_id, upstream_run_id, 1
    FROM run_inputs
    WHERE run_id = $1
  UNION
    SELECT ri.run_id, ri.upstream_run_id, a.depth + 1
    FROM run_inputs ri
    JOIN ancestry a ON ri.run_id = a.upstream_run_id
)
SELECT run_id, upstream_run_id, depth
FROM ancestry
ORDER BY depth, run_id, upstream_run_id`
}

// ProvenanceReport is the walk's whole answer for one row: lineage only --
// stamps, the current author, the author's run facts, ancestry -- and never a
// row image or pre-image payload.
type ProvenanceReport struct {
	// Row is the queried provenance key.
	Row RowKey
	// Stamps is the row's layered write history, newest first, wiped layers
	// listed, never hidden.
	Stamps []Stamp
	// Author is the latest surviving stamp: the current authoring write.
	// Meaningful only when Authored is true.
	Author Stamp
	// Authored reports whether any layer survives; false leaves Author zero
	// (every layer wiped: history listed, no current author).
	Authored bool
	// Facts are the authoring run's facts. Meaningful only when
	// FactsResolved is true.
	Facts RunFacts
	// FactsResolved reports that the authoring run resolved to a run row or
	// its archival summary.
	FactsResolved bool
	// Ancestry is the authoring run's upward consumption lineage.
	Ancestry []AncestryEdge
}

// WalkProvenance runs the whole three-lookup walk for one provenance key:
// the row's stamps, the current author (latest surviving stamp), the
// authoring run's facts (live row or archival summary), and the authoring
// run's ancestry at the given depth (DefaultAncestryDepth when zero,
// FullAncestry for the whole DAG). A row with no stamps was never captured:
// found = false, nothing speculative.
func WalkProvenance(journal []JournalEntry, lineage Lineage, key RowKey, depth int) (ProvenanceReport, bool) {
	stamps := RowStamps(journal, key)
	if len(stamps) == 0 {
		return ProvenanceReport{}, false
	}
	report := ProvenanceReport{Row: key, Stamps: stamps}
	report.Author, report.Authored = CurrentAuthor(stamps)
	if report.Authored {
		report.Facts, report.FactsResolved = lineage.RunFacts(report.Author.RunID)
		report.Ancestry = lineage.Ancestry(report.Author.RunID, depth)
	}
	return report, true
}
