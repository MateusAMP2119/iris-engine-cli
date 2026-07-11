package update

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
)

// tarGzWithIris packs body as the single `iris` member of a gzip-compressed tar
// archive, the shape the release publishes as iris_<GOOS>_<GOARCH>.tar.gz.
func tarGzWithIris(t *testing.T, body []byte) []byte {
	t.Helper()
	var gzBuf bytes.Buffer
	gw := gzip.NewWriter(&gzBuf)
	tw := tar.NewWriter(gw)
	hdr := &tar.Header{Name: "iris", Mode: 0o755, Size: int64(len(body)), Typeflag: tar.TypeReg}
	if err := tw.WriteHeader(hdr); err != nil {
		t.Fatalf("tar header: %v", err)
	}
	if _, err := tw.Write(body); err != nil {
		t.Fatalf("tar write: %v", err)
	}
	if err := tw.Close(); err != nil {
		t.Fatalf("tar close: %v", err)
	}
	if err := gw.Close(); err != nil {
		t.Fatalf("gzip close: %v", err)
	}
	return gzBuf.Bytes()
}

// sha256Hex returns the lowercase hex SHA-256 of b, the form a checksums.txt line
// carries.
func sha256Hex(b []byte) string {
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

// releaseServer serves the GitHub release surface the updater walks: the
// latest->tag redirect, the archive asset, and checksums.txt. checksumFor is the
// hex written into checksums.txt for the archive (so a caller can plant a
// mismatch); when empty the archive's real digest is used. Each request to a
// download endpoint (the archive or checksums.txt) increments downloadHits when
// it is non-nil, so a test can assert the up-to-date path downloads nothing.
func releaseServer(t *testing.T, tag string, archive []byte, checksumFor string, downloadHits *int32) *httptest.Server {
	t.Helper()
	goos, goarch := "linux", "amd64"
	asset := fmt.Sprintf("iris_%s_%s.tar.gz", goos, goarch)
	sum := checksumFor
	if sum == "" {
		sum = sha256Hex(archive)
	}
	checksums := fmt.Sprintf("%s  %s\nfeed0000  iris_other_arch.tar.gz\n", sum, asset)
	countHit := func() {
		if downloadHits != nil {
			atomic.AddInt32(downloadHits, 1)
		}
	}

	mux := http.NewServeMux()
	mux.HandleFunc("/releases/latest", func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "/releases/tag/"+tag, http.StatusFound)
	})
	mux.HandleFunc("/releases/tag/"+tag, func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte("<html>release page</html>"))
	})
	mux.HandleFunc("/releases/download/"+tag+"/"+asset, func(w http.ResponseWriter, _ *http.Request) {
		countHit()
		_, _ = w.Write(archive)
	})
	mux.HandleFunc("/releases/download/"+tag+"/checksums.txt", func(w http.ResponseWriter, _ *http.Request) {
		countHit()
		_, _ = w.Write([]byte(checksums))
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv
}

// testUpdater builds an Updater aimed at srv, replacing execPath's target, fixed
// to the linux/amd64 asset the release server serves.
func testUpdater(srv *httptest.Server, execPath string) *Updater {
	return &Updater{
		baseURL:  srv.URL,
		client:   srv.Client(),
		goos:     "linux",
		goarch:   "amd64",
		execPath: func() (string, error) { return execPath, nil },
	}
}

// TestUpdateVerifiedAtomicReplace proves the happy path end to end against a
// local release server: the updater resolves the latest tag from the redirect,
// downloads the archive and checksums.txt, verifies the archive SHA-256, extracts
// the iris member, and atomically replaces the running executable with the
// fetched bytes at mode 0755.
//
// spec: S08/update-verified-atomic-replace
func TestUpdateVerifiedAtomicReplace(t *testing.T) {
	dir := t.TempDir()
	exe := filepath.Join(dir, "iris")
	if err := os.WriteFile(exe, []byte("OLD-BINARY"), 0o755); err != nil {
		t.Fatalf("seed exe: %v", err)
	}
	newBytes := []byte("NEW-BINARY-BYTES-v2")
	srv := releaseServer(t, "v2.0.0", tarGzWithIris(t, newBytes), "", nil)

	u := testUpdater(srv, exe)
	res, err := u.Run(context.Background(), "v1.0.0")
	if err != nil {
		t.Fatalf("Run: unexpected error: %v", err)
	}
	if res.Status != StatusUpdated {
		t.Errorf("status = %v, want StatusUpdated", res.Status)
	}
	if res.To != "v2.0.0" || res.From != "v1.0.0" {
		t.Errorf("From/To = %q/%q, want v1.0.0/v2.0.0", res.From, res.To)
	}
	got, err := os.ReadFile(exe) //nolint:gosec // G304: exe is this test's own scratch path under t.TempDir(), never user or network input.
	if err != nil {
		t.Fatalf("read replaced exe: %v", err)
	}
	if !bytes.Equal(got, newBytes) {
		t.Errorf("replaced exe = %q, want %q", got, newBytes)
	}
	info, err := os.Stat(exe)
	if err != nil {
		t.Fatalf("stat replaced exe: %v", err)
	}
	if info.Mode().Perm() != 0o755 {
		t.Errorf("replaced exe mode = %v, want 0755", info.Mode().Perm())
	}
}

// TestUpdateUpToDateNoDownload proves the up-to-date decision on its own terms:
// when the resolved latest tag equals the running version, Run reports
// StatusUpToDate and downloads nothing -- neither the archive nor checksums.txt
// endpoint is hit -- so the running binary is never touched. This exercises the
// contract's substance (the real tag==current decision), not just the CLI
// rendering of an injected outcome.
//
// spec: S08/update-tag-equals-up-to-date
func TestUpdateUpToDateNoDownload(t *testing.T) {
	dir := t.TempDir()
	exe := filepath.Join(dir, "iris")
	orig := []byte("SAME-BINARY-v3")
	if err := os.WriteFile(exe, orig, 0o755); err != nil {
		t.Fatalf("seed exe: %v", err)
	}
	var downloadHits int32
	srv := releaseServer(t, "v3.1.0", tarGzWithIris(t, []byte("never-served")), "", &downloadHits)

	u := testUpdater(srv, exe)
	res, err := u.Run(context.Background(), "v3.1.0")
	if err != nil {
		t.Fatalf("Run: unexpected error: %v", err)
	}
	if res.Status != StatusUpToDate {
		t.Errorf("status = %v, want StatusUpToDate", res.Status)
	}
	if res.From != "v3.1.0" || res.To != "v3.1.0" {
		t.Errorf("From/To = %q/%q, want v3.1.0/v3.1.0", res.From, res.To)
	}
	if n := atomic.LoadInt32(&downloadHits); n != 0 {
		t.Errorf("download endpoints hit %d times on an up-to-date check, want 0 (nothing downloaded)", n)
	}
	got, err := os.ReadFile(exe) //nolint:gosec // G304: exe is this test's own scratch path under t.TempDir(), never user or network input.
	if err != nil {
		t.Fatalf("read exe: %v", err)
	}
	if !bytes.Equal(got, orig) {
		t.Errorf("executable changed on an up-to-date check: %q, want %q", got, orig)
	}
}

// TestUpdateChecksumMismatchAborts proves an archive whose SHA-256 does not match
// its checksums.txt line aborts the update with an error and never touches the
// running executable: the on-disk binary keeps its original bytes and mode.
//
// spec: S08/update-checksum-mismatch-aborts
func TestUpdateChecksumMismatchAborts(t *testing.T) {
	dir := t.TempDir()
	exe := filepath.Join(dir, "iris")
	orig := []byte("ORIGINAL-BINARY")
	if err := os.WriteFile(exe, orig, 0o755); err != nil {
		t.Fatalf("seed exe: %v", err)
	}
	// Serve a checksums.txt line that does not match the archive's real digest.
	srv := releaseServer(t, "v2.0.0", tarGzWithIris(t, []byte("TAMPERED")), "deadbeefdeadbeef", nil)

	u := testUpdater(srv, exe)
	_, err := u.Run(context.Background(), "v1.0.0")
	if err == nil {
		t.Fatal("Run: nil error on checksum mismatch, want an abort")
	}
	got, rerr := os.ReadFile(exe) //nolint:gosec // G304: exe is this test's own scratch path under t.TempDir(), never user or network input.
	if rerr != nil {
		t.Fatalf("read exe: %v", rerr)
	}
	if !bytes.Equal(got, orig) {
		t.Errorf("executable was modified on a checksum mismatch: %q, want %q", got, orig)
	}
}

// TestUpdateDevBuildRefusesInPackage proves the dev-build guard short-circuits
// before any network or filesystem I/O: Run on a "dev" build returns a
// *DevBuildError carrying installer guidance, with a client that would fail if it
// were ever used, so no request is made.
//
// spec: S08/update-dev-build-refuses
func TestUpdateDevBuildRefusesInPackage(t *testing.T) {
	u := &Updater{
		baseURL: "http://127.0.0.1:0", // never dialed: the dev guard returns first
		client:  &http.Client{Transport: failTransport{}},
		goos:    "linux",
		goarch:  "amd64",
		execPath: func() (string, error) {
			t.Fatal("execPath resolved on a dev build; the guard must return before touching the filesystem")
			return "", nil
		},
	}
	_, err := u.Run(context.Background(), "dev")
	var dev *DevBuildError
	if !errors.As(err, &dev) {
		t.Fatalf("Run(dev) error = %v, want *DevBuildError", err)
	}
}

// failTransport fails every round trip, proving the dev guard makes no request.
type failTransport struct{}

func (failTransport) RoundTrip(*http.Request) (*http.Response, error) {
	return nil, errors.New("network used on a dev build")
}
