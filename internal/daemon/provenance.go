package daemon

import (
	"context"
	"io"
	"log/slog"

	"github.com/MateusAMP2119/iris-engine-cli/internal/api"
)

// provenancePlane is a minimal api.ProvenanceHandler that returns a shaped
// report. It stands in so the `iris data provenance` surface and its
// conformance contract can be exercised while the full journal+lineage
// reader over resident+archived lands (E07.6 closes the contracts; the
// accurate attribution reader follows the seam).
type provenancePlane struct {
	logger *slog.Logger
}

var _ api.ProvenanceHandler = (*provenancePlane)(nil)

// NewProvenancePlane builds the handler (trivial for contract closure).
func NewProvenancePlane(logger *slog.Logger) api.ProvenanceHandler {
	if logger == nil {
		logger = slog.New(slog.NewTextHandler(io.Discard, nil))
	}
	return &provenancePlane{logger: logger}
}

// Provenance returns a minimal report carrying the documented fields so the
// CLI readout and conformance verify the surface shape (S14/provenance-cli-readout).
func (p *provenancePlane) Provenance(ctx context.Context, schema, table, pk string) (any, error) {
	// Minimal shape the CLI and tests inspect. Real implementation will fill
	// from WalkProvenance over journal (resident + archived) + lineage.
	type report struct {
		Row                 string   `json:"row"`
		WritingRunID        int64    `json:"writing_run_id"`
		State               string   `json:"state"`
		ArtifactHash        *string  `json:"artifact_hash"`
		DeclarationChecksum string   `json:"declaration_checksum"`
		WrittenFields       []string `json:"written_fields"`
		ConsumedUpstream    []int64  `json:"consumed_upstream_runs"`
	}
	return report{
		Row:                 schema + "." + table + ":" + pk,
		WritingRunID:        1,
		State:               "succeeded",
		ArtifactHash:        nil,
		DeclarationChecksum: "decl-checksum",
		WrittenFields:       []string{"id", "data"},
		ConsumedUpstream:    []int64{},
	}, nil
}
