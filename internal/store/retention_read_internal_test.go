package store

import (
	"context"
	"fmt"
	"reflect"
	"testing"
)

// This file proves the meta-backed retention reader over a scripted pool (no live
// Postgres): the census read returns every run's (id, pipeline), the held read
// returns the outstanding dead-letter run ids, and the archival read stitches each
// selected run's consumed upstream ids from run_inputs and passes the selected ids
// through as the query parameter. The terminal-only state predicate is SQL-side
// and is pinned by the statement text asserted here; its behaviour against a real
// Postgres rides the conformance tier like every other reader's.

// retentionScriptRows is a poolRows fake over a fixed result set for the retention
// reader's scan shapes: int64, string, nullable *string and *int64 columns.
type retentionScriptRows struct {
	rows   [][]any
	cursor int
}

func (r *retentionScriptRows) Next() bool {
	if r.cursor >= len(r.rows) {
		return false
	}
	r.cursor++
	return true
}

func (r *retentionScriptRows) Scan(dest ...any) error {
	row := r.rows[r.cursor-1]
	if len(dest) != len(row) {
		return fmt.Errorf("retentionScriptRows: scan into %d dests, row has %d columns", len(dest), len(row))
	}
	for i, d := range dest {
		switch p := d.(type) {
		case *int64:
			*p = row[i].(int64)
		case *string:
			*p = row[i].(string)
		case **string:
			if row[i] == nil {
				*p = nil
			} else {
				v := row[i].(string)
				*p = &v
			}
		case **int64:
			if row[i] == nil {
				*p = nil
			} else {
				v := row[i].(int64)
				*p = &v
			}
		default:
			return fmt.Errorf("retentionScriptRows: unsupported scan dest %T", d)
		}
	}
	return nil
}

func (r *retentionScriptRows) Err() error { return nil }
func (r *retentionScriptRows) Close()     {}

// retentionScriptPool is a readPool fake returning a scripted result set keyed by
// the exact SQL text, recording each query's args, so a test proves the reader
// issues the right statements with the right parameters.
type retentionScriptPool struct {
	bySQL map[string][][]any
	args  map[string][]any
}

func (p *retentionScriptPool) query(_ context.Context, sql string, args ...any) (poolRows, error) {
	if p.args == nil {
		p.args = map[string][]any{}
	}
	p.args[sql] = args
	rows, ok := p.bySQL[sql]
	if !ok {
		return nil, fmt.Errorf("retentionScriptPool: no script for SQL %q", sql)
	}
	return &retentionScriptRows{rows: rows}, nil
}

func TestPgxRetentionReaderReads(t *testing.T) {
	t.Run("pgx-retention-reader", func(t *testing.T) {
		t.Run("the census read returns every run's (id, pipeline)", func(t *testing.T) {
			pool := &retentionScriptPool{bySQL: map[string][][]any{
				retentionRunsSQL: {
					{int64(1), "extract"},
					{int64(2), "load"},
				},
			}}
			got, err := newPgxRetentionReader(pool).RetentionRuns(context.Background())
			if err != nil {
				t.Fatalf("RetentionRuns: %v", err)
			}
			want := []RetentionRunRef{{RunID: 1, Pipeline: "extract"}, {RunID: 2, Pipeline: "load"}}
			if !reflect.DeepEqual(got, want) {
				t.Errorf("RetentionRuns = %v, want %v", got, want)
			}
		})

		t.Run("the held read returns the outstanding dead-letter run ids", func(t *testing.T) {
			pool := &retentionScriptPool{bySQL: map[string][][]any{
				outstandingDeadLetterRunsSQL: {{int64(3)}, {int64(7)}},
			}}
			got, err := newPgxRetentionReader(pool).OutstandingDeadLetterRuns(context.Background())
			if err != nil {
				t.Fatalf("OutstandingDeadLetterRuns: %v", err)
			}
			if want := []int64{3, 7}; !reflect.DeepEqual(got, want) {
				t.Errorf("OutstandingDeadLetterRuns = %v, want %v", got, want)
			}
		})

		t.Run("the archival read passes the ids through and stitches consumed upstreams", func(t *testing.T) {
			hash := "beef"
			pool := &retentionScriptPool{bySQL: map[string][][]any{
				prunableRunsSQL: {
					// id, pipeline, state, artifact_hash, declaration_checksum,
					// snapshot_lsn, journal_floor, journal_ceiling
					{int64(1), "load", "succeeded", hash, "sum1", "0/1A", int64(5), int64(9)},
					{int64(2), "load", "dead_lettered", nil, "sum2", nil, nil, nil},
				},
				prunableRunInputsSQL: {
					{int64(1), int64(40)},
					{int64(1), int64(41)},
				},
			}}
			got, err := newPgxRetentionReader(pool).PrunableRunsByID(context.Background(), []int64{1, 2})
			if err != nil {
				t.Fatalf("PrunableRunsByID: %v", err)
			}
			lsn := "0/1A"
			floor, ceil := int64(5), int64(9)
			want := []PrunableRun{
				{RunID: 1, Pipeline: "load", State: RunSucceeded, ArtifactHash: &hash,
					DeclarationChecksum: "sum1", ConsumedUpstreamRunIDs: []int64{40, 41},
					SnapshotLSN: &lsn, JournalFloor: &floor, JournalCeiling: &ceil},
				{RunID: 2, Pipeline: "load", State: RunDeadLettered, DeclarationChecksum: "sum2"},
			}
			if !reflect.DeepEqual(got, want) {
				t.Errorf("PrunableRunsByID =\n %+v, want\n %+v", got, want)
			}
			// Both statements ride the selected ids as their one parameter.
			for _, sql := range []string{prunableRunsSQL, prunableRunInputsSQL} {
				args := pool.args[sql]
				if len(args) != 1 || !reflect.DeepEqual(args[0], []int64{1, 2}) {
					t.Errorf("query %q args = %v, want [[1 2]]", sql, args)
				}
			}
		})

		t.Run("the pipeline archival read returns every remaining run, any state", func(t *testing.T) {
			pool := &retentionScriptPool{bySQL: map[string][][]any{
				prunablePipelineRunsSQL: {
					{int64(1), "load", "succeeded", nil, "sum1", nil, nil, nil},
					{int64(2), "load", "dead_lettered", nil, "sum2", nil, nil, nil},
				},
				pipelineRunInputsSQL: {
					{int64(2), int64(1)},
				},
			}}
			got, err := newPgxRetentionReader(pool).PrunablePipelineRuns(context.Background(), "load")
			if err != nil {
				t.Fatalf("PrunablePipelineRuns: %v", err)
			}
			want := []PrunableRun{
				{RunID: 1, Pipeline: "load", State: RunSucceeded, DeclarationChecksum: "sum1"},
				{RunID: 2, Pipeline: "load", State: RunDeadLettered, DeclarationChecksum: "sum2", ConsumedUpstreamRunIDs: []int64{1}},
			}
			if !reflect.DeepEqual(got, want) {
				t.Errorf("PrunablePipelineRuns =\n %+v, want\n %+v", got, want)
			}
			for _, sql := range []string{prunablePipelineRunsSQL, pipelineRunInputsSQL} {
				args := pool.args[sql]
				if len(args) != 1 || args[0] != "load" {
					t.Errorf("query %q args = %v, want [load]", sql, args)
				}
			}
		})

		t.Run("the artifact-hash census returns the pipeline's index rows", func(t *testing.T) {
			pool := &retentionScriptPool{bySQL: map[string][][]any{
				artifactHashesSQL: {{"aaa"}, {"bbb"}},
			}}
			got, err := newPgxRetentionReader(pool).ArtifactHashes(context.Background(), "load")
			if err != nil {
				t.Fatalf("ArtifactHashes: %v", err)
			}
			if want := []string{"aaa", "bbb"}; !reflect.DeepEqual(got, want) {
				t.Errorf("ArtifactHashes = %v, want %v", got, want)
			}
		})

		t.Run("no ids reads nothing", func(t *testing.T) {
			pool := &retentionScriptPool{bySQL: map[string][][]any{}}
			got, err := newPgxRetentionReader(pool).PrunableRunsByID(context.Background(), nil)
			if err != nil {
				t.Fatalf("PrunableRunsByID(nil): %v", err)
			}
			if got != nil {
				t.Errorf("PrunableRunsByID(nil) = %v, want nil", got)
			}
			if len(pool.args) != 0 {
				t.Errorf("queries issued for an empty id set: %v", pool.args)
			}
		})
	})
}
