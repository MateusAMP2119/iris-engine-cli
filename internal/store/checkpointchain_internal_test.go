package store

import (
	"context"
	"reflect"
	"testing"
)

// This file proves the archived-checkpoint chain read over a scripted pool (no
// live Postgres): the reader issues the archived-only statement and maps each
// row, so the provenance plane's read-back fallback sees exactly the chain rows
// whose partitions were exported and dropped.

func TestPgxCheckpointChainReader(t *testing.T) {
	t.Run("pgx-checkpoint-chain-reader", func(t *testing.T) {
		pool := &retentionScriptPool{bySQL: map[string][][]any{
			selectArchivedCheckpointsSQL: {
				{int64(1), int64(1), int64(3), "d1", "", "s1", "archived", "2026-07-14T00:00:00Z"},
				{int64(2), int64(4), int64(9), "d2", "d1", "s2", "archived", "2026-07-14T01:00:00Z"},
			},
		}}
		got, err := newPgxCheckpointChainReader(pool).ArchivedCheckpoints(context.Background())
		if err != nil {
			t.Fatalf("ArchivedCheckpoints: %v", err)
		}
		want := []CheckpointRow{
			{Seq: 1, IDFrom: 1, IDTo: 3, Digest: []byte("d1"), ParentDigest: []byte(""), Signature: []byte("s1"), Location: "archived", RecordedAt: "2026-07-14T00:00:00Z"},
			{Seq: 2, IDFrom: 4, IDTo: 9, Digest: []byte("d2"), ParentDigest: []byte("d1"), Signature: []byte("s2"), Location: "archived", RecordedAt: "2026-07-14T01:00:00Z"},
		}
		if !reflect.DeepEqual(got, want) {
			t.Errorf("ArchivedCheckpoints =\n %+v, want\n %+v", got, want)
		}
	})
}
