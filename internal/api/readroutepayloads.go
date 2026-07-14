package api

// This file is the wire-shape half of the E14 read routes: the JSON payloads
// the daemon's runs, trace, and gate planes return and the mux renders, and the
// CLI (the one client) decodes. Like every wire type here api stays a leaf --
// these are plain structs, reaching nothing up the stack -- so the daemon maps
// its store/dispatch models onto them and the mux emits them verbatim.
//
// The runs collection is an object with a runs array ({ "data": { "runs": [...] } }),
// the same shape `iris run list` decodes and the rail renderer draws; each row
// carries its consumed upstream ids and replayed_from as plain attributes only when
// include=inputs was asked (parents-per-row, never a separate edge array). The trace
// and gate payloads mirror the walks the CLI prints, with the verdict enum closed by
// the daemon before it reaches the wire.

// RunRow is one run of the /runs collection: the lineage attributes an external
// renderer draws. Inputs and ReplayedFrom are present only under include=inputs
// (omitted otherwise): Inputs is the consumed upstream run ids (each a solid
// edge), ReplayedFrom the replaced run (an annotation, never an edge). An
// upstream id may name a run since pruned -- run_inputs is FK-free -- so it is
// carried verbatim, the renderer's gap to draw, never resolved away.
type RunRow struct {
	// ID is the run's meta id.
	ID string `json:"id"`
	// Pipeline is the run's pipeline.
	Pipeline string `json:"pipeline"`
	// State is the run's lifecycle state.
	State string `json:"state"`
	// Inputs are the consumed upstream run ids (include=inputs only), one solid
	// edge each; omitted when inputs were not requested.
	Inputs []string `json:"inputs,omitempty"`
	// ReplayedFrom is the replaced run id (include=inputs only), an annotation
	// never an edge; omitted when the run is not a replay or inputs were not asked.
	ReplayedFrom string `json:"replayed_from,omitempty"`
}

// RunsCollection is the body of GET /runs: an object with a runs array, so the
// envelope is { "data": { "runs": [...] } } -- the shape `iris run list` decodes.
// The NDJSON stream (Accept: application/x-ndjson) serves the same rows one per
// line with no envelope (asRows unwraps this collection for it).
type RunsCollection struct {
	// Runs is the run history, newest first (ordering identity, never a clock).
	// Always present, possibly empty.
	Runs []RunRow `json:"runs"`
}

// TraceEdge is one step of a run's ancestry walk: run RunID consumed upstream run
// UpstreamRunID, Depth levels from the queried run (direct edges are depth 1). It
// is the same edge `iris run show <run> --trace` prints, over run_inputs.
type TraceEdge struct {
	// RunID is the consuming run (the queried run's side of the edge, up-walk).
	RunID string `json:"run_id"`
	// UpstreamRunID is the consumed upstream run.
	UpstreamRunID string `json:"upstream_run_id"`
	// Depth is how many edges from the queried run this step sits.
	Depth int `json:"depth"`
}

// RunTracePayload is the body of GET /runs/{id}/trace?direction=up|down: the run's
// ancestry walk over run_inputs (up), or who consumed it (down). It is the same
// walk `iris run show <run> --trace [--down]` prints.
type RunTracePayload struct {
	// Run is the queried run id.
	Run string `json:"run"`
	// Direction is the resolved walk direction (up or down; up is the default).
	Direction string `json:"direction"`
	// Ancestry is the walked edges, in walk order (by depth, then run, then
	// upstream). Always present, possibly empty.
	Ancestry []TraceEdge `json:"ancestry"`
}

// PipelineGatePayload is the body of GET /pipelines/{name}/gate: the pipeline's
// per-edge depends_on gate ledger, the same ledger `iris pipeline show` prints.
// Each row's verdict is from the closed set (open, up_to_date, pending,
// poisoned), closed by the daemon before it reaches the wire.
type PipelineGatePayload struct {
	// Pipeline is the queried pipeline name.
	Pipeline string `json:"pipeline"`
	// Gate is the per-edge verdict ledger, in edge order. Always present, possibly
	// empty (a pipeline with no depends_on edges has an empty ledger).
	Gate []EdgeVerdictView `json:"gate"`
}
