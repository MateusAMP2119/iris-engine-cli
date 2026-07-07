package store

// This file is the local object store (specification sections 4, 9, and the
// Naming block): the one home for heavy immutable payloads, a content-addressed
// directory of plain files at objects_path (default .iris/objects/), keyed by
// hash -- no daemon, no cloud client. The build path stores each produced
// binary's bytes here under the binary's SHA-256 content hash; meta's artifacts
// table holds only the hash and metadata (row = index, never payload), so no
// binary blob ever lands in Postgres. Objects are immutable: written once,
// deleted only by artifact retirement or engine uninstall. Sealed journal
// partitions later share this home (E11), keyed by checkpoint digest.
//
// Ingestion is atomic: bytes stream into a temp file in the store root while the
// hash is computed, then one rename publishes the object under its final name.
// A crash mid-ingest leaves only a temp file, never a half-written object under
// a valid hash; storing bytes that already exist is a no-op (write-once), so
// re-building identical source never rewrites -- or even touches -- the
// existing object.

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"os"
	"path/filepath"
)

// ObjectStore is the content-addressed local object store rooted at
// objects_path. The zero value is not usable; construct one with
// NewObjectStore. It performs plain file I/O only -- it is not a database
// client and holds no connection.
type ObjectStore struct {
	root string
}

// NewObjectStore returns an object store rooted at root (the resolved
// objects_path). The root is created lazily on the first Put, so constructing a
// store is free and side-effect-less.
func NewObjectStore(root string) *ObjectStore {
	return &ObjectStore{root: root}
}

// Path returns the file path an object with the given content hash lives at:
// <root>/<hash>, one plain file per object.
func (s *ObjectStore) Path(hash string) string {
	return filepath.Join(s.root, hash)
}

// Put stores the bytes read from r as one immutable content-addressed object,
// returning the object's SHA-256 hex content hash and byte size. The bytes
// stream through the hasher into a temp file, so the payload is never held in
// memory; the object is published by a single rename, so a reader never sees a
// partial object under a valid hash. An object that already exists is left
// untouched (write-once): identical bytes are already correctly stored, so the
// ingest is discarded and the existing hash returned.
func (s *ObjectStore) Put(r io.Reader) (hash string, size int64, err error) {
	if err := os.MkdirAll(s.root, 0o750); err != nil {
		return "", 0, fmt.Errorf("store: object store: create root %s: %w", s.root, err)
	}
	tmp, err := os.CreateTemp(s.root, ".ingest-*")
	if err != nil {
		return "", 0, fmt.Errorf("store: object store: create ingest file: %w", err)
	}
	tmpName := tmp.Name()
	// The temp file is removed on every path but the publishing rename, so a
	// failed or redundant ingest leaves no stray file in the store.
	defer func() { _ = os.Remove(tmpName) }()

	h := sha256.New()
	size, err = io.Copy(io.MultiWriter(tmp, h), r)
	if err != nil {
		_ = tmp.Close()
		return "", 0, fmt.Errorf("store: object store: ingest bytes: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return "", 0, fmt.Errorf("store: object store: close ingest file: %w", err)
	}

	hash = hex.EncodeToString(h.Sum(nil))
	final := s.Path(hash)

	// Write-once: an existing object under this hash already holds these exact
	// bytes (content addressing), so it is never rewritten or touched.
	if _, err := os.Lstat(final); err == nil {
		return hash, size, nil
	} else if !os.IsNotExist(err) {
		return "", 0, fmt.Errorf("store: object store: probe object %s: %w", final, err)
	}

	// Objects are immutable executables (artifact bytes): read/execute, no write.
	if err := os.Chmod(tmpName, 0o555); err != nil {
		return "", 0, fmt.Errorf("store: object store: set object mode: %w", err)
	}
	if err := os.Rename(tmpName, final); err != nil {
		return "", 0, fmt.Errorf("store: object store: publish object %s: %w", hash, err)
	}
	return hash, size, nil
}
