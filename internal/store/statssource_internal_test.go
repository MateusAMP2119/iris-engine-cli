package store

import (
	"context"
	"fmt"
	"testing"
)

// scriptRows is a poolRows fake over a fixed result set: each row is a slice of
// column values, delivered in order, assigned into the caller's scan destinations
// by concrete type. It covers only the column types the stats source scans.
type scriptRows struct {
	rows   [][]any
	cursor int
	closed bool
}

func (r *scriptRows) Next() bool {
	if r.cursor >= len(r.rows) {
		return false
	}
	r.cursor++
	return true
}

func (r *scriptRows) Scan(dest ...any) error {
	row := r.rows[r.cursor-1]
	if len(dest) != len(row) {
		return fmt.Errorf("scriptRows: scan into %d dests, row has %d columns", len(dest), len(row))
	}
	for i, d := range dest {
		switch p := d.(type) {
		case *int64:
			*p = row[i].(int64)
		case *string:
			*p = row[i].(string)
		case *[]byte:
			if row[i] == nil {
				*p = nil
			} else {
				*p = row[i].([]byte)
			}
		case **int:
			if row[i] == nil {
				*p = nil
			} else {
				v := row[i].(int)
				*p = &v
			}
		default:
			return fmt.Errorf("scriptRows: unsupported scan dest %T", d)
		}
	}
	return nil
}

func (r *scriptRows) Err() error { return nil }
func (r *scriptRows) Close()     { r.closed = true }

// scriptPool is a readPool fake that returns a scripted result set keyed by the
// exact SQL text, so a test can prove the meta-backed stats source issues the
// right query and maps its rows into the rollup.
type scriptPool struct {
	bySQL map[string][][]any
}

func (p *scriptPool) query(_ context.Context, sql string, _ ...any) (poolRows, error) {
	rows, ok := p.bySQL[sql]
	if !ok {
		return nil, fmt.Errorf("scriptPool: no script for SQL %q", sql)
	}
	return &scriptRows{rows: rows}, nil
}

// TestPgxStatsSourceRollup proves the meta-backed StatsSource reads each control
// table and feeds BuildStats a rollup identical in shape to the fake-fed one: the
// engine dead-letter depth and per-reason counts, running runs, and checkpoint
// chain head come straight off the meta snapshot; the per-lane membership and
// per-pipeline last-values follow. It exercises the production source (pgxStatsSource)
// over a scripted pool, no live Postgres.
//
// spec: S11/stats-engine-rollup
// spec: S11/stats-lane-rollup
// spec: S11/stats-pipeline-rollup
func TestPgxStatsSourceRollup(t *testing.T) {
	pool := &scriptPool{bySQL: map[string][][]any{
		statsRunsSQL: {
			{int64(1), "extract_orders", "succeeded", 0},
			{int64(2), "load_orders", "dead_lettered", 7},
			{int64(3), "load_orders", "running", nil},
		},
		statsDeadLettersSQL: {
			{int64(2), "failed"},
		},
		statsLaneMembersSQL: {
			{"ingest", "extract_orders"},
			{"ingest", "load_orders"},
		},
		statsPipelinesSQL: {
			{"extract_orders"},
			{"load_orders"},
		},
		statsCheckpointsSQL: {
			{int64(1), []byte{0xab, 0xcd}, "resident"},
		},
	}}
	src := newPgxStatsSource(pool)

	rollup, err := BuildStats(context.Background(), src, map[string]int64{"ingest": 5})
	if err != nil {
		t.Fatalf("BuildStats over pgxStatsSource: %v", err)
	}

	// spec: S11/stats-engine-rollup
	t.Run("S11/stats-engine-rollup", func(t *testing.T) {
		if rollup.Engine.DeadLetterDepth != 1 {
			t.Errorf("dead-letter depth = %d, want 1", rollup.Engine.DeadLetterDepth)
		}
		if rollup.Engine.DeadLettersByReason["failed"] != 1 {
			t.Errorf("dead-letters by reason[failed] = %d, want 1", rollup.Engine.DeadLettersByReason["failed"])
		}
		if rollup.Engine.RunningRuns != 1 {
			t.Errorf("running runs = %d, want 1", rollup.Engine.RunningRuns)
		}
		if rollup.Engine.SealedPartitions != 1 {
			t.Errorf("sealed partitions = %d, want 1", rollup.Engine.SealedPartitions)
		}
		if rollup.Engine.ChainHead == nil || rollup.Engine.ChainHead.Digest != "abcd" {
			t.Errorf("chain head = %+v, want digest abcd", rollup.Engine.ChainHead)
		}
		// The journal counters are honestly zero until E07's readers land.
		if rollup.Engine.CapturedWrites != 0 || rollup.Engine.JournalRows != 0 {
			t.Errorf("journal counters = %d/%d, want 0/0 (no journal reader yet)", rollup.Engine.CapturedWrites, rollup.Engine.JournalRows)
		}
	})

	// spec: S11/stats-lane-rollup
	t.Run("S11/stats-lane-rollup", func(t *testing.T) {
		if len(rollup.Lanes) != 1 {
			t.Fatalf("lanes = %d, want 1", len(rollup.Lanes))
		}
		lane := rollup.Lanes[0]
		if lane.Lane != "ingest" || lane.Pipelines != 2 {
			t.Errorf("lane = %+v, want ingest with 2 pipelines", lane)
		}
		if lane.Running != 1 {
			t.Errorf("lane running = %d, want 1", lane.Running)
		}
		if lane.Passes != 5 {
			t.Errorf("lane passes = %d, want the leader-held 5", lane.Passes)
		}
	})

	// spec: S11/stats-pipeline-rollup
	t.Run("S11/stats-pipeline-rollup", func(t *testing.T) {
		byName := map[string]PipelineRollup{}
		for _, p := range rollup.Pipelines {
			byName[p.Pipeline] = p
		}
		load := byName["load_orders"]
		if load.LatestRunState != "running" || load.LastRunID != "3" {
			t.Errorf("load_orders latest = %q/%q, want running/3", load.LatestRunState, load.LastRunID)
		}
		if load.LastExitCode == nil || *load.LastExitCode != 7 {
			t.Errorf("load_orders last exit = %v, want 7 (from run 2, the latest carrying one)", load.LastExitCode)
		}
		extract := byName["extract_orders"]
		if extract.LatestRunState != "succeeded" {
			t.Errorf("extract_orders latest = %q, want succeeded", extract.LatestRunState)
		}
	})
}
