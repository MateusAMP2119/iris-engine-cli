package declare

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"

	"github.com/goccy/go-yaml"
)

// This file is the endpoint compile leaf: a
// declared read endpoint is one flat, shape-only YAML file at
// endpoints/<name>.yaml (the sole folder-per-unit exception). It parses that
// shape, validates the single-table projection and its filter/sort rules against
// the schemas/ set, refuses the journal as a source, and derives exactly one
// parameterized SQL text deterministically. Persisting the compiled result to the
// endpoints and endpoint_filters meta tables, prepare-verifying it against the
// data database, and serving /q/{endpoint} all belong to later tasks; this leaf
// owns the pure compile: bytes and a validated source table in, one deterministic
// statement out.

// endpointsDirName is the canonical top-level directory holding the flat endpoint
// files.
const endpointsDirName = "endpoints"

// endpointFileExt is the required extension of an endpoint file: a declared read
// endpoint is a YAML document.
const endpointFileExt = ".yaml"

// FilterOp is an endpoint filter's kind: the closed set the wire grammar admits.
// A filter is either an equality match or a bounded range; nothing else is a
// legal filter.
type FilterOp string

// The two endpoint filter kinds.
const (
	// FilterEq is an equality filter: <param>= binds one value.
	FilterEq FilterOp = "eq"
	// FilterRange is a bounded-range filter: <param>_from/<param>_to, either side
	// optional, bounds inclusive.
	FilterRange FilterOp = "range"
)

// Filter is one endpoint filter param: the source column it matches and its kind.
// The param name is the source column, so the wire param (<param>=, or
// <param>_from/<param>_to) and the SQL predicate share one identifier.
type Filter struct {
	// Param is the filter's query-param name, which is the source column it filters.
	Param string
	// Op is the filter kind (eq or range).
	Op FilterOp
}

// filterList is an ordered endpoint filter set that decodes from a YAML map while
// preserving the author's declaration order: a YAML map decodes into a Go map
// with no defined iteration order, which would make the derived SQL
// non-deterministic, so filters decode through goccy's order-preserving MapSlice
// instead.
type filterList []Filter

// UnmarshalYAML decodes the filters map into an ordered filter list, preserving
// declaration order via MapSlice. Op validity (eq or range) is enforced by
// ParseEndpoint, not here, so a malformed op is reported with full file context.
func (fl *filterList) UnmarshalYAML(b []byte) error {
	var ms yaml.MapSlice
	if err := yaml.Unmarshal(b, &ms); err != nil {
		return fmt.Errorf("filters must be a map of param to filter kind: %w", err)
	}
	out := make(filterList, 0, len(ms))
	for _, item := range ms {
		param, ok := item.Key.(string)
		if !ok {
			return fmt.Errorf("filter param %v is not a string", item.Key)
		}
		op, ok := item.Value.(string)
		if !ok {
			return fmt.Errorf("filter %q kind %v is not a string", param, item.Value)
		}
		out = append(out, Filter{Param: param, Op: FilterOp(op)})
	}
	*fl = out
	return nil
}

// Endpoint is a parsed endpoint file: the flat, shape-only read surface. It is
// single-table: one source, an explicit flat field projection, filter params
// (each eq or range), and a keyset-pagination sort key.
type Endpoint struct {
	// Name is the endpoint name; it must equal the file's basename.
	Name string `yaml:"endpoint"`
	// Source is the single dotted schema.table the endpoint reads.
	Source string `yaml:"source"`
	// Fields is the explicit field projection: a flat list of source columns.
	Fields []string `yaml:"fields"`
	// Filters are the endpoint's filter params, in declaration order.
	Filters filterList `yaml:"filters"`
	// Sort is the keyset-pagination key: a unique source column.
	Sort string `yaml:"sort"`
}

// endpointFields is the whitelist of keys a flat endpoint file may carry. Any
// other key -- a join, a group_by/aggregate, a having -- is rejected: an endpoint
// is single-table with an explicit projection, never a query builder.
var endpointFields = map[string]bool{
	"endpoint": true, "source": true, "fields": true, "filters": true, "sort": true,
}

// endpointFieldList is the human-readable rendering of endpointFields, for errors.
const endpointFieldList = "endpoint, source, fields, filters, sort"

// identRe matches a bare SQL identifier: a projected field, a filter param, a sort
// key, and each dotted part of a source name must match it. This rejects computed
// or aggregated fields (sum(amount), amount * 2) structurally at parse and keeps
// every identifier the derived SQL interpolates injection-safe.
var identRe = regexp.MustCompile(`^[a-zA-Z_][a-zA-Z0-9_]*$`)

// ParseEndpoint parses an endpoint file's bytes into an Endpoint and validates its
// flat, single-table shape: the field whitelist (naming
// any offending key, so a join or aggregation is refused), the required fields
// (endpoint, source, fields, sort), a dotted schema.table source, an eq-or-range
// kind on every filter, and a bare-identifier projection (no computed fields). It
// is pure over bytes; source resolution against schemas/ is CompileEndpoint's.
func ParseEndpoint(data []byte) (*Endpoint, error) {
	raw := map[string]any{}
	if err := yaml.Unmarshal(data, &raw); err != nil {
		return nil, fmt.Errorf("declare: parse endpoint: %w", err)
	}
	if len(raw) == 0 {
		return nil, errors.New("declare: parse endpoint: empty document; an endpoint needs endpoint, source, fields, sort")
	}
	if err := checkKeys(raw, endpointFields, endpointFieldList); err != nil {
		return nil, fmt.Errorf("declare: parse endpoint: %w", err)
	}

	var ep Endpoint
	if err := yaml.Unmarshal(data, &ep); err != nil {
		return nil, fmt.Errorf("declare: parse endpoint: %w", err)
	}
	if err := ep.validateShape(); err != nil {
		return nil, err
	}
	return &ep, nil
}

// validateShape enforces the required-field, identifier, and filter-kind rules on
// a decoded endpoint, independent of any source table.
func (ep *Endpoint) validateShape() error {
	name := strings.TrimSpace(ep.Name)
	if name == "" {
		return errors.New("declare: parse endpoint: missing required field \"endpoint\" (the endpoint name)")
	}
	if !identRe.MatchString(name) {
		return fmt.Errorf("declare: parse endpoint %q: name is not a bare identifier", name)
	}
	if strings.TrimSpace(ep.Source) == "" {
		return fmt.Errorf("declare: parse endpoint %q: missing required field \"source\" (the dotted schema.table)", name)
	}
	if schema, table, ok := splitDottedTable(ep.Source); !ok || !identRe.MatchString(schema) || !identRe.MatchString(table) {
		return fmt.Errorf("declare: parse endpoint %q: source %q is not a dotted schema.table of two bare identifiers", name, ep.Source)
	}
	if len(ep.Fields) == 0 {
		return fmt.Errorf("declare: parse endpoint %q: fields is required and must be non-empty; the projection is an explicit column list", name)
	}
	for _, f := range ep.Fields {
		if !identRe.MatchString(f) {
			return fmt.Errorf("declare: parse endpoint %q: field %q is not a bare column identifier; a projection has no computed or aggregated fields", name, f)
		}
	}
	for _, flt := range ep.Filters {
		if !identRe.MatchString(flt.Param) {
			return fmt.Errorf("declare: parse endpoint %q: filter param %q is not a bare column identifier", name, flt.Param)
		}
		if flt.Op != FilterEq && flt.Op != FilterRange {
			return fmt.Errorf("declare: parse endpoint %q: filter %q kind %q is not one of eq, range", name, flt.Param, flt.Op)
		}
	}
	if strings.TrimSpace(ep.Sort) == "" {
		return fmt.Errorf("declare: parse endpoint %q: missing required field \"sort\" (the keyset-pagination column)", name)
	}
	if !identRe.MatchString(ep.Sort) {
		return fmt.Errorf("declare: parse endpoint %q: sort %q is not a bare column identifier", name, ep.Sort)
	}
	return nil
}

// DiscoveredEndpoint is one flat endpoint file found under a workspace's
// endpoints/ directory: its name (from the file's basename, authoritative), its
// path, and the parsed shape.
type DiscoveredEndpoint struct {
	// Name is the endpoint name, taken from the file basename (authoritative) and
	// verified equal to the parsed endpoint: field.
	Name string
	// Path is the endpoint file path.
	Path string
	// Spec is the parsed endpoint.
	Spec *Endpoint
}

// LoadEndpointFile reads and parses one endpoint file, verifying it is a .yaml
// file whose basename equals its endpoint: field (filename = endpoint field). A
// basename disagreement is rejected naming both, so a misfiled endpoint never
// applies under the wrong name.
func LoadEndpointFile(path string) (*DiscoveredEndpoint, error) {
	base := filepath.Base(path)
	ext := filepath.Ext(base)
	if ext != endpointFileExt {
		return nil, fmt.Errorf("declare: endpoint file %s: an endpoint is a %s file", path, endpointFileExt)
	}
	name := strings.TrimSuffix(base, ext)

	data, err := readFile(path)
	if err != nil {
		return nil, fmt.Errorf("declare: read endpoint file %s: %w", path, err)
	}
	ep, err := ParseEndpoint(data)
	if err != nil {
		return nil, err
	}
	if ep.Name != name {
		return nil, fmt.Errorf("declare: endpoint file %s: endpoint: field %q does not match the filename %q; the filename is the endpoint name", path, ep.Name, name)
	}
	return &DiscoveredEndpoint{Name: name, Path: path, Spec: ep}, nil
}

// DiscoverEndpoints walks a workspace's endpoints/ directory and returns its
// declared read endpoints as flat, shape-only YAML files: one file per endpoint
// at endpoints/<name>.yaml, filename = endpoint:
// field. An absent endpoints/ tree yields an empty result rather than an error; a
// subdirectory under endpoints/ is rejected (the flat-file exception owns no
// folders). Results are returned sorted by name for a deterministic order.
func DiscoverEndpoints(root string) ([]DiscoveredEndpoint, error) {
	dir := filepath.Join(root, endpointsDirName)
	entries, err := os.ReadDir(dir)
	switch {
	case errors.Is(err, fs.ErrNotExist):
		return nil, nil
	case err != nil:
		return nil, fmt.Errorf("declare: read endpoints directory %s: %w", dir, err)
	}

	var out []DiscoveredEndpoint
	for _, e := range entries {
		name := e.Name()
		if isHidden(name) {
			continue
		}
		if e.IsDir() {
			return nil, fmt.Errorf("declare: endpoints/%s is a directory; endpoints/ holds one flat %s file per endpoint, no folders", name, endpointFileExt)
		}
		de, err := LoadEndpointFile(filepath.Join(dir, name))
		if err != nil {
			return nil, err
		}
		out = append(out, *de)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out, nil
}

// ParamKind is the role of one bound parameter in a compiled endpoint's SQL: an
// equality match, either bound of a range, the keyset cursor, or the row limit.
type ParamKind string

// The compiled-endpoint parameter kinds.
const (
	// ParamEq binds an equality filter's single value.
	ParamEq ParamKind = "eq"
	// ParamRangeFrom binds a range filter's inclusive lower bound.
	ParamRangeFrom ParamKind = "range_from"
	// ParamRangeTo binds a range filter's inclusive upper bound.
	ParamRangeTo ParamKind = "range_to"
	// ParamAfter binds the keyset-pagination cursor (the after value).
	ParamAfter ParamKind = "after"
	// ParamLimit binds the row limit.
	ParamLimit ParamKind = "limit"
)

// ParamSlot is one positional bound parameter of a compiled endpoint's SQL: its
// 1-based index, the query param it binds, its kind, the source column it
// compares (empty for the limit), and that column's Postgres type. The read path
// binds request values by these slots; it never assembles SQL.
type ParamSlot struct {
	// Index is the 1-based positional parameter number ($Index in the SQL).
	Index int
	// Param is the query-param base name this slot binds (empty for the limit).
	Param string
	// Kind is the slot's role.
	Kind ParamKind
	// Column is the source column the slot compares (empty for the limit).
	Column string
	// PgType is the column's Postgres type, the cast the SQL applies.
	PgType string
}

// CompiledEndpoint is the deterministic result of compiling an endpoint against
// its source table: the resolved source, the projection and filter/sort shape,
// exactly one parameterized SQL text, and the ordered parameter-binding plan the
// read path binds by.
type CompiledEndpoint struct {
	// Name is the endpoint name.
	Name string
	// Schema and Table are the resolved source's dotted parts.
	Schema string
	Table  string
	// Fields is the validated projection, in declaration order.
	Fields []string
	// Filters is the validated filter set, in declaration order.
	Filters []Filter
	// Sort is the validated unique keyset-pagination column.
	Sort string
	// SQL is the single parameterized statement, derived deterministically.
	SQL string
	// Params is the ordered parameter-binding plan, one slot per $n in SQL.
	Params []ParamSlot
}

// TableIndex builds the "schema.table" -> *Table lookup CompileEndpoint resolves an
// endpoint's source against, from a discovered schemas/ tree.
func TableIndex(tables []DiscoveredTable) map[string]*Table {
	idx := make(map[string]*Table, len(tables))
	for i := range tables {
		t := tables[i]
		idx[t.Schema+"."+t.Table] = t.Spec
	}
	return idx
}

// CompileEndpoint validates a parsed endpoint against the declared schemas/ set
// and derives its one deterministic parameterized SQL text.
// It resolves the single dotted source against tables (keyed schema.table),
// refusing the journal and the reserved public schema; requires every projected
// field and filter param to be a source column; requires sort to be a unique
// source column (keyset pagination); and emits one SELECT whose filter, keyset,
// and limit params are bound positionally. The output is a pure function of the
// endpoint and the source table: identical input yields byte-identical SQL.
func CompileEndpoint(ep *Endpoint, tables map[string]*Table) (*CompiledEndpoint, error) {
	if ep == nil {
		return nil, errors.New("declare: compile endpoint: nil endpoint")
	}
	if err := ep.validateShape(); err != nil {
		return nil, err
	}

	schema, table, ok := splitDottedTable(ep.Source)
	if !ok {
		return nil, fmt.Errorf("declare: compile endpoint %q: source %q is not a dotted schema.table", ep.Name, ep.Source)
	}
	if schema == journalSchema && table == journalTableName {
		return nil, fmt.Errorf("declare: compile endpoint %q: source %s.%s is the data journal, which is never an endpoint source", ep.Name, schema, table)
	}
	if schema == publicSchema {
		return nil, fmt.Errorf("declare: compile endpoint %q: source schema %q is engine-reserved and is never an endpoint source", ep.Name, publicSchema)
	}

	src, ok := tables[ep.Source]
	if !ok || src == nil {
		return nil, fmt.Errorf("declare: compile endpoint %q: source %q is not a declared table under schemas/", ep.Name, ep.Source)
	}

	cols := make(map[string]Column, len(src.Columns))
	for _, c := range src.Columns {
		cols[c.Name] = c
	}

	for _, f := range ep.Fields {
		if _, ok := cols[f]; !ok {
			return nil, fmt.Errorf("declare: compile endpoint %q: field %q is not a column of source %s", ep.Name, f, ep.Source)
		}
	}
	for _, flt := range ep.Filters {
		if _, ok := cols[flt.Param]; !ok {
			return nil, fmt.Errorf("declare: compile endpoint %q: filter param %q is not a column of source %s", ep.Name, flt.Param, ep.Source)
		}
	}
	sortCol, ok := cols[ep.Sort]
	if !ok {
		return nil, fmt.Errorf("declare: compile endpoint %q: sort %q is not a column of source %s", ep.Name, ep.Sort, ep.Source)
	}
	if !sortCol.PrimaryKey && !sortCol.Unique {
		return nil, fmt.Errorf("declare: compile endpoint %q: sort %q must be a unique source column; keyset pagination needs a unique key", ep.Name, ep.Sort)
	}

	sql, params, err := buildEndpointSQL(schema, table, ep, cols)
	if err != nil {
		return nil, fmt.Errorf("declare: compile endpoint %q: %w", ep.Name, err)
	}

	return &CompiledEndpoint{
		Name:    ep.Name,
		Schema:  schema,
		Table:   table,
		Fields:  append([]string(nil), ep.Fields...),
		Filters: append([]Filter(nil), ep.Filters...),
		Sort:    ep.Sort,
		SQL:     sql,
		Params:  params,
	}, nil
}

// buildEndpointSQL renders the one parameterized statement and its ordered
// parameter plan. Every identifier is a validated bare column, schema, or table
// name (never caller SQL), and every value is a positional bound param cast to its
// source column's Postgres type, so a request binds values but can never widen the
// shape. Filters keep declaration order; each eq binds one param, each range binds
// two (inclusive from/to, either omissible via a NULL bound), then the keyset
// cursor and the limit.
func buildEndpointSQL(schema, table string, ep *Endpoint, cols map[string]Column) (string, []ParamSlot, error) {
	pgType := func(name string) (string, error) {
		pt, err := ResolveColumnType(cols[name])
		if err != nil {
			return "", fmt.Errorf("column %q: %w", name, err)
		}
		return pt, nil
	}

	var (
		clauses []string
		params  []ParamSlot
		idx     = 1
	)
	// optional renders an optionally-bound predicate: a NULL param means the caller
	// omitted this bound, so the clause matches every row.
	optional := func(col, pt, op string) string {
		p := fmt.Sprintf("$%d::%s", idx, pt)
		return fmt.Sprintf("(%s IS NULL OR %s %s %s)", p, col, op, p)
	}

	for _, flt := range ep.Filters {
		pt, err := pgType(flt.Param)
		if err != nil {
			return "", nil, err
		}
		switch flt.Op {
		case FilterEq:
			clauses = append(clauses, optional(flt.Param, pt, "="))
			params = append(params, ParamSlot{Index: idx, Param: flt.Param, Kind: ParamEq, Column: flt.Param, PgType: pt})
			idx++
		case FilterRange:
			clauses = append(clauses, optional(flt.Param, pt, ">="))
			params = append(params, ParamSlot{Index: idx, Param: flt.Param + "_from", Kind: ParamRangeFrom, Column: flt.Param, PgType: pt})
			idx++
			clauses = append(clauses, optional(flt.Param, pt, "<="))
			params = append(params, ParamSlot{Index: idx, Param: flt.Param + "_to", Kind: ParamRangeTo, Column: flt.Param, PgType: pt})
			idx++
		default:
			return "", nil, fmt.Errorf("filter %q has kind %q, not eq or range", flt.Param, flt.Op)
		}
	}

	// Keyset cursor: ascending by the unique sort column, the after bound optional.
	sortType, err := pgType(ep.Sort)
	if err != nil {
		return "", nil, err
	}
	clauses = append(clauses, optional(ep.Sort, sortType, ">"))
	params = append(params, ParamSlot{Index: idx, Param: "after", Kind: ParamAfter, Column: ep.Sort, PgType: sortType})
	idx++

	limitSlot := ParamSlot{Index: idx, Param: "limit", Kind: ParamLimit}

	var b strings.Builder
	fmt.Fprintf(&b, "SELECT %s\n", strings.Join(ep.Fields, ", "))
	fmt.Fprintf(&b, "FROM %s.%s\n", schema, table)
	fmt.Fprintf(&b, "WHERE %s\n", strings.Join(clauses, "\n  AND "))
	fmt.Fprintf(&b, "ORDER BY %s ASC\n", ep.Sort)
	fmt.Fprintf(&b, "LIMIT $%d;", limitSlot.Index)

	params = append(params, limitSlot)
	return b.String(), params, nil
}
