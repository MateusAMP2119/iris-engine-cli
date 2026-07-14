package api

import (
	"context"
	"errors"
	"net/http"
)

// This file is the daemon's provenance surface: GET
// /provenance/{schema}/{table}/{pk}, the row-level lineage readout that `iris
// data provenance <schema.table> <pk>` prints. It returns the layered write
// history under the read scope alone, carrying stamps with disposition (the
// full list, wiped layers listed) and never row images.
//
// Like other read surfaces, api is a leaf: the seam and wire shapes only. The daemon
// supplies the handler that fetches journal stamps (data db) + run facts and ancestry
// (meta) and runs the pure WalkProvenance; the mux routes and envelopes.

// ProvenanceResult is the document GET /provenance serves and the CLI renders for
// `iris data provenance`. It is lineage only: the stamp list (with per-stamp op/undo
// disposition), current author, authoring run facts, and ancestry edges. No pre_image
// or row data ever appears.
type ProvenanceResult struct {
	Schema string            `json:"schema"`
	Table  string            `json:"table"`
	PK     string            `json:"pk"`
	Stamps []ProvenanceStamp `json:"stamps"`
	// Author is the latest surviving stamp, if any.
	Author   *ProvenanceStamp `json:"author,omitempty"`
	Authored bool             `json:"authored"`
	// Facts for the authoring run (populated when Authored).
	Pipeline            string           `json:"pipeline,omitempty"`
	State               string           `json:"state,omitempty"`
	ArtifactHash        *string          `json:"artifact_hash,omitempty"`
	DeclarationChecksum string           `json:"declaration_checksum,omitempty"`
	FromSummary         bool             `json:"from_summary,omitempty"`
	Ancestry            []ProvenanceEdge `json:"ancestry,omitempty"`
}

// ProvenanceStamp is the image-free projection of one journal layer.
type ProvenanceStamp struct {
	EntryID int64  `json:"entry_id"`
	RunID   int64  `json:"run_id"`
	Op      string `json:"op"`
	Undo    string `json:"undo"`
}

// ProvenanceEdge is one ancestry step (run consumed upstream).
type ProvenanceEdge struct {
	RunID         int64 `json:"run_id"`
	UpstreamRunID int64 `json:"upstream_run_id"`
	Depth         int   `json:"depth"`
}

// ProvenanceHandler serves the provenance lineage walk. The daemon implements it
// by querying the journal for the row's stamps and the meta lineage (runs/summaries/inputs),
// then WalkProvenance; the mux depends only on this interface.
type ProvenanceHandler interface {
	// Provenance returns the lineage report for the (schema, table, pk). Not found
	// (no stamps) is an error the handler returns (caller maps to 404 or appropriate).
	Provenance(ctx context.Context, schema, table, pk string) (ProvenanceResult, error)
}

// ErrProvenanceUnavailable is returned by the default (unwired) handler.
var ErrProvenanceUnavailable = errors.New("api: provenance not available")

// WithProvenance wires the provenance handler for GET /provenance/{...}.
func WithProvenance(h ProvenanceHandler) MuxOption {
	return func(m *mux) {
		if h != nil {
			m.provenance = h
		}
	}
}

// noProvenance is the default before wiring.
type noProvenance struct{}

func (noProvenance) Provenance(context.Context, string, string, string) (ProvenanceResult, error) {
	return ProvenanceResult{}, ErrProvenanceUnavailable
}

// serveProvenance handles GET /provenance/{schema}/{table}/{pk}.
// It is a read under read scope. Unwired or error -> 500 internal.
func (m *mux) serveProvenance(w http.ResponseWriter, r *http.Request, schema, table, pk string) {
	if r.Method != http.MethodGet {
		WriteError(w, http.StatusMethodNotAllowed, "method_not_allowed", "GET "+r.URL.Path+" only")
		return
	}
	if !noParams(w, r) {
		return
	}
	payload, err := m.provenance.Provenance(r.Context(), schema, table, pk)
	if err != nil {
		// Distinguish not-found? For now internal maps missing data; handlers can
		// return specific later. A row with stamps returns the layers.
		WriteError(w, http.StatusInternalServerError, "internal", err.Error())
		return
	}
	WriteData(w, http.StatusOK, payload)
}
