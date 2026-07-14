package daemon

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"github.com/MateusAMP2119/iris-engine-cli/internal/archive"
	"github.com/MateusAMP2119/iris-engine-cli/internal/pg"
	"github.com/MateusAMP2119/iris-engine-cli/internal/store"
	"github.com/MateusAMP2119/iris-engine-cli/internal/store/storetest"
)

// This file proves the provenance plane's archived-stamp fallback: a row whose
// stamps were sealed, exported, and dropped from the resident journal is still
// answered -- recovered from the exported partition under the archived
// checkpoint's digest -- instead of reading as "no provenance recorded".

// emptyStamps is a stampsReader over an empty resident journal (the sealed rows
// were dropped).
type emptyStamps struct{}

func (emptyStamps) Stamps(context.Context, pg.RowKey) ([]pg.JournalEntry, error) { return nil, nil }

// liveStamps is a stampsReader with resident entries for every key.
type liveStamps struct{ entries []pg.JournalEntry }

func (s liveStamps) Stamps(context.Context, pg.RowKey) ([]pg.JournalEntry, error) {
	return s.entries, nil
}

// chainFake is a store.CheckpointChainReader over fixed rows, counting reads.
type chainFake struct {
	rows  []store.CheckpointRow
	reads int
}

func (c *chainFake) ArchivedCheckpoints(context.Context) ([]store.CheckpointRow, error) {
	c.reads++
	return c.rows, nil
}

// compactedRow renders one canonical compacted-row serialization, the exact
// shape pg.Client.QueryCompactedRows produces and the archive persists.
func compactedRow(id int64, runID int64, schema, table, pk, op, pre, undo string) []byte {
	return []byte(fmt.Sprintf("%d|iris_admin|%d|%s|%s|%s|%s|%s|%s|2026-07-14T00:00:00Z", id, runID, schema, table, pk, op, pre, undo))
}

// seedLineageRun records one run in the fake lineage so the walk resolves facts.
func seedLineageRun(meta *storetest.Fake, runID int64, pipeline string) {
	var lin store.ProvenanceLineage
	lin.Runs = append(lin.Runs, struct {
		RunID               int64
		Pipeline            string
		State               string
		ArtifactHash        *string
		DeclarationChecksum string
		SnapshotLSN         *string
		JournalFloor        *int64
		JournalCeiling      *int64
	}{RunID: runID, Pipeline: pipeline, State: "succeeded", DeclarationChecksum: "sum"})
	meta.SetProvenanceLineage(lin)
}

func TestProvenanceArchivedFallback(t *testing.T) {
	t.Run("provenance-archived-fallback", func(t *testing.T) {
		ctx := context.Background()

		t.Run("dropped stamps resolve from the archived partition", func(t *testing.T) {
			// The exported partition holds the key's two stamps plus a foreign row.
			digest := []byte{0xbe, 0xef, 0x01}
			objects := store.NewObjectStore(t.TempDir())
			rows := [][]byte{
				compactedRow(11, 7, "analytics", "orders", "200", "insert", "", "promoted"),
				compactedRow(12, 7, "analytics", "orders", "200", "update", `{"amount": 1, "note": "a|b"}`, "promoted"),
				compactedRow(13, 7, "analytics", "orders", "201", "insert", "", "promoted"),
			}
			hdr := archive.Header{IDFrom: 11, IDTo: 13, Digest: digest, Signature: []byte("sig")}
			if err := archive.Write(objects.Path(fmt.Sprintf("%x", digest)), hdr, rows); err != nil {
				t.Fatalf("write archive: %v", err)
			}

			meta := storetest.New()
			seedLineageRun(meta, 7, "load_orders")
			chain := &chainFake{rows: []store.CheckpointRow{{Seq: 1, IDFrom: 11, IDTo: 13, Digest: digest, Location: "archived"}}}
			plane := NewProvenancePlane(meta, emptyStamps{}, objects, chain, nil)

			res, err := plane.Provenance(ctx, "analytics", "orders", "200")
			if err != nil {
				t.Fatalf("Provenance over archived stamps: %v", err)
			}
			if len(res.Stamps) != 2 {
				t.Fatalf("recovered %d stamps, want 2 (the key's own; the foreign row is filtered)", len(res.Stamps))
			}
			// The layer stack renders newest-first, exactly as resident stamps do.
			if res.Stamps[0].EntryID != 12 || res.Stamps[1].EntryID != 11 {
				t.Errorf("recovered stamp ids = %d, %d, want 12, 11 (newest-first)", res.Stamps[0].EntryID, res.Stamps[1].EntryID)
			}
			if !res.Authored || res.Author == nil || res.Author.RunID != 7 {
				t.Errorf("author not resolved from archived stamps: authored=%v author=%+v", res.Authored, res.Author)
			}
			if res.Pipeline != "load_orders" {
				t.Errorf("facts pipeline = %q, want load_orders (lineage walk over recovered stamps)", res.Pipeline)
			}
		})

		t.Run("a checkpoint whose archive is missing fails loudly", func(t *testing.T) {
			objects := store.NewObjectStore(t.TempDir())
			meta := storetest.New()
			chain := &chainFake{rows: []store.CheckpointRow{{Seq: 3, Digest: []byte{0x01}, Location: "archived"}}}
			plane := NewProvenancePlane(meta, emptyStamps{}, objects, chain, nil)

			_, err := plane.Provenance(ctx, "analytics", "orders", "200")
			if err == nil || !strings.Contains(err.Error(), "archived") {
				t.Errorf("missing archive returned %v, want a loud archived-partition read failure (never a silent \"no provenance\")", err)
			}
		})

		t.Run("resident stamps never touch the archives", func(t *testing.T) {
			meta := storetest.New()
			seedLineageRun(meta, 7, "load_orders")
			live := liveStamps{entries: []pg.JournalEntry{
				{ID: 5, RunID: 7, Schema: "analytics", Table: "orders", RowPK: "200", Op: pg.WriteOp("insert"), Undo: pg.UndoOpen},
			}}
			chain := &chainFake{}
			plane := NewProvenancePlane(meta, live, store.NewObjectStore(t.TempDir()), chain, nil)

			if _, err := plane.Provenance(ctx, "analytics", "orders", "200"); err != nil {
				t.Fatalf("Provenance over resident stamps: %v", err)
			}
			if chain.reads != 0 {
				t.Errorf("resident answer read the checkpoint chain %d times, want 0 (the archive scan is the rare path)", chain.reads)
			}
		})

		t.Run("an unwired fallback keeps the no-provenance answer", func(t *testing.T) {
			plane := NewProvenancePlane(storetest.New(), emptyStamps{}, nil, nil, nil)
			_, err := plane.Provenance(ctx, "analytics", "orders", "200")
			if err == nil || !strings.Contains(err.Error(), "no provenance recorded") {
				t.Errorf("unwired fallback returned %v, want the no-provenance refusal", err)
			}
		})
	})
}
