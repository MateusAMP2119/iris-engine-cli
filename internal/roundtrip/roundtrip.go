// Package roundtrip is the archive round-trip harness: it drives payload bytes
// through an encode -> real temp file -> decode cycle and verifies byte-exact
// equality plus a matching SHA-256 checksum.
//
// The concrete archive format is E07's deliverable and does not exist yet. This
// package fixes the round-trip CONVENTION that E07's archive-format tests MUST
// reuse: build a Format from the real Encode/Decode, then pass a representative
// payload to RoundTrip so the actual format round-trips through real temp files
// exactly as proven here -- write the archive, read it back, decode it, and
// assert the recovered payload is byte-identical to the original.
//
// This is test-support infrastructure imported only by _test.go files.
package roundtrip

import (
	"bytes"
	"crypto/sha256"
	"fmt"
	"os"
	"path/filepath"
	"testing"
)

// Format is a reversible byte codec: Encode serializes a payload into the bytes
// written to a file; Decode reconstructs the payload from those bytes. An
// archive format satisfies it, and so does the identity codec.
type Format struct {
	// Name labels the format in test output and failure messages.
	Name string
	// Encode serializes payload into the bytes to be written to a file.
	Encode func(payload []byte) ([]byte, error)
	// Decode reconstructs the original payload from the encoded bytes.
	Decode func(encoded []byte) ([]byte, error)
}

// RoundTrip drives payload through format via a real temp file and fails t on
// any encode/decode error, byte difference, or checksum mismatch. E07's
// archive-format tests MUST reuse it against the real archive Format.
func RoundTrip(t testing.TB, format Format, payload []byte) {
	t.Helper()
	if err := roundTrip(t.TempDir(), format, payload); err != nil {
		t.Fatalf("round trip [%s]: %v", format.Name, err)
	}
}

// roundTrip performs the encode -> write -> read -> decode cycle in dir and
// returns a non-nil error on any mismatch. Kept separate from RoundTrip so
// tests can exercise the failure path without a fake testing.TB.
func roundTrip(dir string, format Format, payload []byte) error {
	encoded, err := format.Encode(payload)
	if err != nil {
		return fmt.Errorf("encode: %w", err)
	}
	path := filepath.Join(dir, "archive.bin")
	if err := os.WriteFile(path, encoded, 0o644); err != nil {
		return fmt.Errorf("write %s: %w", path, err)
	}
	readBack, err := os.ReadFile(path) //nolint:gosec // G304: path is a fixed filename under this harness's own t.TempDir(), read back from what it just wrote; no external input.
	if err != nil {
		return fmt.Errorf("read %s: %w", path, err)
	}
	if !bytes.Equal(readBack, encoded) {
		return fmt.Errorf("encoded bytes changed on the round trip through %s", path)
	}
	if sha256.Sum256(readBack) != sha256.Sum256(encoded) {
		return fmt.Errorf("encoded-byte checksum changed on the round trip through %s", path)
	}
	decoded, err := format.Decode(readBack)
	if err != nil {
		return fmt.Errorf("decode: %w", err)
	}
	if !bytes.Equal(decoded, payload) {
		return fmt.Errorf("decoded payload differs from the original (%d vs %d bytes)", len(decoded), len(payload))
	}
	if sha256.Sum256(decoded) != sha256.Sum256(payload) {
		return fmt.Errorf("decoded-payload checksum differs from the original")
	}
	return nil
}

// Identity is the pass-through codec: it copies bytes unchanged, the trivial
// round-trip baseline.
func Identity() Format {
	return Format{
		Name:   "identity",
		Encode: func(p []byte) ([]byte, error) { return append([]byte(nil), p...), nil },
		Decode: func(e []byte) ([]byte, error) { return append([]byte(nil), e...), nil },
	}
}
