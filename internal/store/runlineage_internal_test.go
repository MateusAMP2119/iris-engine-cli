package store

import (
	"context"
	"fmt"
	"reflect"
	"testing"
)

// This file proves the meta-backed run-lineage reader over a scripted pool (no live
// Postgres): the whole-history read orders newest first and maps each row's consumed
// upstream array and nullable replayed_from, and the single-run read reports absence
// as (_, false, nil) rather than an error. The production array/NULL scanning against
// a real Postgres is exercised at conformance tier.

// lineageScriptRows is a poolRows fake over a fixed result set for the run-lineage
// reader's scan shape: id (int64), pipeline and state (string), the nullable
// replayed_from (*int64), and the consumed-upstream array ([]int64).
type lineageScriptRows struct {
	rows   [][]any
	cursor int
	closed bool
}

func (r *lineageScriptRows) Next() bool {
	if r.cursor >= len(r.rows) {
		return false
	}
	r.cursor++
	return true
}

func (r *lineageScriptRows) Scan(dest ...any) error {
	row := r.rows[r.cursor-1]
	if len(dest) != len(row) {
		return fmt.Errorf("lineageScriptRows: scan into %d dests, row has %d columns", len(dest), len(row))
	}
	for i, d := range dest {
		switch p := d.(type) {
		case *int64:
			*p = row[i].(int64)
		case *string:
			*p = row[i].(string)
		case **int64:
			if row[i] == nil {
				*p = nil
			} else {
				v := row[i].(int64)
				*p = &v
			}
		case *[]int64:
			if row[i] == nil {
				*p = nil
			} else {
				*p = append([]int64(nil), row[i].([]int64)...)
			}
		default:
			return fmt.Errorf("lineageScriptRows: unsupported scan dest %T", d)
		}
	}
	return nil
}

func (r *lineageScriptRows) Err() error { return nil }
func (r *lineageScriptRows) Close()     { r.closed = true }

// lineageScriptPool is a readPool fake returning a scripted result set keyed by the
// exact SQL text, so a test proves the reader issues the right query.
type lineageScriptPool struct {
	bySQL    map[string][][]any
	lastArgs []any
}

func (p *lineageScriptPool) query(_ context.Context, sql string, args ...any) (poolRows, error) {
	p.lastArgs = args
	rows, ok := p.bySQL[sql]
	if !ok {
		return nil, fmt.Errorf("lineageScriptPool: no script for SQL %q", sql)
	}
	return &lineageScriptRows{rows: rows}, nil
}

// spec: S07/runs-include-inputs
func TestPgxRunLineagesNewestFirstWithInputs(t *testing.T) {
	rf := int64(1)
	pool := &lineageScriptPool{bySQL: map[string][][]any{
		// The reader asks newest-first; the script returns rows already in that order,
		// as the ORDER BY id DESC query would.
		runLineagesSQL: {
			{int64(42), "load", "succeeded", rf, []int64{39, 40}},
			{int64(2), "extract", "succeeded", nil, []int64{}},
		},
	}}
	got, err := newPgxRunLineageReader(pool).RunLineages(context.Background())
	if err != nil {
		t.Fatalf("RunLineages: %v", err)
	}
	want := []RunLineage{
		{ID: 42, Pipeline: "load", State: RunSucceeded, ReplayedFrom: &rf, Inputs: []int64{39, 40}},
		{ID: 2, Pipeline: "extract", State: RunSucceeded, ReplayedFrom: nil, Inputs: nil},
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("RunLineages = %+v, want %+v", got, want)
	}
}

// spec: S07/runs-include-inputs
func TestPgxRunLineageByID(t *testing.T) {
	pool := &lineageScriptPool{bySQL: map[string][][]any{
		runLineageByIDSQL: {
			{int64(7), "transform", "running", nil, []int64{5}},
		},
	}}
	r := newPgxRunLineageReader(pool)

	rl, found, err := r.RunLineage(context.Background(), 7)
	if err != nil || !found {
		t.Fatalf("RunLineage(7) = (%+v, %v, %v), want found", rl, found, err)
	}
	if rl.ID != 7 || rl.Pipeline != "transform" || rl.State != RunRunning {
		t.Errorf("RunLineage(7) = %+v, want run 7 transform running", rl)
	}
	if !reflect.DeepEqual(rl.Inputs, []int64{5}) {
		t.Errorf("RunLineage(7).Inputs = %v, want [5]", rl.Inputs)
	}
	if len(pool.lastArgs) != 1 || pool.lastArgs[0].(int64) != 7 {
		t.Errorf("RunLineage bound args = %v, want the id 7", pool.lastArgs)
	}
}

// spec: S07/runs-include-inputs
func TestPgxRunLineageAbsent(t *testing.T) {
	pool := &lineageScriptPool{bySQL: map[string][][]any{
		runLineageByIDSQL: {}, // no rows: the id resolves to no run
	}}
	rl, found, err := newPgxRunLineageReader(pool).RunLineage(context.Background(), 999)
	if err != nil {
		t.Fatalf("RunLineage(absent): unexpected error %v", err)
	}
	if found {
		t.Errorf("RunLineage(absent) found = true, want false (no run 999); got %+v", rl)
	}
}
