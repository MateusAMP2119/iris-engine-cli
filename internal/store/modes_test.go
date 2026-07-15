package store_test

import (
	"strings"
	"testing"

	"github.com/MateusAMP2119/iris-lakehouse/internal/store"
)

// TestArtifactDataModeMatrix proves the artifact/data mode matrix: artifact
// (source/built) and data (disposable/permanent) toggle independently, but
// permanent requires built. Validation accepts source+disposable (dev),
// built+disposable (throwaway-data artifact test), and built+permanent
// (production), and blocks source+permanent so loose source never writes
// permanent data. A value outside either CHECK set is rejected rather than waved
// through, so a zero value can never slip past the matrix.
func TestArtifactDataModeMatrix(t *testing.T) {
	cases := []struct {
		name     string
		artifact store.Artifact
		mode     store.DataMode
		ok       bool
	}{
		{"source+disposable (dev)", store.ArtifactSource, store.DataDisposable, true},
		{"built+disposable (throwaway-data artifact test)", store.ArtifactBuilt, store.DataDisposable, true},
		{"built+permanent (production)", store.ArtifactBuilt, store.DataPermanent, true},
		{"source+permanent (blocked)", store.ArtifactSource, store.DataPermanent, false},
		{"unknown artifact mode", store.Artifact("loose"), store.DataDisposable, false},
		{"unknown data mode", store.ArtifactBuilt, store.DataMode("forever"), false},
		{"zero values", store.Artifact(""), store.DataMode(""), false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := store.ValidateModeMatrix(tc.artifact, tc.mode)
			if tc.ok && err != nil {
				t.Fatalf("ValidateModeMatrix(%q, %q) = %v, want accepted", tc.artifact, tc.mode, err)
			}
			if !tc.ok && err == nil {
				t.Fatalf("ValidateModeMatrix(%q, %q) accepted, want blocked", tc.artifact, tc.mode)
			}
		})
	}

	// The blocked cell's refusal names the rule, not just the pair: loose source
	// never writes permanent data, and building is the way out.
	err := store.ValidateModeMatrix(store.ArtifactSource, store.DataPermanent)
	if err == nil {
		t.Fatal("source+permanent accepted, want blocked")
	}
	for _, want := range []string{"source", "permanent", "built"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("source+permanent refusal does not mention %q: %v", want, err)
		}
	}
}
