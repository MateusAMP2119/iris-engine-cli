package daemon

import (
	"fmt"
	"os"
	"path/filepath"
	"sync"

	"github.com/MateusAMP2119/iris-lakehouse/internal/api"
	"github.com/MateusAMP2119/iris-lakehouse/internal/declare"
)

// This file is the production api.DataSource: the declared-table shape lookup the
// raw /data/{schema}/{table} route resolves requests against. A shape is the
// declared columns in declaration order with their resolved Postgres types (the
// closed type mapping) plus the primary key that keys the route's keyset paging --
// pure declaration, no live-database read. The source of truth is the leader's
// workspace schemas/ tree, the same tree provisioning materializes, so the /data
// shape and the physical table always agree.
//
// The shapes are read from disk lazily and cached: the first request for any table
// discovers the whole schemas/ tree once and every later request serves from the
// cache, so a declare apply that adds a table is picked up by a daemon restart
// (which the schemas/ tree edit already implies) without re-walking the tree per
// request. The cache never serves a table that is not declared -- an undeclared
// table is the route's 404, decided by Postgres-independent declaration data.

// workspaceDataSource resolves declared-table shapes from a workspace's schemas/
// tree, caching the discovered set on first use.
type workspaceDataSource struct {
	workspace string

	mu     sync.Mutex
	shapes map[string]*api.DataShape
	loaded bool
	err    error
}

// compile-time proof the workspace data source satisfies the api seam.
var _ api.DataSource = (*workspaceDataSource)(nil)

// newWorkspaceDataSource builds the /data shape source over the leader's workspace
// tree. Discovery is deferred to the first DataShape call.
func newWorkspaceDataSource(workspace string) *workspaceDataSource {
	return &workspaceDataSource{workspace: workspace}
}

// DataShape returns the declared shape of schema.table, or false when no such table
// is declared under schemas/ (the route's 404) or when the tree cannot be read (a
// misread tree yields no shapes, so /data 404s rather than serving a half-built
// shape). It walks and caches the tree on first use.
func (s *workspaceDataSource) DataShape(schema, table string) (*api.DataShape, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.loaded {
		s.shapes, s.err = discoverDataShapes(s.workspace)
		s.loaded = true
	}
	if s.err != nil {
		return nil, false
	}
	sh, ok := s.shapes[schema+"."+table]
	return sh, ok
}

// discoverDataShapes walks the workspace schemas/ tree and builds the declared-table
// shapes keyed "schema.table". An absent schemas/ tree yields an empty set (no
// declared tables, every /data request 404s); a malformed tree or an unmappable
// column type is an error the caller treats as "no shapes".
func discoverDataShapes(workspace string) (map[string]*api.DataShape, error) {
	schemasDir := filepath.Join(workspace, "schemas")
	if info, err := os.Stat(schemasDir); err != nil || !info.IsDir() {
		return map[string]*api.DataShape{}, nil
	}
	tables, err := declare.ValidateSchemaTree(schemasDir)
	if err != nil {
		return nil, fmt.Errorf("daemon: read schemas tree for /data shapes: %w", err)
	}
	out := make(map[string]*api.DataShape, len(tables))
	for _, dt := range tables {
		shape, err := dataShapeOf(dt.Spec)
		if err != nil {
			return nil, err
		}
		out[dt.Schema+"."+dt.Table] = shape
	}
	return out, nil
}

// dataShapeOf renders one declared table into its /data shape: the declared columns
// in declaration order with their resolved Postgres types, and the primary-key
// column list that keys the route's keyset paging.
func dataShapeOf(t *declare.Table) (*api.DataShape, error) {
	if t == nil {
		return nil, fmt.Errorf("daemon: /data shape: nil declared table")
	}
	cols := make([]api.ResponseColumn, 0, len(t.Columns))
	var pk []string
	for _, c := range t.Columns {
		pt, err := declare.ResolveColumnType(c)
		if err != nil {
			return nil, fmt.Errorf("daemon: /data shape %s.%s: column %q: %w", t.Schema, t.Table, c.Name, err)
		}
		cols = append(cols, api.ResponseColumn{Name: c.Name, PgType: pt})
		if c.PrimaryKey {
			pk = append(pk, c.Name)
		}
	}
	return &api.DataShape{Schema: t.Schema, Table: t.Table, Columns: cols, PrimaryKey: pk}, nil
}
