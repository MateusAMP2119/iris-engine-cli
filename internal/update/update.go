// Package update replaces the running iris binary with the latest published
// GitHub release, mirroring the curl installer without a package manager. It is a
// stdlib-only leaf: it resolves the latest release tag by following the
// redirect of the releases/latest URL (no GitHub API JSON, so no rate-limit
// exposure), downloads and SHA-256-verifies the platform archive against the
// release checksums, extracts the iris member, and atomically replaces the
// running executable. The CLI drives it and owns the exit-code and output
// surface; this package owns the fetch, verification, and replace.
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
	"io"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"time"
)

// gitHubBase is the release surface the production updater walks: GitHub follows
// <base>/releases/latest with a redirect to <base>/releases/tag/<tag> and serves
// assets under <base>/releases/download/<tag>/.
const gitHubBase = "https://github.com/MateusAMP2119/iris-engine-cli"

// maxDownloadBytes caps a single release download, so a runaway or hostile
// response cannot exhaust memory. A published iris archive is a few tens of MB.
const maxDownloadBytes = 512 << 20

// The progress stages a self-update passes through, in order. They are the stable
// keys the optional Updater.Progress hook is called with, so the CLI can render a
// live journey; the detail string that accompanies each carries the variable
// payload (the tag, the asset and size, "OK", "done"). They exist only for
// rendering: the update's correctness never depends on them.
const (
	// StageResolve fires once the latest release tag is resolved and a replace is
	// due; detail is the tag.
	StageResolve = "resolve"
	// StageDownload fires once the platform archive is fetched; detail is the asset
	// name and human size, tab-separated ("iris_<goos>_<goarch>.tar.gz\t5.8 MB").
	StageDownload = "download"
	// StageVerify fires once the archive's SHA-256 matches its checksum; detail is
	// "OK".
	StageVerify = "verify"
	// StageReplace fires once the running executable is atomically replaced; detail
	// is "done".
	StageReplace = "replace"
)

// Status is the terminal state of a self-update.
type Status int

const (
	// StatusUpToDate reports the running binary already matches the latest release,
	// so nothing was downloaded or replaced.
	StatusUpToDate Status = iota
	// StatusUpdated reports the running binary was replaced with the latest release.
	StatusUpdated
)

// Result is the outcome of a completed self-update: the terminal status, the
// running version (From) and latest tag (To), and, when replaced, the executable
// path.
type Result struct {
	Status Status
	From   string
	To     string
	Path   string
}

// DevBuildError reports that self-update was refused because the running binary is
// an unstamped dev build (not installed from a release). Its message carries
// installer guidance.
type DevBuildError struct{ Version string }

// Error describes the refusal and points at the ways to obtain a release build.
func (e *DevBuildError) Error() string {
	return fmt.Sprintf("this is a %q build, not installed from a release, so there is nothing to self-update to; "+
		"install a release with the curl installer (see the project README) or with \"go install\", then update from there", e.Version)
}

// Updater replaces the running iris binary with the latest GitHub release. Its
// fields are unexported so the seams (release base URL, HTTP client, target
// platform, and executable path) can be driven by tests while production is
// constructed by New with the real GitHub surface.
type Updater struct {
	baseURL  string
	client   *http.Client
	goos     string
	goarch   string
	execPath func() (string, error)
	// Progress, when non-nil, is invoked as each self-update stage completes, with a
	// stable stage key (StageResolve, StageDownload, StageVerify, StageReplace) and a
	// human detail string. It lets the CLI render a live journey; this package stays
	// a stdlib-only, silent leaf, so a nil Progress (the default from New) emits
	// nothing. It is called synchronously on Run's goroutine, in stage order.
	Progress func(stage, detail string)
}

// New returns a production Updater targeting the iris GitHub releases for the
// running platform. Its HTTP client follows redirects (the latest->tag resolution
// relies on it) and carries a bounded timeout in addition to the caller's context.
func New() *Updater {
	return &Updater{
		baseURL:  gitHubBase,
		client:   &http.Client{Timeout: 5 * time.Minute},
		goos:     runtime.GOOS,
		goarch:   runtime.GOARCH,
		execPath: os.Executable,
	}
}

// Run performs the self-update for a binary currently reporting version current.
// A dev build is refused before any I/O. Otherwise it resolves the latest tag; an
// equal tag reports up-to-date without downloading; a newer tag is downloaded,
// checksum-verified, and atomically swapped over the running executable. The
// context bounds every network call.
func (u *Updater) Run(ctx context.Context, current string) (Result, error) {
	if current == "dev" {
		return Result{}, &DevBuildError{Version: current}
	}

	tag, err := u.latestTag(ctx)
	if err != nil {
		return Result{}, err
	}
	if tag == current {
		return Result{Status: StatusUpToDate, From: current, To: tag}, nil
	}
	u.report(StageResolve, tag)

	asset := fmt.Sprintf("iris_%s_%s.tar.gz", u.goos, u.goarch)
	downloadBase := u.baseURL + "/releases/download/" + tag + "/"

	archive, err := u.download(ctx, downloadBase+asset)
	if err != nil {
		return Result{}, fmt.Errorf("download release archive: %w", err)
	}
	checksums, err := u.download(ctx, downloadBase+"checksums.txt")
	if err != nil {
		return Result{}, fmt.Errorf("download checksums: %w", err)
	}
	u.report(StageDownload, asset+"\t"+humanBytes(len(archive)))
	if err := verifyChecksum(archive, checksums, asset); err != nil {
		return Result{}, err
	}
	u.report(StageVerify, "OK")
	binary, err := extractIris(archive)
	if err != nil {
		return Result{}, err
	}

	path, err := u.resolveExecutable()
	if err != nil {
		return Result{}, err
	}
	if err := replaceExecutable(path, binary); err != nil {
		return Result{}, err
	}
	u.report(StageReplace, "done")
	return Result{Status: StatusUpdated, From: current, To: tag, Path: path}, nil
}

// report invokes the Progress hook when one is set. A nil hook is silent, keeping
// this package a stdlib-only leaf with no output surface of its own.
func (u *Updater) report(stage, detail string) {
	if u.Progress != nil {
		u.Progress(stage, detail)
	}
}

// humanBytes formats a byte count as a short decimal-scaled string ("5.8 MB"),
// for the download progress detail. It is presentational only.
func humanBytes(n int) string {
	const unit = 1000
	if n < unit {
		return fmt.Sprintf("%d B", n)
	}
	div, exp := int64(unit), 0
	for m := int64(n) / unit; m >= unit; m /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f %cB", float64(n)/float64(div), "kMGTPE"[exp])
}

// latestTag resolves the latest release tag by following the releases/latest
// redirect and reading the tag from the final URL path. It reads no GitHub API
// JSON, so it never touches the rate-limited API.
func (u *Updater) latestTag(ctx context.Context) (string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u.baseURL+"/releases/latest", nil)
	if err != nil {
		return "", fmt.Errorf("build latest-release request: %w", err)
	}
	resp, err := u.client.Do(req)
	if err != nil {
		return "", fmt.Errorf("resolve latest release: %w", err)
	}
	defer drain(resp)

	finalPath := resp.Request.URL.Path
	const marker = "/releases/tag/"
	idx := strings.Index(finalPath, marker)
	if idx < 0 {
		return "", fmt.Errorf("latest release did not redirect to a tag (final url %s)", resp.Request.URL)
	}
	tag := strings.Trim(finalPath[idx+len(marker):], "/")
	if tag == "" {
		return "", fmt.Errorf("latest release redirect carried no tag (final url %s)", resp.Request.URL)
	}
	return tag, nil
}

// download GETs url and returns its body, bounded by maxDownloadBytes. A non-200
// status is an error.
func (u *Updater) download(ctx context.Context, url string) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, fmt.Errorf("build request: %w", err)
	}
	resp, err := u.client.Do(req)
	if err != nil {
		return nil, err
	}
	defer drain(resp)
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("GET %s: status %s", url, resp.Status)
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxDownloadBytes+1))
	if err != nil {
		return nil, fmt.Errorf("read body: %w", err)
	}
	if len(body) > maxDownloadBytes {
		return nil, fmt.Errorf("GET %s: response exceeds %d bytes", url, maxDownloadBytes)
	}
	return body, nil
}

// verifyChecksum confirms archive's SHA-256 matches the asset's line in a
// goreleaser-style checksums.txt ("<hex>  <filename>"). A missing line or a
// mismatch is an error and the caller must not proceed.
func verifyChecksum(archive, checksums []byte, asset string) error {
	want, err := checksumFor(checksums, asset)
	if err != nil {
		return err
	}
	sum := sha256.Sum256(archive)
	got := hex.EncodeToString(sum[:])
	if !strings.EqualFold(got, want) {
		return fmt.Errorf("checksum mismatch for %s: computed %s, release lists %s", asset, got, want)
	}
	return nil
}

// checksumFor returns the hex digest checksums records for asset, or an error
// when no line names it.
func checksumFor(checksums []byte, asset string) (string, error) {
	for _, line := range strings.Split(string(checksums), "\n") {
		fields := strings.Fields(line)
		if len(fields) == 2 && fields[1] == asset {
			return fields[0], nil
		}
	}
	return "", fmt.Errorf("checksums.txt has no entry for %s", asset)
}

// extractIris returns the bytes of the iris member of a gzip-compressed tar
// archive, or an error when the member is absent.
func extractIris(archive []byte) ([]byte, error) {
	gz, err := gzip.NewReader(bytes.NewReader(archive))
	if err != nil {
		return nil, fmt.Errorf("open gzip: %w", err)
	}
	defer func() { _ = gz.Close() }()

	tr := tar.NewReader(gz)
	for {
		hdr, err := tr.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return nil, fmt.Errorf("read tar: %w", err)
		}
		if hdr.Typeflag != tar.TypeReg || filepath.Base(hdr.Name) != "iris" {
			continue
		}
		body, err := io.ReadAll(io.LimitReader(tr, maxDownloadBytes+1))
		if err != nil {
			return nil, fmt.Errorf("read iris member: %w", err)
		}
		if int64(len(body)) > maxDownloadBytes {
			return nil, fmt.Errorf("iris member exceeds %d bytes", maxDownloadBytes)
		}
		return body, nil
	}
	return nil, errors.New("release archive contains no iris binary")
}

// resolveExecutable returns the running executable's path with symlinks resolved,
// so the atomic replace writes the real file rather than a symlink.
func (u *Updater) resolveExecutable() (string, error) {
	path, err := u.execPath()
	if err != nil {
		return "", fmt.Errorf("locate running executable: %w", err)
	}
	resolved, err := filepath.EvalSymlinks(path)
	if err != nil {
		return "", fmt.Errorf("resolve executable symlinks: %w", err)
	}
	return resolved, nil
}

// replaceExecutable atomically replaces the file at path with binary: it writes a
// sibling temp file in the same directory, chmods it 0755, and renames it over
// path (rename within a directory is atomic). On any failure the temp file is
// removed, so a failed replace leaves the original binary and no residue. A
// permission failure carries guidance to escalate or reinstall.
func replaceExecutable(path string, binary []byte) error {
	dir := filepath.Dir(path)
	tmp, err := os.CreateTemp(dir, ".iris-update-*")
	if err != nil {
		if os.IsPermission(err) {
			return permissionGuidance(path, err)
		}
		return fmt.Errorf("create temp file beside executable: %w", err)
	}
	tmpName := tmp.Name()

	cleanup := func(cause error) error {
		return errors.Join(cause, os.Remove(tmpName))
	}

	if _, err := tmp.Write(binary); err != nil {
		_ = tmp.Close()
		return cleanup(fmt.Errorf("write new binary: %w", err))
	}
	if err := tmp.Chmod(0o755); err != nil {
		_ = tmp.Close()
		return cleanup(fmt.Errorf("chmod new binary: %w", err))
	}
	if err := tmp.Close(); err != nil {
		return cleanup(fmt.Errorf("close new binary: %w", err))
	}
	if err := os.Rename(tmpName, path); err != nil {
		if os.IsPermission(err) {
			return cleanup(permissionGuidance(path, err))
		}
		return cleanup(fmt.Errorf("replace executable: %w", err))
	}
	return nil
}

// permissionGuidance wraps a permission failure with actionable next steps.
func permissionGuidance(path string, err error) error {
	return fmt.Errorf("cannot replace %s: %w; re-run with elevated privileges (e.g. sudo) or reinstall with the curl installer", path, err)
}

// drain discards and closes a response body so the connection can be reused.
func drain(resp *http.Response) {
	_, _ = io.Copy(io.Discard, resp.Body)
	_ = resp.Body.Close()
}
