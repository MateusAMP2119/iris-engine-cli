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
// Ingestion is atomic and durable: bytes stream into a temp file in the store
// root while the hash is computed, the temp file is fsynced, then one rename
// publishes the object under its final name and the store directory is fsynced.
// A crash mid-ingest leaves only a temp file, never a half-written object under a
// valid hash; and once published, both the object's bytes and its directory entry
// are on stable storage, so a power loss cannot leave meta naming a hash whose
// object file is empty or partial. Storing bytes that already exist is a no-op
// (write-once), so re-building identical source never rewrites -- or even touches
// -- the existing object.

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
// memory; the temp file is fsynced and the object published by a single rename
// (with the store directory then fsynced), so neither a reader nor a crash ever
// sees a partial object under a valid hash. An object that already exists is left
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
	// fsync the bytes to stable storage BEFORE the publishing rename, so the object
	// under its hash can never be empty or partial after a crash while meta already
	// names the hash. Sync failure aborts the ingest rather than publishing durably-
	// unwritten bytes.
	if err := tmp.Sync(); err != nil {
		_ = tmp.Close()
		return "", 0, fmt.Errorf("store: object store: flush ingest file: %w", err)
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
	// fsync the directory so the new entry survives a crash: without it the renamed
	// name can be lost even though the file's bytes are durable.
	if err := syncDir(s.root); err != nil {
		return "", 0, fmt.Errorf("store: object store: flush store dir: %w", err)
	}
	return hash, size, nil
}

// syncDir fsyncs a directory so a rename into it is durable across a crash: the
// published object name survives even if the OS has not yet flushed the directory
// entry. A close error after a successful sync is still reported.
func syncDir(dir string) error {
	d, err := os.Open(dir) //nolint:gosec // G304: the object-store root is engine-owned.
	if err != nil {
		return err
	}
	serr := d.Sync()
	cerr := d.Close()
	if serr != nil {
		return serr
	}
	return cerr
}
