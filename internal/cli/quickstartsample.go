package cli

import (
	"bytes"
	"embed"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
)

// quickstartDataDir is the embed root of the quickstart sample tree. The paths
// beneath it mirror the workspace layout the materializer writes:
// pipelines/hello_iris/{iris-declare.yaml,main.sh} and
// schemas/demo/colors/table.yaml (specification section 8, quickstart sample).
const quickstartDataDir = "quickstartdata"

// quickstartData embeds the hello_iris sample: the golden-pinned declaration,
// its POSIX-sh script, and the demo.colors table file. The sample ships inside
// the binary and is materialized on demand into the workspace; its files must
// always parse through the real declare loaders (an invalid sample is a test
// failure, never a runtime surprise).
//
//go:embed quickstartdata
var quickstartData embed.FS

// materializeQuickstartSample writes the embedded hello_iris sample into the
// workspace rooted at root, write-if-absent: a missing file is created (0644,
// parent directories 0755), a present byte-identical file is left alone, and a
// present-but-different file is kept with a warning line on warn -- the sample
// never clobbers an operator's file. Creation is race-safe (temp file plus
// link, so a concurrent writer's file survives intact). It returns the
// workspace-relative paths it wrote, in walk order.
func materializeQuickstartSample(root string, warn io.Writer) ([]string, error) {
	var written []string
	err := fs.WalkDir(quickstartData, quickstartDataDir, func(path string, d fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if d.IsDir() {
			return nil
		}
		rel := strings.TrimPrefix(path, quickstartDataDir+"/")
		want, err := quickstartData.ReadFile(path)
		if err != nil {
			return fmt.Errorf("quickstart: read embedded sample %s: %w", path, err)
		}
		wrote, err := writeSampleFile(filepath.Join(root, filepath.FromSlash(rel)), rel, want, warn)
		if err != nil {
			return err
		}
		if wrote {
			written = append(written, rel)
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return written, nil
}

// writeSampleFile creates one sample file at target with content want, unless a
// file is already there: an identical file is silently kept, a different one is
// kept with a warning naming its workspace-relative path. The create is
// create-once: the content lands in a same-directory temp file first and is
// linked into place, so a target that appears concurrently is never truncated
// or half-written. It reports whether it wrote the file.
func writeSampleFile(target, rel string, want []byte, warn io.Writer) (bool, error) {
	got, err := os.ReadFile(target) //nolint:gosec // G304: target is the workspace-relative path of an embedded sample file, not user input.
	switch {
	case err == nil:
		return false, keepExisting(got, want, rel, warn)
	case !errors.Is(err, fs.ErrNotExist):
		return false, fmt.Errorf("quickstart: probe sample file %s: %w", target, err)
	}

	dir := filepath.Dir(target)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return false, fmt.Errorf("quickstart: create sample directory %s: %w", dir, err)
	}
	tmp, err := os.CreateTemp(dir, ".iris-quickstart-*")
	if err != nil {
		return false, fmt.Errorf("quickstart: stage sample file %s: %w", rel, err)
	}
	tmpName := tmp.Name()
	_, werr := tmp.Write(want)
	if cerr := tmp.Close(); werr == nil {
		werr = cerr
	}
	if werr == nil {
		// CreateTemp opens 0600; the sample is workspace source, so world-readable.
		werr = os.Chmod(tmpName, 0o644)
	}
	if werr != nil {
		return false, errors.Join(
			fmt.Errorf("quickstart: stage sample file %s: %w", rel, werr),
			os.Remove(tmpName),
		)
	}

	if err := os.Link(tmpName, target); err != nil {
		rmErr := os.Remove(tmpName)
		if errors.Is(err, fs.ErrExist) {
			// Lost a create race: another writer owns the file now; never clobber it.
			existing, rerr := os.ReadFile(target) //nolint:gosec // G304: same workspace sample path as above.
			if rerr != nil {
				return false, errors.Join(fmt.Errorf("quickstart: re-read sample file %s: %w", target, rerr), rmErr)
			}
			return false, errors.Join(keepExisting(existing, want, rel, warn), rmErr)
		}
		return false, errors.Join(fmt.Errorf("quickstart: place sample file %s: %w", target, err), rmErr)
	}
	if err := os.Remove(tmpName); err != nil {
		return true, fmt.Errorf("quickstart: remove staging file %s: %w", tmpName, err)
	}
	return true, nil
}

// keepExisting resolves a present sample target: byte-identical content is the
// idempotent re-run (silent), different content is the operator's file, kept
// with one warning line on warn.
func keepExisting(got, want []byte, rel string, warn io.Writer) error {
	if bytes.Equal(got, want) {
		return nil
	}
	_, err := fmt.Fprintf(warn, "iris: warning: %s exists and differs from the embedded quickstart sample; keeping your file\n", rel)
	if err != nil {
		return fmt.Errorf("quickstart: report kept sample file %s: %w", rel, err)
	}
	return nil
}
