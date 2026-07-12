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

	"github.com/goccy/go-yaml"
)

// catalogDataDir is the embed root of the pipeline catalog. Beneath it live
// the ordered catalog.yaml index and one folder per entry: an entry.yaml
// (metadata) beside a workspace/ subtree mirroring exactly what materializes
// into the workspace (specification section 8, quickstart pipeline catalog).
const catalogDataDir = "catalogdata"

// catalogData embeds the pipeline catalog: curated starter pipelines shipped
// inside the binary (go:embed, no network), golden-pinned. Every entry must
// parse through the real declare loaders -- an invalid entry is a test
// failure, never a runtime surprise.
//
//go:embed catalogdata
var catalogData embed.FS

// catalogShowcase names the table and primary key the tour's provenance
// finale queries for one catalog entry.
type catalogShowcase struct {
	Table string `yaml:"table" json:"table"`
	PK    string `yaml:"pk" json:"pk"`
}

// catalogEntry is one pipeline-catalog entry's metadata, parsed from its
// entry.yaml: the id matching its folder, the display name and one-line pitch
// the shop paints, the description a pick renders, the run note the tour's
// run step explains itself with, and the provenance showcase the act closes
// on.
type catalogEntry struct {
	ID          string          `yaml:"id"`
	Name        string          `yaml:"name"`
	Pitch       string          `yaml:"pitch"`
	Description string          `yaml:"description"`
	RunNote     string          `yaml:"run_note"`
	Showcase    catalogShowcase `yaml:"showcase"`
}

// pipelineCatalog is the loaded catalog: the entries in catalog.yaml's order,
// entry 1 the default pick.
type pipelineCatalog struct {
	Entries []catalogEntry
}

// catalogIndex is the catalog.yaml document: the ordered entry-folder list.
type catalogIndex struct {
	Entries []string `yaml:"entries"`
}

// loadCatalog parses the embedded catalog fresh per call (no mutable package
// state): the ordered catalog.yaml index, then each listed entry's entry.yaml.
// It verifies the registry's own shape -- the index lists exactly the entry
// folders, ids are unique and match their folder -- so a malformed embed
// surfaces as a clear error, never a skewed shop.
func loadCatalog() (*pipelineCatalog, error) {
	raw, err := catalogData.ReadFile(catalogDataDir + "/catalog.yaml")
	if err != nil {
		return nil, fmt.Errorf("catalog: read embedded index: %w", err)
	}
	var idx catalogIndex
	if err := yaml.Unmarshal(raw, &idx); err != nil {
		return nil, fmt.Errorf("catalog: parse catalog.yaml: %w", err)
	}
	if len(idx.Entries) == 0 {
		return nil, errors.New("catalog: catalog.yaml lists no entries")
	}

	listed := map[string]bool{}
	cat := &pipelineCatalog{}
	for _, id := range idx.Entries {
		if listed[id] {
			return nil, fmt.Errorf("catalog: catalog.yaml lists %s twice", id)
		}
		listed[id] = true
		raw, err := catalogData.ReadFile(catalogDataDir + "/" + id + "/entry.yaml")
		if err != nil {
			return nil, fmt.Errorf("catalog: read entry %s: %w", id, err)
		}
		var e catalogEntry
		if err := yaml.Unmarshal(raw, &e); err != nil {
			return nil, fmt.Errorf("catalog: parse %s/entry.yaml: %w", id, err)
		}
		if e.ID != id {
			return nil, fmt.Errorf("catalog: entry.yaml id %q does not match its folder %q", e.ID, id)
		}
		cat.Entries = append(cat.Entries, e)
	}

	// The index must list exactly the entry folders: an unlisted folder is as
	// wrong as a listed ghost.
	dirs, err := fs.ReadDir(catalogData, catalogDataDir)
	if err != nil {
		return nil, fmt.Errorf("catalog: read embedded catalog root: %w", err)
	}
	for _, d := range dirs {
		if d.IsDir() && !listed[d.Name()] {
			return nil, fmt.Errorf("catalog: entry folder %s is not listed in catalog.yaml", d.Name())
		}
	}
	return cat, nil
}

// defaultEntry returns the catalog's default pick: entry 1.
func (c *pipelineCatalog) defaultEntry() catalogEntry {
	return c.Entries[0]
}

// entryByID resolves one catalog entry by id; an unknown id is an error
// naming the available ids.
func (c *pipelineCatalog) entryByID(id string) (catalogEntry, error) {
	for _, e := range c.Entries {
		if e.ID == id {
			return e, nil
		}
	}
	ids := make([]string, len(c.Entries))
	for i, e := range c.Entries {
		ids[i] = e.ID
	}
	return catalogEntry{}, fmt.Errorf("catalog: unknown pipeline %q; available: %s", id, strings.Join(ids, ", "))
}

// catalogEntryByID loads the catalog fresh and resolves one entry by id; an
// unknown id is an error naming the available ids.
func catalogEntryByID(id string) (catalogEntry, error) {
	cat, err := loadCatalog()
	if err != nil {
		return catalogEntry{}, err
	}
	return cat.entryByID(id)
}

// materializeCatalogEntry writes one embedded catalog entry's workspace
// subtree into the workspace rooted at root, write-if-absent: a missing file
// is created (0644, parent directories 0755), a present byte-identical file
// is left alone, and a present-but-different file is kept with a warning line
// on warn -- the catalog never clobbers an operator's file. Creation is
// race-safe (temp file plus link, so a concurrent writer's file survives
// intact). It returns the workspace-relative paths it wrote, in walk order.
func materializeCatalogEntry(id, root string, warn io.Writer) ([]string, error) {
	if _, err := catalogEntryByID(id); err != nil {
		return nil, err
	}
	walkRoot := catalogDataDir + "/" + id + "/workspace"
	var written []string
	err := fs.WalkDir(catalogData, walkRoot, func(path string, d fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if d.IsDir() {
			return nil
		}
		rel := strings.TrimPrefix(path, walkRoot+"/")
		want, err := catalogData.ReadFile(path)
		if err != nil {
			return fmt.Errorf("catalog: read embedded entry file %s: %w", path, err)
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
