package daemon

import (
	"context"
	"io"
	"log/slog"
	"reflect"
	"strings"
	"testing"

	"github.com/MateusAMP2119/iris-engine-cli/internal/dispatch"
	"github.com/MateusAMP2119/iris-engine-cli/internal/store"
	"github.com/MateusAMP2119/iris-engine-cli/internal/store/storetest"
)

// discardLogger is the silent logger the post-pass fakes run under.
func discardLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

// This file proves the lane post-pass drives count-based retention: after a pass,
// each lane pipeline's runs beyond the newest `retain` are pruned through the
// single writer (archival summary + delete in one meta transaction, log deleted
// via the hook), runs held by outstanding dead letters are spared, the prune is
// scoped to the lane's own pipelines, and a lane within retention writes nothing.

// retentionReaderFake scripts the three retention reads and records what the
// post-pass asked for.
type retentionReaderFake struct {
	census  []store.RetentionRunRef
	held    []int64
	records map[int64]store.PrunableRun

	askedIDs []int64 // ids handed to PrunableRunsByID
	byIDCall int
}

func (f *retentionReaderFake) RetentionRuns(context.Context) ([]store.RetentionRunRef, error) {
	return append([]store.RetentionRunRef(nil), f.census...), nil
}

func (f *retentionReaderFake) OutstandingDeadLetterRuns(context.Context) ([]int64, error) {
	return append([]int64(nil), f.held...), nil
}

func (f *retentionReaderFake) PrunableRunsByID(_ context.Context, ids []int64) ([]store.PrunableRun, error) {
	f.byIDCall++
	f.askedIDs = append([]int64(nil), ids...)
	var out []store.PrunableRun
	for _, id := range ids {
		if rec, ok := f.records[id]; ok {
			out = append(out, rec)
		}
	}
	return out, nil
}

func (f *retentionReaderFake) PrunablePipelineRuns(context.Context, string) ([]store.PrunableRun, error) {
	return nil, nil
}

func (f *retentionReaderFake) ArtifactHashes(context.Context, string) ([]string, error) {
	return nil, nil
}

// recorderSubmitter is a dispatch.Submitter handing each mutation the single
// writer over the recording write connection, so a test reads the exact
// statements the prune submitted.
type recorderSubmitter struct{ rec *storetest.WriteRecorder }

func (s recorderSubmitter) Submit(_ context.Context, fn func(*store.Writer) error) error {
	return fn(store.NewWriter(s.rec))
}

// pruneLogSpy records the run ids whose per-run logs were deleted.
type pruneLogSpy struct{ deleted []string }

func (p *pruneLogSpy) delete(runID string) error {
	p.deleted = append(p.deleted, runID)
	return nil
}

// runRef abbreviates a census row.
func runRef(id int64, pipeline string) store.RetentionRunRef {
	return store.RetentionRunRef{RunID: id, Pipeline: pipeline}
}

// prunableRecord builds the archival record the fake returns for id.
func prunableRecord(id int64, pipeline string) store.PrunableRun {
	return store.PrunableRun{RunID: id, Pipeline: pipeline, State: store.RunSucceeded, DeclarationChecksum: "c"}
}

// pruneBatches returns the recorded transactions that are prune batches (their
// first statement archives into run_summaries), each keyed by the pruned run id.
func pruneBatches(rec *storetest.WriteRecorder) []int64 {
	var ids []int64
	for _, tx := range rec.Transactions() {
		if len(tx) == 0 || !strings.Contains(tx[0].SQL, "INSERT INTO run_summaries") {
			continue
		}
		if len(tx) != 3 || !strings.Contains(tx[1].SQL, "DELETE FROM run_inputs") || !strings.Contains(tx[2].SQL, "DELETE FROM runs") {
			return nil // malformed batch: summary insert, inputs cascade, run delete -- in that order
		}
		ids = append(ids, tx[0].Args[0].(int64))
	}
	return ids
}

func TestLanePostPassRetentionPrune(t *testing.T) {
	t.Run("lane-post-pass-retention-prune", func(t *testing.T) {
		report := dispatch.PassReport{Lane: "etl", Pipelines: []string{"p"}}

		t.Run("runs beyond the newest retain are pruned atomically, logs die with rows", func(t *testing.T) {
			reader := &retentionReaderFake{
				census: []store.RetentionRunRef{
					runRef(1, "p"), runRef(2, "p"), runRef(3, "p"), runRef(4, "p"), runRef(5, "p"),
				},
				records: map[int64]store.PrunableRun{
					1: prunableRecord(1, "p"), 2: prunableRecord(2, "p"), 3: prunableRecord(3, "p"),
				},
			}
			rec := storetest.NewWriteRecorder()
			logs := &pruneLogSpy{}
			post := lanePostPass{
				submit: recorderSubmitter{rec: rec}, retention: reader, retain: 2,
				deleteLog: logs.delete, logger: discardLogger(),
			}
			if err := post.AfterPass(context.Background(), report); err != nil {
				t.Fatalf("AfterPass: %v", err)
			}
			// The newest 2 (runs 4, 5) survive; 1, 2, 3 are pruned, each in one atomic
			// batch: summary insert, run_inputs cascade, run delete.
			if got, want := pruneBatches(rec), []int64{1, 2, 3}; !reflect.DeepEqual(got, want) {
				t.Errorf("pruned run batches = %v, want %v", got, want)
			}
			// Each pruned run's per-run log is deleted through the hook, by run id.
			if got, want := logs.deleted, []string{"1", "2", "3"}; !reflect.DeepEqual(got, want) {
				t.Errorf("deleted logs = %v, want %v", got, want)
			}
		})

		t.Run("a run held by an outstanding dead letter is spared", func(t *testing.T) {
			reader := &retentionReaderFake{
				census: []store.RetentionRunRef{
					runRef(1, "p"), runRef(2, "p"), runRef(3, "p"), runRef(4, "p"), runRef(5, "p"),
				},
				held: []int64{2},
				records: map[int64]store.PrunableRun{
					1: prunableRecord(1, "p"), 3: prunableRecord(3, "p"),
				},
			}
			rec := storetest.NewWriteRecorder()
			post := lanePostPass{
				submit: recorderSubmitter{rec: rec}, retention: reader, retain: 2,
				deleteLog: (&pruneLogSpy{}).delete, logger: discardLogger(),
			}
			if err := post.AfterPass(context.Background(), report); err != nil {
				t.Fatalf("AfterPass: %v", err)
			}
			if got, want := pruneBatches(rec), []int64{1, 3}; !reflect.DeepEqual(got, want) {
				t.Errorf("pruned run batches = %v, want %v (run 2 is dead-letter-held and spared)", got, want)
			}
		})

		t.Run("the prune is scoped to the lane's own pipelines", func(t *testing.T) {
			reader := &retentionReaderFake{
				census: []store.RetentionRunRef{
					runRef(1, "p"), runRef(2, "p"), runRef(3, "p"),
					// another lane's pipeline, beyond retain: never this post-pass's to prune.
					runRef(10, "other"), runRef(11, "other"), runRef(12, "other"),
				},
				records: map[int64]store.PrunableRun{1: prunableRecord(1, "p")},
			}
			rec := storetest.NewWriteRecorder()
			post := lanePostPass{
				submit: recorderSubmitter{rec: rec}, retention: reader, retain: 2,
				deleteLog: (&pruneLogSpy{}).delete, logger: discardLogger(),
			}
			if err := post.AfterPass(context.Background(), report); err != nil {
				t.Fatalf("AfterPass: %v", err)
			}
			if got, want := reader.askedIDs, []int64{1}; !reflect.DeepEqual(got, want) {
				t.Errorf("prunable ids requested = %v, want %v (only the lane's own pipeline)", got, want)
			}
			if got, want := pruneBatches(rec), []int64{1}; !reflect.DeepEqual(got, want) {
				t.Errorf("pruned run batches = %v, want %v", got, want)
			}
		})

		t.Run("a lane within retention writes nothing", func(t *testing.T) {
			reader := &retentionReaderFake{
				census: []store.RetentionRunRef{runRef(1, "p"), runRef(2, "p")},
			}
			rec := storetest.NewWriteRecorder()
			post := lanePostPass{
				submit: recorderSubmitter{rec: rec}, retention: reader, retain: 2,
				deleteLog: (&pruneLogSpy{}).delete, logger: discardLogger(),
			}
			if err := post.AfterPass(context.Background(), report); err != nil {
				t.Fatalf("AfterPass: %v", err)
			}
			if reader.byIDCall != 0 {
				t.Errorf("PrunableRunsByID called %d times for a within-retention lane, want 0", reader.byIDCall)
			}
			if got := len(rec.Statements()); got != 0 {
				t.Errorf("%d statements written for a within-retention lane, want 0", got)
			}
		})

		t.Run("an unwired retention seam is a no-op, never a fault", func(t *testing.T) {
			rec := storetest.NewWriteRecorder()
			post := lanePostPass{submit: recorderSubmitter{rec: rec}, logger: discardLogger()}
			if err := post.AfterPass(context.Background(), report); err != nil {
				t.Fatalf("AfterPass with no retention seam: %v", err)
			}
			if got := len(rec.Statements()); got != 0 {
				t.Errorf("%d statements written with no retention seam, want 0", got)
			}
		})
	})
}
