package api

import (
	"errors"
	"net/http"
	"net/url"
	"strings"

	"github.com/MateusAMP2119/iris-engine-cli/internal/store"
)

// This file is the raw /data/{schema}/{table} route: the ad-hoc debugging
// surface over the declared tables -- column projection, eq/range filters,
// keyset paging by the table PK -- executing through the same shared read pool
// and role mechanics as /q. The route never assembles SQL from a request: the
// statement is engine-assembled per shape (table, projection, and the set of
// filtered columns) from validated identifiers only, referencing no column
// beyond what the request itself addresses, so Postgres alone decides what the
// caller's role may read (a clause on an unaddressed column would drag its
// grant into every read). Disposable rows are served like any other: no
// predicate here or anywhere on the surface filters them out.

// DataShape is one declared table as the /data route serves it: the ordered
// declared columns with their resolved Postgres types (the closed type mapping)
// and the primary key that keys the route's keyset paging.
type DataShape struct {
	// Schema and Table name the declared source.
	Schema string
	Table  string
	// Columns are the declared columns, in declaration order, each with its
	// resolved Postgres type.
	Columns []ResponseColumn
	// PrimaryKey is the table's primary key column list.
	PrimaryKey []string
}

// DataSource is the declared-table lookup the /data route resolves requests
// against: the daemon supplies the shapes of the declared schemas/ tree; a
// fake stands in for integration tests.
type DataSource interface {
	// DataShape returns the declared shape of schema.table, or false when no
	// such table is declared (the route's 404).
	DataShape(schema, table string) (*DataShape, bool)
}

// WithDataSource wires the declared-table shape source the /data route
// resolves requests against. A nil source is ignored, keeping the unwired
// default (/data answers the internal-fault envelope).
func WithDataSource(src DataSource) MuxOption {
	return func(m *mux) {
		if src != nil {
			m.datasrc = src
		}
	}
}

// serveData handles GET /data/{schema}/{table}. With either seam unwired it
// answers the internal-fault envelope. Wired, it resolves the declared
// shape (404 for an undeclared table), parses the projection and the eq/range
// wire grammar (400 naming a refused param), assembles the fixed statement from
// validated identifiers, and executes it through the read pool as the calling
// PAT's role -- a grant Postgres refuses is a 403 naming the addressed table,
// never the fields.
func (m *mux) serveData(w http.ResponseWriter, r *http.Request, schema, table string) {
	if m.datasrc == nil || m.readexec == nil {
		serveUnwiredRead(w, r, "data")
		return
	}
	if r.Method != http.MethodGet {
		WriteError(w, http.StatusMethodNotAllowed, string(CodeMethodNotAllowed), "GET "+r.URL.Path+" only")
		return
	}

	shape, ok := m.datasrc.DataShape(schema, table)
	if !ok {
		WriteError(w, http.StatusNotFound, string(CodeNotFound), "no such declared table: "+schema+"."+table)
		return
	}

	q := r.URL.Query()
	projection, err := dataProjection(shape, q)
	if err != nil {
		writeDataFault(w, shape, err)
		return
	}
	delete(q, "columns") // consumed above; the filter grammar never sees it.

	fields := make(map[string]string, len(shape.Columns))
	for _, c := range shape.Columns {
		fields[c.Name] = c.PgType
	}
	plan, err := PlanDataQuery(shape.PrimaryKey, fields, q)
	if err != nil {
		writeDataFault(w, shape, err)
		return
	}

	// The statement references only what the request addresses: the projection,
	// the filtered columns, and the PK paging key. Anything else stays out of
	// the text, so an unrelated column's grant is never dragged into the read.
	filtered := make(map[string]string, len(plan.Predicates)+len(shape.PrimaryKey))
	for _, p := range plan.Predicates {
		filtered[p.Column] = fields[p.Column]
	}
	for _, k := range shape.PrimaryKey {
		filtered[k] = fields[k]
	}
	stmt, err := BuildDataStatement(shape.Schema, shape.Table, projection, filtered, shape.PrimaryKey)
	if err != nil {
		writeDataFault(w, shape, err)
		return
	}
	args, err := BindArgs(stmt.Params, plan)
	if err != nil {
		writeDataFault(w, shape, err)
		return
	}

	rows, err := executeRead(r.Context(), m.readexec, stmt.Name, stmt.SQL, args, projection)
	if err != nil {
		writeDataFault(w, shape, err)
		return
	}
	if rows == nil {
		rows = []map[string]any{}
	}
	WriteDataPage(w, http.StatusOK, rows, Page{NextAfter: plan.Cursor.NextAfter(rows), Limit: plan.Cursor.Limit})
}

// writeDataFault maps a /data failure onto the closed error envelope: a
// wire-grammar rejection is a 400 naming the param; a grant Postgres refused is
// a 403 forbidden naming the addressed table with a fresh message (never the
// Postgres text, never any field name); everything else is the 500 internal
// envelope.
func writeDataFault(w http.ResponseWriter, shape *DataShape, err error) {
	var pe *ParamError
	switch {
	case errors.As(err, &pe):
		WriteError(w, http.StatusBadRequest, string(CodeBadParam), pe.Error())
	case errors.Is(err, store.ErrReadForbidden):
		WriteError(w, http.StatusForbidden, string(CodeForbidden),
			"forbidden: the calling role lacks a grant on "+shape.Schema+"."+shape.Table)
	default:
		WriteError(w, http.StatusInternalServerError, string(CodeInternal),
			"api: data "+shape.Schema+"."+shape.Table+": "+err.Error())
	}
}

// dataProjection resolves the route's column projection: absent, the full
// declared column list in declaration order; present, columns= is one
// comma-separated list of declared column names, served in caller order. The
// projection must include every primary-key column -- the PK is the route's
// paging key, and a page without its key cannot continue -- and an unknown,
// repeated, or empty name is a 400 naming the param.
func dataProjection(shape *DataShape, q url.Values) ([]string, error) {
	vs, ok := q["columns"]
	if !ok {
		all := make([]string, len(shape.Columns))
		for i, c := range shape.Columns {
			all[i] = c.Name
		}
		return all, nil
	}
	if len(vs) > 1 {
		return nil, paramErrf("columns", "repeated parameter")
	}

	declared := make(map[string]struct{}, len(shape.Columns))
	for _, c := range shape.Columns {
		declared[c.Name] = struct{}{}
	}
	seen := make(map[string]struct{})
	var out []string
	for _, part := range strings.Split(vs[0], ",") {
		name := strings.TrimSpace(part)
		if name == "" {
			return nil, paramErrf("columns", "empty column name")
		}
		if _, ok := declared[name]; !ok {
			return nil, paramErrf("columns", "unknown column %q of %s.%s", name, shape.Schema, shape.Table)
		}
		if _, dup := seen[name]; dup {
			return nil, paramErrf("columns", "repeated column %q", name)
		}
		seen[name] = struct{}{}
		out = append(out, name)
	}
	for _, k := range shape.PrimaryKey {
		if _, ok := seen[k]; !ok {
			return nil, paramErrf("columns", "projection must include primary key column %q (the paging key)", k)
		}
	}
	return out, nil
}
