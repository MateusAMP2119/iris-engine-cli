package plugin

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"time"
)

// Install layout: <root>/<name>/<version>/{<name> binary, manifest.yaml copy}.

// Fetcher fetches one URL's bytes; tests inject a fake, production uses HTTPFetch.
type Fetcher func(ctx context.Context, url string) ([]byte, error)

// maxFetchBytes bounds one fetched document (a plugin binary is one executable, not an archive).
const maxFetchBytes = 512 << 20

// HTTPFetch is the production Fetcher: plain GET, timeout, 200-only, size-bounded.
func HTTPFetch(ctx context.Context, url string) ([]byte, error) {
	ctx, cancel := context.WithTimeout(ctx, 5*time.Minute)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, fmt.Errorf("plugin: fetch %s: %w", url, err)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("plugin: fetch %s: %w", url, err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("plugin: fetch %s: unexpected status %s", url, resp.Status)
	}
	data, err := io.ReadAll(io.LimitReader(resp.Body, maxFetchBytes+1))
	if err != nil {
		return nil, fmt.Errorf("plugin: fetch %s: read body: %w", url, err)
	}
	if len(data) > maxFetchBytes {
		return nil, fmt.Errorf("plugin: fetch %s: body exceeds %d bytes", url, maxFetchBytes)
	}
	return data, nil
}

// isURL reports whether source is an http(s) URL rather than a local path.
func isURL(source string) bool {
	return strings.HasPrefix(source, "http://") || strings.HasPrefix(source, "https://")
}

// Digest returns the lowercase hex sha256 of data.
func Digest(data []byte) string {
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

// Dir returns the install directory of one plugin version under the root.
func Dir(root, name, version string) string {
	return filepath.Join(root, name, version)
}

// BinaryPath returns the installed binary's path (the plugin's own name in its version dir).
func BinaryPath(root, name, version string) string {
	return filepath.Join(Dir(root, name, version), name)
}

// Platform returns this process's binary platform key ("goos/goarch").
func Platform() string { return runtime.GOOS + "/" + runtime.GOARCH }

// Install fetches manifest + platform binary, refuses a sha256 deviation, and
// lands the layout atomically (stage + rename; a failed install leaves nothing).
func Install(ctx context.Context, root, source string, fetch Fetcher) (*Resolved, error) {
	if fetch == nil {
		fetch = HTTPFetch
	}
	data, err := readSource(ctx, source, fetch)
	if err != nil {
		return nil, err
	}
	m, err := ParseManifest(data)
	if err != nil {
		return nil, err
	}
	bin, ok := m.Binaries[Platform()]
	if !ok {
		return nil, fmt.Errorf("plugin: %s@%s has no binary for platform %s", m.Name, m.Version, Platform())
	}
	blob, err := readBinary(ctx, source, bin.URL, fetch)
	if err != nil {
		return nil, err
	}
	digest := Digest(blob)
	if !strings.EqualFold(digest, bin.SHA256) {
		return nil, fmt.Errorf("plugin: %s@%s binary digest mismatch: manifest pins %s, fetched %s", m.Name, m.Version, strings.ToLower(bin.SHA256), digest)
	}

	dest := Dir(root, m.Name, m.Version)
	if err := os.MkdirAll(filepath.Dir(dest), 0o755); err != nil {
		return nil, fmt.Errorf("plugin: install %s@%s: %w", m.Name, m.Version, err)
	}
	stage, err := os.MkdirTemp(filepath.Dir(dest), "."+m.Version+".stage-")
	if err != nil {
		return nil, fmt.Errorf("plugin: install %s@%s: stage: %w", m.Name, m.Version, err)
	}
	defer func() { _ = os.RemoveAll(stage) }()
	if err := os.WriteFile(filepath.Join(stage, m.Name), blob, 0o755); err != nil { //nolint:gosec // G306: the plugin binary must be executable by design.
		return nil, fmt.Errorf("plugin: install %s@%s: write binary: %w", m.Name, m.Version, err)
	}
	if err := os.WriteFile(filepath.Join(stage, ManifestFile), data, 0o644); err != nil { //nolint:gosec // G306: the manifest is public metadata.
		return nil, fmt.Errorf("plugin: install %s@%s: write manifest: %w", m.Name, m.Version, err)
	}
	if err := os.RemoveAll(dest); err != nil {
		return nil, fmt.Errorf("plugin: install %s@%s: clear previous: %w", m.Name, m.Version, err)
	}
	if err := os.Rename(stage, dest); err != nil {
		return nil, fmt.Errorf("plugin: install %s@%s: place: %w", m.Name, m.Version, err)
	}
	return &Resolved{Manifest: *m, Binary: BinaryPath(root, m.Name, m.Version), Digest: digest}, nil
}

// readSource reads the manifest bytes from a local path or through the fetcher.
func readSource(ctx context.Context, source string, fetch Fetcher) ([]byte, error) {
	if isURL(source) {
		return fetch(ctx, source)
	}
	data, err := os.ReadFile(source) //nolint:gosec // G304: the manifest path is the operator's own install argument.
	if err != nil {
		return nil, fmt.Errorf("plugin: read manifest %s: %w", source, err)
	}
	return data, nil
}

// readBinary reads the platform binary: fetcher for URLs, else a path relative
// to a local manifest (refused for a URL-sourced manifest).
func readBinary(ctx context.Context, manifestSource, binURL string, fetch Fetcher) ([]byte, error) {
	if isURL(binURL) {
		return fetch(ctx, binURL)
	}
	if isURL(manifestSource) {
		return nil, fmt.Errorf("plugin: manifest fetched from a URL pins non-URL binary %q; a remote manifest must pin remote binaries", binURL)
	}
	path := binURL
	if !filepath.IsAbs(path) {
		path = filepath.Join(filepath.Dir(manifestSource), path)
	}
	data, err := os.ReadFile(path) //nolint:gosec // G304: resolved relative to the operator-supplied manifest by design.
	if err != nil {
		return nil, fmt.Errorf("plugin: read binary %s: %w", path, err)
	}
	return data, nil
}
