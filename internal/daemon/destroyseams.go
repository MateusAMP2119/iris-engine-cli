package daemon

import (
	"context"
	"fmt"

	"github.com/MateusAMP2119/iris-lakehouse/internal/dispatch"
	"github.com/MateusAMP2119/iris-lakehouse/internal/pg"
	"github.com/MateusAMP2119/iris-lakehouse/internal/store"
)

// This file supplies the production implementations of dispatch.Destroyer's
// teardown seams -- what a pipeline destroy actually does beyond retiring meta
// rows. The reverter replays the target's un-promoted disposable data away
// through the same journal-driven revert a workload wipe uses; the run lister
// feeds the archival summaries and the artifact-hash census from the retention
// read seam; the object deleter frees the content-addressed artifact files. The
// leader wires all three at Destroyer construction (leadership.go); the
// dispatch-layer defaults stay open/no-op only for compositions that wire none.

// destroyReverter reverts a pipeline's un-promoted disposable data as the first
// step of its teardown: the same journal-driven reverse-replay a scoped
// `workload wipe <pipeline>` runs (delete disposable inserts, restore
// pre-images, conflict-skip rows with a later still-in-value write), through the
// same ExecuteWipe transaction. It runs before any meta row is retired, so a
// revert failure leaves meta exactly as it was.
type destroyReverter struct {
	reader store.Reader
	data   dataPlane
}

// compile-time proof the reverter satisfies the destroyer's revert seam.
var _ dispatch.DataReverter = destroyReverter{}

// RevertUnpromoted reverts the pipeline's un-promoted disposable writes.
func (r destroyReverter) RevertUnpromoted(ctx context.Context, pipeline string) error {
	runs, err := r.reader.Runs(ctx, store.RunFilter{})
	if err != nil {
		return fmt.Errorf("revert un-promoted data: read runs for attribution: %w", err)
	}
	runPipeline := make(map[int64]string, len(runs))
	for _, run := range runs {
		if id := parseRunID(run.ID); id != 0 {
			runPipeline[id] = run.Pipeline
		}
	}
	if _, err := r.data.ExecuteWipe(ctx, pg.WipeTarget{Pipeline: pipeline, RunPipeline: runPipeline}); err != nil {
		return fmt.Errorf("revert un-promoted data: %w", err)
	}
	return nil
}

// destroyRunLister feeds the destroy teardown's two pre-retirement reads from
// the retention read seam: the remaining runs in archival shape (for the
// summaries written inside the retirement transaction) and the artifact-hash
// census (for the object bytes freed after it commits).
type destroyRunLister struct {
	retention store.RetentionReader
}

// compile-time proof the lister satisfies the destroyer's archival seam.
var _ dispatch.RunLister = destroyRunLister{}

// ListPrunableRuns returns the pipeline's remaining runs in archival shape.
func (l destroyRunLister) ListPrunableRuns(ctx context.Context, pipeline string) ([]store.PrunableRun, error) {
	return l.retention.PrunablePipelineRuns(ctx, pipeline)
}

// ListArtifactHashes returns the pipeline's content-addressed artifact hashes.
func (l destroyRunLister) ListArtifactHashes(ctx context.Context, pipeline string) ([]string, error) {
	return l.retention.ArtifactHashes(ctx, pipeline)
}

// destroyObjectDeleter frees a destroyed pipeline's artifact bytes from the
// object store: one content-addressed delete per hash, run only after the meta
// retirement commits. hash is the artifacts index's primary key, so a hash is
// never shared with a surviving pipeline; an already-absent file is not an
// error, so a re-run teardown stays idempotent.
type destroyObjectDeleter struct {
	objects *store.ObjectStore
}

// compile-time proof the deleter satisfies the destroyer's object-bytes seam.
var _ dispatch.ObjectDeleter = destroyObjectDeleter{}

// DeleteObjects removes the pipeline's artifact files under objects_path.
func (d destroyObjectDeleter) DeleteObjects(_ context.Context, pipeline string, hashes []string) error {
	for _, h := range hashes {
		if err := d.objects.Delete(h); err != nil {
			return fmt.Errorf("destroy pipeline %q: %w", pipeline, err)
		}
	}
	return nil
}
