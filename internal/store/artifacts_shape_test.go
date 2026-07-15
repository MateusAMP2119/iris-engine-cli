package store_test

import (
	"reflect"
	"testing"

	"github.com/MateusAMP2119/iris-lakehouse/internal/store"
)

// TestArtifactsRowIsIndex proves the artifacts table's shape contract:
// content-addressed built binaries indexed by hash. The DDL declares hash as the
// text primary key, a pipeline foreign key to pipelines(name), size_bytes, and
// recorded_at -- and nothing else. A row is an index entry: the binary's bytes
// live only in the object store under the hash, never as a blob column in
// Postgres, so the table carries no bytea (or any payload-capable) column at all.
// The DDL itself lives in the bootstrap meta schema (MetaSchema, schema.go); this
// test locks its shape as the contract the build path and artifact retirement
// depend on.
func TestArtifactsRowIsIndex(t *testing.T) {
	s := store.MetaSchema()
	a := tableByName(t, s, "artifacts")

	// Content addressing: hash is the whole primary key, a non-nullable text
	// column (the binary's content hash, the object-store key).
	if !reflect.DeepEqual(a.PrimaryKey, []string{"hash"}) {
		t.Errorf("artifacts primary key = %v, want [hash] (content-addressed rows)", a.PrimaryKey)
	}
	hash := columnByName(t, a, "hash")
	if hash.Type != "text" {
		t.Errorf("artifacts.hash type = %q, want text", hash.Type)
	}
	if hash.Nullable {
		t.Error("artifacts.hash is nullable; the primary key must be NOT NULL")
	}

	// pipeline is a text FK to pipelines(name): every artifact belongs to a
	// registered pipeline.
	pipeline := columnByName(t, a, "pipeline")
	if pipeline.Type != "text" {
		t.Errorf("artifacts.pipeline type = %q, want text", pipeline.Type)
	}
	var pipelineFK bool
	for _, fk := range a.ForeignKeys {
		if fk.Column == "pipeline" && fk.RefTable == "pipelines" && fk.RefColumn == "name" {
			pipelineFK = true
		}
	}
	if !pipelineFK {
		t.Errorf("artifacts.pipeline has no foreign key to pipelines(name); FKs = %v", a.ForeignKeys)
	}

	// size_bytes and recorded_at complete the index entry.
	if sb := columnByName(t, a, "size_bytes"); sb.Type != "bigint" {
		t.Errorf("artifacts.size_bytes type = %q, want bigint", sb.Type)
	}
	if ra := columnByName(t, a, "recorded_at"); ra.Type != "text" {
		t.Errorf("artifacts.recorded_at type = %q, want text", ra.Type)
	}

	// Row = index, never payload: exactly the four index columns, and no column
	// that could hold the binary's bytes (no bytea blob in Postgres -- bytes live
	// only in the object store under the hash).
	wantCols := map[string]bool{"hash": true, "pipeline": true, "size_bytes": true, "recorded_at": true}
	if len(a.Columns) != len(wantCols) {
		t.Errorf("artifacts has %d columns, want exactly %d (index entry only)", len(a.Columns), len(wantCols))
	}
	for _, c := range a.Columns {
		if !wantCols[c.Name] {
			t.Errorf("artifacts carries unexpected column %q; a row is an index entry, never payload", c.Name)
		}
		if c.Type == "bytea" {
			t.Errorf("artifacts.%s is bytea; binary bytes live only in the object store, never as blobs in Postgres", c.Name)
		}
	}
}
