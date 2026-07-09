package store

import (
	"context"
	"fmt"
)

// This file is the artifacts write surface: the leader-only path that records a
// built binary's index row (specification sections 4 and 9). The artifacts table
// holds content-addressed index entries -- hash PK, pipeline FK, size_bytes,
// recorded_at -- while the binary's bytes live only in the object store under the
// hash (objects.go), never as blobs in Postgres. Rows are immutable: the surface
// is insert-only, so a rebuild inserts a new row under its new hash and the
// pipeline's current artifact is simply its newest row. Rows leave only through
// artifact retirement (post-prune, a later E08 task) or teardown, never through
// an update.

// ArtifactRow is the input to InsertArtifact: one immutable artifacts index
// entry for a successfully built binary. recorded_at is stamped database-side
// (now()::text, an opaque audit string), never carried here.
type ArtifactRow struct {
	// Hash is the built binary's SHA-256 hex content hash (artifacts.hash), the
	// object-store key its bytes live under.
	Hash string
	// Pipeline is the registered pipeline the binary was built for
	// (artifacts.pipeline).
	Pipeline string
	// SizeBytes is the binary's size in bytes (artifacts.size_bytes).
	SizeBytes int64
}

// insertArtifactSQL records one artifact index entry. It is a plain INSERT --
// no upsert, no conflict clause -- because rows are immutable: a rebuild of
// changed source arrives under a new hash and inserts a new row, and a rebuild
// of identical source arrives under an existing hash, which the primary key
// rejects rather than silently rewriting history. recorded_at is filled
// database-side so no clock is read in the engine.
const insertArtifactSQL = `INSERT INTO artifacts (hash, pipeline, size_bytes, recorded_at)
VALUES ($1, $2, $3, now()::text)`

// InsertArtifact records a successfully built binary's index row: its content
// hash, owning pipeline, and byte size (specification section 9: building
// records the hash in artifacts, the bytes in the object store). It rides the
// single Writer like every meta write, and it only ever inserts -- artifact
// rows are immutable, so there is deliberately no update counterpart.
func (w *Writer) InsertArtifact(ctx context.Context, row ArtifactRow) error {
	if err := w.conn.Exec(ctx, insertArtifactSQL, row.Hash, row.Pipeline, row.SizeBytes); err != nil {
		return fmt.Errorf("store: insert artifact %s for pipeline %q: %w", row.Hash, row.Pipeline, err)
	}
	return nil
}
