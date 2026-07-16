package daemon

import (
	"context"
	"io"
	"log/slog"
	"reflect"
	"strings"
	"testing"

	"github.com/MateusAMP2119/iris-lakehouse/internal/dispatch"
	"github.com/MateusAMP2119/iris-lakehouse/internal/store"
	"github.com/MateusAMP2119/iris-lakehouse/internal/store/storetest"
)

// discardLogger is the silent logger the post-pass fakes run under.
func discardLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

// This file proves the lane post-pass drives count-based retention: after a pass,
// each lane pipeline's runs beyond the newest `retain` are pruned through the
// single writer in chunks of pruneBatchSize (per-run archival summary + delete
// triples sharing one meta transaction per chunk, logs deleted via the hook),
// runs held by outstanding dead letters are spared, the prune is scoped to the
// lane's own pipelines, and a lane within retention writes nothing.

// retentionReaderFake scripts the three retention reads and records what the
// post-pass asked for.
type retentionReaderFake struct {
	census  []store.RetentionRunRef
	held    []int64
	records map[int64]store.PrunableRun

	askedIDs   []int64 // ids handed to PrunableRunsByID
	byIDCall   int
	censusCall int
}

func (f *retentionReaderFake) RetentionRuns(context.Context) ([]store.RetentionRunRef, error) {
	f.censusCall++
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

// pruneBatches returns the run ids pruned across the recorded transactions that
// are prune batches (their first statement archives into run_summaries). A batch
// carries one or more per-run statement triples -- summary insert, inputs
// cascade, run delete, in that order -- and ids are collected in issue order.
func pruneBatches(rec *storetest.WriteRecorder) []int64 {
	var ids []int64
	for _, tx := range rec.Transactions() {
		if len(tx) == 0 || !strings.Contains(tx[0].SQL, "INSERT INTO run_summaries") {
			continue
		}
		if len(tx)%3 != 0 {
			return nil // malformed batch: statements must come in per-run triples
		}
		for i := 0; i < len(tx); i += 3 {
			if !strings.Contains(tx[i].SQL, "INSERT INTO run_summaries") ||
				!strings.Contains(tx[i+1].SQL, "DELETE FROM run_inputs") ||
				!strings.Contains(tx[i+2].SQL, "DELETE FROM runs") {
				return nil // malformed triple: summary insert, inputs cascade, run delete -- in that order
			}
			ids = append(ids, tx[i].Args[0].(int64))
		}
	}
	return ids
}

func TestLanePostPassRetentionPrune(t *testing.T) {
	t.Run("lane-post-pass-retention-prune", func(t *testing.T) {
		// Started is non-empty: a pass that started nothing runs no retention work
		// at all, and a lane's FIRST started pass is always due, so each subtest's
		// single AfterPass call prunes immediately.
		report := dispatch.PassReport{Lane: "etl", Pipelines: []string{"p"}, Started: []string{"p"}}

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
			post := &lanePostPass{startedSince: map[string]int{}, 
				submit: recorderSubmitter{rec: rec}, retention: reader, retain: 2,
				deleteLog: logs.delete, logger: discardLogger(),
			}
			if err := post.AfterPass(context.Background(), report); err != nil {
				t.Fatalf("AfterPass: %v", err)
			}
			// The newest 2 (runs 4, 5) survive; 1, 2, 3 are pruned together in ONE
			// atomic batch of per-run triples: summary insert, run_inputs cascade,
			// run delete.
			if got, want := pruneBatches(rec), []int64{1, 2, 3}; !reflect.DeepEqual(got, want) {
				t.Errorf("pruned run batches = %v, want %v", got, want)
			}
			if got := len(rec.Transactions()); got != 1 {
				t.Errorf("prune issued %d transactions for 3 runs, want 1 (runs within a chunk share a transaction)", got)
			}
			// Each pruned run's per-run log is deleted through the hook, by run id.
			if got, want := logs.deleted, []string{"1", "2", "3"}; !reflect.DeepEqual(got, want) {
				t.Errorf("deleted logs = %v, want %v", got, want)
			}
		})

		t.Run("a backlog beyond one chunk prunes in pruneBatchSize transactions", func(t *testing.T) {
			// 600 runs beyond retain: the drain must chunk (256, 256, 88), never one
			// transaction per run (the pathological backlog case) and never one
			// unbounded transaction holding the writer.
			const backlog = 600
			reader := &retentionReaderFake{records: map[int64]store.PrunableRun{}}
			for id := int64(1); id <= backlog+2; id++ {
				reader.census = append(reader.census, runRef(id, "p"))
				if id <= backlog {
					reader.records[id] = prunableRecord(id, "p")
				}
			}
			rec := storetest.NewWriteRecorder()
			logs := &pruneLogSpy{}
			post := &lanePostPass{startedSince: map[string]int{}, 
				submit: recorderSubmitter{rec: rec}, retention: reader, retain: 2,
				deleteLog: logs.delete, logger: discardLogger(),
			}
			if err := post.AfterPass(context.Background(), report); err != nil {
				t.Fatalf("AfterPass: %v", err)
			}
			if got := len(pruneBatches(rec)); got != backlog {
				t.Errorf("pruned %d runs, want %d", got, backlog)
			}
			wantTxs := (backlog + pruneBatchSize - 1) / pruneBatchSize
			if got := len(rec.Transactions()); got != wantTxs {
				t.Errorf("prune issued %d transactions for %d runs, want %d chunks of at most %d", got, backlog, wantTxs, pruneBatchSize)
			}
			if got := len(logs.deleted); got != backlog {
				t.Errorf("deleted %d run logs, want %d (every pruned run's log dies with its row)", got, backlog)
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
			post := &lanePostPass{startedSince: map[string]int{}, 
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
			post := &lanePostPass{startedSince: map[string]int{}, 
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
			post := &lanePostPass{startedSince: map[string]int{}, 
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

		t.Run("an empty pass runs no retention work at all", func(t *testing.T) {
			reader := &retentionReaderFake{census: []store.RetentionRunRef{
				runRef(1, "p"), runRef(2, "p"), runRef(3, "p"),
			}}
			rec := storetest.NewWriteRecorder()
			post := &lanePostPass{startedSince: map[string]int{}, submit: recorderSubmitter{rec: rec}, retention: reader, retain: 1, logger: discardLogger()}
			// Started empty: the run set did not grow, so no census read, no
			// held-set read, no write -- an empty pass costs the post-pass nothing,
			// even with runs beyond retain lingering (they wait for a started pass).
			empty := dispatch.PassReport{Lane: "etl", Pipelines: []string{"p"}}
			if err := post.AfterPass(context.Background(), empty); err != nil {
				t.Fatalf("AfterPass: %v", err)
			}
			if reader.censusCall != 0 {
				t.Errorf("census read %d times on an empty pass, want 0", reader.censusCall)
			}
			if got := len(rec.Statements()); got != 0 {
				t.Errorf("%d statements written on an empty pass, want 0", got)
			}
		})

		t.Run("the prune amortizes on the started-run cadence", func(t *testing.T) {
			reader := &retentionReaderFake{records: map[int64]store.PrunableRun{}}
			rec := storetest.NewWriteRecorder()
			post := &lanePostPass{startedSince: map[string]int{}, submit: recorderSubmitter{rec: rec}, retention: reader, retain: 2, logger: discardLogger()}

			// First started pass of the term: always due (drains a prior term's
			// backlog immediately).
			if err := post.AfterPass(context.Background(), report); err != nil {
				t.Fatalf("AfterPass: %v", err)
			}
			if reader.censusCall != 1 {
				t.Fatalf("census read %d times after the first started pass, want 1 (first pass is due)", reader.censusCall)
			}

			// Subsequent single-run passes accumulate without pruning until the
			// cadence is reached; the census is read once more, at the threshold.
			for i := 0; i < pruneEveryRuns-1; i++ {
				if err := post.AfterPass(context.Background(), report); err != nil {
					t.Fatalf("AfterPass (accumulating): %v", err)
				}
				if reader.censusCall != 1 {
					t.Fatalf("census read %d times after %d accumulated runs, want still 1 (below the cadence)", reader.censusCall, i+1)
				}
			}
			if err := post.AfterPass(context.Background(), report); err != nil {
				t.Fatalf("AfterPass (threshold): %v", err)
			}
			if reader.censusCall != 2 {
				t.Errorf("census read %d times at the cadence threshold, want 2 (one prune per %d started runs)", reader.censusCall, pruneEveryRuns)
			}
		})

		t.Run("an unwired retention seam is a no-op, never a fault", func(t *testing.T) {
			rec := storetest.NewWriteRecorder()
			post := &lanePostPass{startedSince: map[string]int{}, submit: recorderSubmitter{rec: rec}, logger: discardLogger()}
			if err := post.AfterPass(context.Background(), report); err != nil {
				t.Fatalf("AfterPass with no retention seam: %v", err)
			}
			if got := len(rec.Statements()); got != 0 {
				t.Errorf("%d statements written with no retention seam, want 0", got)
			}
		})
	})
}
