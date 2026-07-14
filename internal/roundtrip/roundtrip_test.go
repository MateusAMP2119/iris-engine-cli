package roundtrip

import (
	"bytes"
	"compress/gzip"
	"errors"
	"io"
	"os"
	"path/filepath"
	"testing"

	"github.com/MateusAMP2119/iris-engine-cli/internal/fixtures"
)

// gzipFormat is a representative archive-like format (compression) built only
// on the standard library, standing in for E07's real archive format.
func gzipFormat() Format {
	return Format{
		Name: "gzip",
		Encode: func(p []byte) ([]byte, error) {
			var buf bytes.Buffer
			w := gzip.NewWriter(&buf)
			if _, err := w.Write(p); err != nil {
				return nil, err
			}
			if err := w.Close(); err != nil {
				return nil, err
			}
			return buf.Bytes(), nil
		},
		Decode: func(e []byte) ([]byte, error) {
			r, err := gzip.NewReader(bytes.NewReader(e))
			if err != nil {
				return nil, err
			}
			defer func() { _ = r.Close() }()
			return io.ReadAll(r)
		},
	}
}

// TestArchiveRoundTripTempFiles round-trips representative payloads through the
// harness against real temp files: the identity and gzip formats over the
// whole golden-workspace fixture inventory plus empty and binary payloads,
// proving byte-exact and checksum-verified preservation through a real file.
func TestArchiveRoundTripTempFiles(t *testing.T) {
	formats := []Format{Identity(), gzipFormat()}
	payloads := fixturePayloads(t)
	for _, f := range formats {
		for _, p := range payloads {
			t.Run(f.Name+"/"+p.name, func(t *testing.T) {
				RoundTrip(t, f, p.bytes) // fails on any byte/checksum mismatch
			})
		}
		t.Run(f.Name+"/empty", func(t *testing.T) { RoundTrip(t, f, nil) })
		t.Run(f.Name+"/binary", func(t *testing.T) {
			RoundTrip(t, f, []byte{0x00, 0x01, 0xff, 0x00, 0xfe})
		})
	}
}

// TestRoundTripDetectsCorruption proves the harness is not vacuous: a codec
// that corrupts the payload, or fails to encode, fails the round trip.
func TestRoundTripDetectsCorruption(t *testing.T) {
	corrupting := Format{
		Name:   "corrupting",
		Encode: func(p []byte) ([]byte, error) { return p, nil },
		Decode: func(e []byte) ([]byte, error) { return append(append([]byte{}, e...), 'X'), nil },
	}
	if err := roundTrip(t.TempDir(), corrupting, []byte("payload")); err == nil {
		t.Error("corrupting codec passed the round trip, want failure")
	}

	failEncode := Format{
		Name:   "fail-encode",
		Encode: func(p []byte) ([]byte, error) { return nil, errors.New("boom") },
		Decode: func(e []byte) ([]byte, error) { return e, nil },
	}
	if err := roundTrip(t.TempDir(), failEncode, []byte("payload")); err == nil {
		t.Error("encode error swallowed, want failure")
	}
}

type payload struct {
	name  string
	bytes []byte
}

// fixturePayloads reads every regular file in the golden workspace fixture as a
// representative payload, exercising the shared fixtures accessors.
func fixturePayloads(t *testing.T) []payload {
	t.Helper()
	root := fixtures.WorkspaceGolden()
	var out []payload
	err := filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			return nil
		}
		b, err := os.ReadFile(path) //nolint:gosec // G304: path comes from WalkDir over the in-repo golden fixture tree, a trusted checked-in source.
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(root, path)
		if err != nil {
			return err
		}
		out = append(out, payload{name: filepath.ToSlash(rel), bytes: b})
		return nil
	})
	if err != nil {
		t.Fatalf("walk workspace fixtures under %s: %v", root, err)
	}
	if len(out) == 0 {
		t.Fatalf("no workspace fixtures found under %s", root)
	}
	return out
}
