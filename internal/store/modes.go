package store

import "fmt"

// This file holds the artifact/data mode matrix. The two per-pipeline modes
// toggle independently -- artifact: source or built; data: disposable or
// permanent -- but not every cell of the matrix is legal:
//
//	source + disposable   valid (dev: fast iteration, wipe-eligible data)
//	built  + disposable   valid (throwaway-data artifact test)
//	built  + permanent    valid (production)
//	source + permanent    BLOCKED: loose source never writes permanent data
//
// ValidateModeMatrix is the registration-side half of the mode policy: it judges
// a pipeline's declared mode pair before it lands in meta. The write-side half is
// pg.PermanentRequiresBuilt, the durability gate that refuses a permanent-mode
// write from an un-built pipeline at capture time; the two enforce the same
// blocked cell at their respective boundaries.

// ValidateModeMatrix validates one artifact/data mode combination against the
// mode matrix. It accepts source+disposable, built+disposable, and
// built+permanent; it blocks source+permanent, because permanent data requires a
// built artifact -- loose source never writes permanent data. A value outside
// either CHECK set is rejected too, so an unset or garbage mode can never pass as
// valid.
func ValidateModeMatrix(artifact Artifact, mode DataMode) error {
	if artifact != ArtifactSource && artifact != ArtifactBuilt {
		return fmt.Errorf("store: unknown artifact mode %q (valid: %s, %s)", artifact, ArtifactSource, ArtifactBuilt)
	}
	if mode != DataDisposable && mode != DataPermanent {
		return fmt.Errorf("store: unknown data mode %q (valid: %s, %s)", mode, DataDisposable, DataPermanent)
	}
	if artifact == ArtifactSource && mode == DataPermanent {
		return fmt.Errorf("store: artifact mode %q cannot carry data mode %q; permanent data requires a built artifact (loose source never writes permanent data)", ArtifactSource, DataPermanent)
	}
	return nil
}
