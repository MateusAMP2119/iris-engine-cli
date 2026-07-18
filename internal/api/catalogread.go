package api

import (
	"context"
	"errors"
	"net/http"
)

// This file is the catalog read surface (#219): GET /catalog, the pack listing the
// ps overlay renders. It is a read (any role serves it, scope read); the client
// renders only and never touches the filesystem, so remote --host sessions behave
// identically.

// CatalogPack is one pack in the GET /catalog listing: index entry, provenance, and preview material.
type CatalogPack struct {
	// Name is the pack name installs use.
	Name string `json:"name"`
	// Description is the one-line pack summary.
	Description string `json:"description,omitempty"`
	// Tags are the pack's browse labels.
	Tags []string `json:"tags,omitempty"`
	// Requires is the pack's minimum engine version.
	Requires string `json:"requires,omitempty"`
	// SHA256 is the pack tarball digest (remote sources only).
	SHA256 string `json:"sha256,omitempty"`
	// Source names where the pack resolves from (embedded, or a catalog URL).
	Source string `json:"source"`
	// Shadowed marks a pack hidden by a same-named earlier source.
	Shadowed bool `json:"shadowed,omitempty"`
	// Installed marks a pack whose pipelines are all currently registered.
	Installed bool `json:"installed,omitempty"`
	// Pipelines are the pack's declared pipeline names.
	Pipelines []string `json:"pipelines,omitempty"`
	// Files are the workspace-relative paths the pack materializes.
	Files []string `json:"files,omitempty"`
	// ApplyOrder is the derived declare sequence.
	ApplyOrder []string `json:"apply_order,omitempty"`
	// Readme is the pack's documentation.
	Readme string `json:"readme,omitempty"`
}

// CatalogListResult is the GET /catalog payload: packs plus advisory source warnings.
type CatalogListResult struct {
	// Packs are the visible packs across every source, embedded first.
	Packs []CatalogPack `json:"packs"`
	// Warnings name catalog sources that failed to resolve (the overlay banners them).
	Warnings []string `json:"warnings,omitempty"`
}

// CatalogListHandler serves the pack listing; the daemon wires it over the resolver and the registry reader.
type CatalogListHandler interface {
	// ListPacks returns every visible pack with its badges and preview material.
	ListPacks(ctx context.Context) (CatalogListResult, error)
}

// ErrCatalogListUnavailable is the unwired-reader fault for GET /catalog.
var ErrCatalogListUnavailable = errors.New("api: catalog listing not available")

// WithCatalogList wires the pack-listing reader GET /catalog serves from.
func WithCatalogList(h CatalogListHandler) MuxOption {
	return func(m *mux) {
		if h != nil {
			m.catalogList = h
		}
	}
}

// noCatalogList faults the listing until a real reader is wired.
type noCatalogList struct{}

func (noCatalogList) ListPacks(context.Context) (CatalogListResult, error) {
	return CatalogListResult{}, ErrCatalogListUnavailable
}

// serveCatalogList handles GET /catalog: render the listing in the data envelope.
func (m *mux) serveCatalogList(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		WriteError(w, http.StatusMethodNotAllowed, "method_not_allowed", "GET "+r.URL.Path+" only")
		return
	}
	if !noParams(w, r) {
		return
	}
	res, err := m.catalogList.ListPacks(r.Context())
	if err != nil {
		WriteError(w, http.StatusInternalServerError, "internal", err.Error())
		return
	}
	WriteData(w, http.StatusOK, res)
}
