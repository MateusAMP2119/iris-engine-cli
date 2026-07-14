package store_test

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"os"
	"path/filepath"
	"testing"

	"github.com/MateusAMP2119/iris-engine-cli/internal/store"
)

// TestObjectStoreContentAddressed proves the object-store half of the build
// contract: the object store at objects_path holds content-addressed artifact
// bytes. A successful build stores the produced binary's bytes as one plain file
// under the binary's content hash: Put returns
// the SHA-256 hex hash of exactly the bytes written, the bytes land at
// <root>/<hash> byte-for-byte, objects are write-once (re-storing identical bytes
// is idempotent, never a rewrite), and distinct bytes land under distinct hashes
// with earlier objects untouched.
func TestObjectStoreContentAddressed(t *testing.T) {
	root := filepath.Join(t.TempDir(), "objects")
	s := store.NewObjectStore(root)

	payload := []byte("#!ELF fake built binary v1")
	wantSum := sha256.Sum256(payload)
	wantHash := hex.EncodeToString(wantSum[:])

	hash, size, err := s.Put(bytes.NewReader(payload))
	if err != nil {
		t.Fatalf("Put: %v", err)
	}
	if hash != wantHash {
		t.Errorf("Put hash = %q, want the SHA-256 hex of the bytes %q", hash, wantHash)
	}
	if size != int64(len(payload)) {
		t.Errorf("Put size = %d, want %d", size, len(payload))
	}

	// The bytes live as one plain file keyed by the hash.
	got, err := os.ReadFile(s.Path(hash))
	if err != nil {
		t.Fatalf("read stored object at %s: %v", s.Path(hash), err)
	}
	if !bytes.Equal(got, payload) {
		t.Errorf("stored object bytes = %q, want %q (byte-for-byte)", got, payload)
	}

	// Write-once: re-storing identical bytes is idempotent -- same hash, object
	// still present, still identical.
	again, _, err := s.Put(bytes.NewReader(payload))
	if err != nil {
		t.Fatalf("re-Put identical bytes: %v", err)
	}
	if again != hash {
		t.Errorf("re-Put hash = %q, want %q (content addressing is deterministic)", again, hash)
	}

	// Distinct bytes: a second object under its own hash, the first untouched.
	payload2 := []byte("#!ELF fake built binary v2")
	hash2, _, err := s.Put(bytes.NewReader(payload2))
	if err != nil {
		t.Fatalf("Put second object: %v", err)
	}
	if hash2 == hash {
		t.Fatalf("distinct bytes produced the same hash %q", hash)
	}
	if got2, err := os.ReadFile(s.Path(hash2)); err != nil || !bytes.Equal(got2, payload2) {
		t.Errorf("second object = %q (err %v), want %q", got2, err, payload2)
	}
	if got1, err := os.ReadFile(s.Path(hash)); err != nil || !bytes.Equal(got1, payload) {
		t.Errorf("first object after second Put = %q (err %v), want %q untouched", got1, err, payload)
	}

	// No stray files: the store holds exactly the two objects, no leftover
	// ingest temp files.
	entries, err := os.ReadDir(root)
	if err != nil {
		t.Fatalf("read object-store root: %v", err)
	}
	if len(entries) != 2 {
		var names []string
		for _, e := range entries {
			names = append(names, e.Name())
		}
		t.Errorf("object-store root holds %v, want exactly the two hashed objects", names)
	}
}
