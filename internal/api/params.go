package api

import (
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/MateusAMP2119/iris-engine-cli/internal/declare"
)

// This file is the request-side half of the read-API wire contract: the
// eq/range filter grammar, exact-field filtering on collection routes, the
// fixed cursor-key roster per collection, keyset-cursor pagination (ascending
// by key, with the id-keyed before= reverse cursor), the limit default and cap,
// and strict 400 handling for unknown, repeated, or unparseable params. It is
// pure grammar: it turns a route's compiled shape plus a request's raw query
// values into a validated QueryPlan (filter predicates + a keyset cursor plan),
// or a ParamError naming the offending param. It never assembles SQL, opens a
// connection, or touches a server; the route layer (/q in endpoint.go, /data in
// dataroute.go) surfaces a ParamError as a 400 bad_param and executes the plan
// against the shared read pool.
//
// It lives in the api layer (not a declare leaf) because it is a read-API concern
// that composes declare's CompiledEndpoint binding plan downward: api outranks
// declare, so the import flows one direction, and no arch roster entry is needed.

// ParamError is a wire-grammar rejection: a query param the grammar refuses. It
// names the offending param so the route layer can surface a 400 bad_param
// naming it (a bad, unknown, repeated, or unparseable param is a 400, never
// silently ignored or clamped). Reason is the human-readable detail.
type ParamError struct {
	// Param is the offending query-param name.
	Param string
	// Reason is the human-readable detail (the message half of the error envelope).
	Reason string
}

// Error renders the param and reason. The route layer reads Param for the
// bad_param code's context; the string form is for logs and tests.
func (e *ParamError) Error() string {
	return fmt.Sprintf("param %q: %s", e.Param, e.Reason)
}

// paramErrf builds a ParamError naming param with a formatted reason.
func paramErrf(param, format string, args ...any) *ParamError {
	return &ParamError{Param: param, Reason: fmt.Sprintf(format, args...)}
}

// Limit bounds: a request omitting limit gets DefaultLimit; an explicit limit
// above MaxLimit is a 400, never a silent clamp.
const (
	// DefaultLimit is the page size a request that omits limit receives.
	DefaultLimit = 100
	// MaxLimit is the largest limit a request may ask for; over-cap is a 400.
	MaxLimit = 1000
)

// Collection identifies a fixed engine-state collection route whose cursor key
// is pinned by the roster. The data (/data) and endpoint (/q) routes carry
// per-source keys instead and are keyed via DataCursorKey and
// EndpointCursorKey.
type Collection string

// The fixed engine-state collections and their roster cursor keys.
const (
	// CollectionRuns is /runs, keyed by the monotonic run id.
	CollectionRuns Collection = "runs"
	// CollectionDeadLetters is /dead_letters, keyed by the monotonic run id.
	CollectionDeadLetters Collection = "dead_letters"
	// CollectionJournal is the data journal, keyed by its monotonic id.
	CollectionJournal Collection = "journal"
	// CollectionPipelines is /pipelines, keyed by name.
	CollectionPipelines Collection = "pipelines"
	// CollectionLanes is /lanes, keyed by the (lane, pos) pair.
	CollectionLanes Collection = "lanes"
)

// CursorKey is a collection's fixed keyset-pagination key: the ordered key
// column(s) that define its ascending order, and whether the key is a single
// monotonic id. Only an id-keyed collection admits the before= reverse cursor
// (a newest-first log page); every other collection pages ascending only (keys,
// never clocks, define order).
type CursorKey struct {
	// Columns are the ordered key columns; ascending keyset order compares them
	// lexicographically. A monotonic id is one column; lanes is (lane, pos).
	Columns []string
	// IDKeyed reports whether the key is a single monotonic id, the one class of
	// collection that also accepts before= (a reverse cursor, still a key).
	IDKeyed bool
}

// single reports the key's sole column when it has exactly one, so the cursor
// grammar can parse an after/before value against that column's type. A composite
// key (lanes) has no single-value cursor.
func (k CursorKey) single() (string, bool) {
	if len(k.Columns) == 1 {
		return k.Columns[0], true
	}
	return "", false
}

// CollectionKey returns the fixed cursor key for a fixed engine-state
// collection (the roster): a monotonic id for runs, dead_letters, and the
// journal; name for pipelines; the (lane, pos) pair for lanes. ok is false for
// a collection outside the roster. It is a pure switch, not a mutable table, so
// the roster cannot drift at runtime.
func CollectionKey(c Collection) (key CursorKey, ok bool) {
	switch c {
	case CollectionRuns:
		return CursorKey{Columns: []string{"id"}, IDKeyed: true}, true
	case CollectionDeadLetters:
		return CursorKey{Columns: []string{"run_id"}, IDKeyed: true}, true
	case CollectionJournal:
		return CursorKey{Columns: []string{"id"}, IDKeyed: true}, true
	case CollectionPipelines:
		return CursorKey{Columns: []string{"name"}, IDKeyed: false}, true
	case CollectionLanes:
		return CursorKey{Columns: []string{"lane", "pos"}, IDKeyed: false}, true
	default:
		return CursorKey{}, false
	}
}

// DataCursorKey returns the /data route's cursor key: the source table's primary
// key columns, ascending only. A table PK is not a monotonic engine id (it may be
// a uuid), so /data takes no before= reverse cursor.
func DataCursorKey(primaryKey []string) CursorKey {
	return CursorKey{Columns: append([]string(nil), primaryKey...), IDKeyed: false}
}

// EndpointCursorKey returns a /q endpoint's cursor key: its declared sort column,
// ascending only. The sort is a unique source column (validated at compile), not a
// monotonic engine id, so /q takes no before= reverse cursor.
func EndpointCursorKey(sort string) CursorKey {
	return CursorKey{Columns: []string{sort}, IDKeyed: false}
}

// PredicateOp is a filter or cursor comparison operator: exact equality, an
// inclusive range bound, or a strict cursor bound. The set is closed; no other
// operator (LIKE, IN, <>) is ever a wire predicate.
type PredicateOp string

// The closed predicate operator set.
const (
	// OpEq is exact equality (an eq filter or a collection field filter).
	OpEq PredicateOp = "="
	// OpGte is an inclusive lower range bound (<param>_from).
	OpGte PredicateOp = ">="
	// OpLte is an inclusive upper range bound (<param>_to).
	OpLte PredicateOp = "<="
	// OpGt is the ascending keyset cursor bound (key > after).
	OpGt PredicateOp = ">"
	// OpLt is the descending keyset cursor bound (key < before).
	OpLt PredicateOp = "<"
)

// Predicate is one resolved filter the request binds: a source column, its
// comparison operator, and the value already parsed per the column's type. It is
// the grammar's output; the read layer renders it as a bound parameter, never as
// caller SQL.
type Predicate struct {
	// Column is the source column the predicate compares.
	Column string
	// Op is the comparison operator.
	Op PredicateOp
	// Value is the parsed request value (typed per the column's wire type).
	Value any
}

// CursorBound is the keyset cursor comparison a page applies: a strict operator
// (OpGt for an ascending after cursor, OpLt for a descending before cursor) and
// the parsed cursor value. A nil bound means the first page (no cursor).
type CursorBound struct {
	// Op is the strict cursor operator (OpGt ascending, OpLt descending).
	Op PredicateOp
	// Value is the parsed after/before key value (parsed per the key column's type).
	Value any
}

// CursorPlan is the keyset-pagination plan for one request: the collection's
// key, the page direction, the optional cursor bound, and the row limit. There
// is never an offset or a timestamp bound; the key alone defines the order.
type CursorPlan struct {
	// Key is the collection's fixed cursor key.
	Key CursorKey
	// Descending is true only for the id-keyed before= reverse cursor (a
	// newest-first log page); every other page is ascending by the key.
	Descending bool
	// Bound is the cursor comparison the page applies, or nil for the first page.
	Bound *CursorBound
	// Limit is the resolved page size (default 100, cap 1000).
	Limit int
}

// NextAfter returns the continuation cursor for a served page (the envelope's
// page.next_after): the key value of the last served row when the page filled to
// the limit, or nil when it did not (a short page has no next page). rows are the
// served rows as column->value maps, in served order. For a composite key it
// returns the ordered key tuple ([]any); for a single-column key, the bare value.
// A reverse (before) page continues from its last row's key just the same, passed
// back as before=.
func (p CursorPlan) NextAfter(rows []map[string]any) any {
	if p.Limit <= 0 || len(rows) < p.Limit {
		return nil
	}
	last := rows[len(rows)-1]
	if col, ok := p.Key.single(); ok {
		return last[col]
	}
	tuple := make([]any, len(p.Key.Columns))
	for i, c := range p.Key.Columns {
		tuple[i] = last[c]
	}
	return tuple
}

// QueryPlan is the fully-resolved read plan for one request: the filter predicates
// (in a deterministic order) and the keyset cursor plan. It is what the route layer
// binds into the compiled statement (for /q) or the assembled statement (for
// collection and /data routes); the grammar produces it or a ParamError, never a
// half-validated plan.
type QueryPlan struct {
	// Predicates are the resolved filter predicates.
	Predicates []Predicate
	// Cursor is the keyset-pagination plan.
	Cursor CursorPlan
}

// PlanEndpointQuery resolves a request against a compiled /q endpoint: it
// validates every query param against the endpoint's binding plan (unknown or
// repeated -> 400 naming it), binds each eq filter as an equality predicate and
// each range filter as inclusive from/to bounds (either side omittable), parses
// every value per its source-column type (parse failure -> 400 naming the
// param), resolves the limit (default 100, cap 1000, over-cap -> 400), and
// builds the ascending keyset cursor from after= (a /q endpoint is never
// id-keyed, so it takes no before). It composes declare's compiled ParamSlot
// plan directly: the allowed param set and the per-param type are exactly the
// compiled slots.
func PlanEndpointQuery(ce *declare.CompiledEndpoint, q url.Values) (*QueryPlan, error) {
	if ce == nil {
		return nil, errors.New("api: plan endpoint query: nil compiled endpoint")
	}

	// The compiled binding plan enumerates every legal param, including after and
	// limit; before is deliberately absent (a /q endpoint is not id-keyed).
	allowed := make(map[string]struct{}, len(ce.Params))
	for _, s := range ce.Params {
		allowed[s.Param] = struct{}{}
	}
	if err := checkKnownSingle(q, allowed); err != nil {
		return nil, err
	}

	var (
		preds     []Predicate
		afterType string
	)
	for _, s := range ce.Params {
		switch s.Kind {
		case declare.ParamEq:
			if raw, ok := single(q, s.Param); ok {
				v, err := parseParam(s.Param, s.PgType, raw)
				if err != nil {
					return nil, err
				}
				preds = append(preds, Predicate{Column: s.Column, Op: OpEq, Value: v})
			}
		case declare.ParamRangeFrom:
			if raw, ok := single(q, s.Param); ok {
				v, err := parseParam(s.Param, s.PgType, raw)
				if err != nil {
					return nil, err
				}
				preds = append(preds, Predicate{Column: s.Column, Op: OpGte, Value: v})
			}
		case declare.ParamRangeTo:
			if raw, ok := single(q, s.Param); ok {
				v, err := parseParam(s.Param, s.PgType, raw)
				if err != nil {
					return nil, err
				}
				preds = append(preds, Predicate{Column: s.Column, Op: OpLte, Value: v})
			}
		case declare.ParamAfter:
			afterType = s.PgType
		}
	}

	limit, err := resolveLimit(q)
	if err != nil {
		return nil, err
	}

	cursor := CursorPlan{Key: EndpointCursorKey(ce.Sort), Limit: limit}
	if raw, ok := single(q, "after"); ok {
		v, err := parseParam("after", afterType, raw)
		if err != nil {
			return nil, err
		}
		cursor.Bound = &CursorBound{Op: OpGt, Value: v}
	}

	return &QueryPlan{Predicates: preds, Cursor: cursor}, nil
}

// PlanCollectionQuery resolves a request against a fixed engine-state
// collection: its cursor key comes from the roster, its filterable fields and
// their types come from fields (the route supplies the collection's curated
// column set). Filtering is exact field equality only; there is no range or
// operator grammar on a collection route.
func PlanCollectionQuery(c Collection, fields map[string]string, q url.Values) (*QueryPlan, error) {
	key, ok := CollectionKey(c)
	if !ok {
		return nil, fmt.Errorf("api: plan collection query: unknown collection %q", c)
	}
	return planKeyedQuery(key, fields, q)
}

// PlanDataQuery resolves a request against the raw /data route (/data is column
// projection plus eq/range filters keyset-paged by table PK; the filtering and
// paging grammar lives here, projection in the route layer). It is keyed by the
// source table's primary key, and every declared column takes the full filter
// grammar: <col>= binds exact equality and <col>_from / <col>_to bind an
// inclusive range, either side omittable -- the same eq/range wire grammar a /q
// endpoint declares, ad hoc. A declared column whose name collides with another
// column's range param keeps its own name (the declared column always wins).
// primaryKey is the table's PK columns; fields is the table's filterable
// columns and their types.
func PlanDataQuery(primaryKey []string, fields map[string]string, q url.Values) (*QueryPlan, error) {
	type rangeRef struct {
		column string
		op     PredicateOp
	}
	allowed := map[string]struct{}{"after": {}, "limit": {}}
	ranges := make(map[string]rangeRef)
	for f := range fields {
		allowed[f] = struct{}{}
	}
	for f := range fields {
		for _, r := range []struct {
			suffix string
			op     PredicateOp
		}{{"_from", OpGte}, {"_to", OpLte}} {
			p := f + r.suffix
			if _, isColumn := fields[p]; isColumn {
				continue // the declared column name wins the param
			}
			allowed[p] = struct{}{}
			ranges[p] = rangeRef{column: f, op: r.op}
		}
	}
	if err := checkKnownSingle(q, allowed); err != nil {
		return nil, err
	}

	// Equality predicates in sorted column order, then range predicates in
	// sorted param order, so the plan is deterministic regardless of query
	// iteration order.
	var preds []Predicate
	for _, f := range presentFields(fields, q) {
		raw, _ := single(q, f)
		v, err := parseParam(f, fields[f], raw)
		if err != nil {
			return nil, err
		}
		preds = append(preds, Predicate{Column: f, Op: OpEq, Value: v})
	}
	var present []string
	for p := range ranges {
		if _, ok := q[p]; ok {
			present = append(present, p)
		}
	}
	sort.Strings(present)
	for _, p := range present {
		raw, _ := single(q, p)
		ref := ranges[p]
		v, err := parseParam(p, fields[ref.column], raw)
		if err != nil {
			return nil, err
		}
		preds = append(preds, Predicate{Column: ref.column, Op: ref.op, Value: v})
	}

	limit, err := resolveLimit(q)
	if err != nil {
		return nil, err
	}
	cursor, err := planCursor(DataCursorKey(primaryKey), fields, q, limit)
	if err != nil {
		return nil, err
	}
	return &QueryPlan{Predicates: preds, Cursor: cursor}, nil
}

// planKeyedQuery is the shared grammar for the exact-equality routes (fixed
// collections and /data): a fixed cursor key, a known-field type map, exact-field
// filtering, keyset paging (before= only when the key is id-keyed), and the limit
// rule. It validates unknown/repeated params first, then binds field equality
// predicates in a deterministic order, then the limit, then the cursor.
func planKeyedQuery(key CursorKey, fields map[string]string, q url.Values) (*QueryPlan, error) {
	allowed := map[string]struct{}{"after": {}, "limit": {}}
	if key.IDKeyed {
		allowed["before"] = struct{}{}
	}
	for f := range fields {
		allowed[f] = struct{}{}
	}
	if err := checkKnownSingle(q, allowed); err != nil {
		return nil, err
	}

	// Exact field-equality predicates, in sorted field order for determinism.
	var preds []Predicate
	for _, f := range presentFields(fields, q) {
		raw, _ := single(q, f)
		v, err := parseParam(f, fields[f], raw)
		if err != nil {
			return nil, err
		}
		preds = append(preds, Predicate{Column: f, Op: OpEq, Value: v})
	}

	limit, err := resolveLimit(q)
	if err != nil {
		return nil, err
	}

	cursor, err := planCursor(key, fields, q, limit)
	if err != nil {
		return nil, err
	}
	return &QueryPlan{Predicates: preds, Cursor: cursor}, nil
}

// planCursor builds the keyset cursor for an exact-equality route from after= /
// before=: ascending key > after by default; the id-keyed reverse cursor
// descending key < before for newest-first pages. after and before are mutually
// exclusive (a page is forward or reverse, never both). The cursor value parses
// per the key column's type, like any param. A composite key takes no single-value
// cursor.
func planCursor(key CursorKey, fields map[string]string, q url.Values, limit int) (CursorPlan, error) {
	cursor := CursorPlan{Key: key, Limit: limit}
	afterRaw, hasAfter := single(q, "after")
	beforeRaw, hasBefore := single(q, "before")
	if hasAfter && hasBefore {
		return CursorPlan{}, paramErrf("before", "after and before are mutually exclusive; a page is forward or reverse, not both")
	}
	switch {
	case hasAfter:
		v, err := parseCursorValue(key, fields, "after", afterRaw)
		if err != nil {
			return CursorPlan{}, err
		}
		cursor.Bound = &CursorBound{Op: OpGt, Value: v}
	case hasBefore:
		v, err := parseCursorValue(key, fields, "before", beforeRaw)
		if err != nil {
			return CursorPlan{}, err
		}
		cursor.Descending = true
		cursor.Bound = &CursorBound{Op: OpLt, Value: v}
	}
	return cursor, nil
}

// parseCursorValue parses a cursor bound (after or before) against the key
// column's type. A composite-key collection (lanes) has no single-value cursor, so
// a cursor param on it is a 400 naming the param.
func parseCursorValue(key CursorKey, fields map[string]string, param, raw string) (any, error) {
	col, ok := key.single()
	if !ok {
		return nil, paramErrf(param, "collection keyed by (%s) takes no single-value cursor", strings.Join(key.Columns, ", "))
	}
	pgType, ok := fields[col]
	if !ok {
		// The route must expose the key column in fields; a missing type is a route
		// misconfiguration, not a client error.
		return nil, fmt.Errorf("api: cursor key column %q has no known type", col)
	}
	return parseParam(param, pgType, raw)
}

// checkKnownSingle rejects any query param not in allowed (unknown) or carrying
// more than one value (repeated), naming the first offender in sorted order so
// the error is deterministic (unknown/repeated -> 400, never silently ignored).
func checkKnownSingle(q url.Values, allowed map[string]struct{}) error {
	keys := make([]string, 0, len(q))
	for k := range q {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		if _, ok := allowed[k]; !ok {
			return paramErrf(k, "unknown parameter")
		}
		if len(q[k]) > 1 {
			return paramErrf(k, "repeated parameter")
		}
	}
	return nil
}

// presentFields returns the field params present in q, in sorted order, so the
// predicate list is deterministic regardless of query iteration order.
func presentFields(fields map[string]string, q url.Values) []string {
	var out []string
	for f := range fields {
		if _, ok := q[f]; ok {
			out = append(out, f)
		}
	}
	sort.Strings(out)
	return out
}

// single returns a param's sole value and whether it is present. Repetition has
// already been rejected by checkKnownSingle, so the first value is the value.
func single(q url.Values, key string) (string, bool) {
	vs, ok := q[key]
	if !ok || len(vs) == 0 {
		return "", false
	}
	return vs[0], true
}

// resolveLimit resolves the row limit: absent -> DefaultLimit; present ->
// parsed, required positive, capped at MaxLimit with an over-cap value a 400
// (never a silent clamp). Every failure is a ParamError naming limit.
func resolveLimit(q url.Values) (int, error) {
	raw, ok := single(q, "limit")
	if !ok {
		return DefaultLimit, nil
	}
	n, err := strconv.Atoi(strings.TrimSpace(raw))
	if err != nil {
		return 0, paramErrf("limit", "must be an integer")
	}
	if n < 1 {
		return 0, paramErrf("limit", "must be a positive integer")
	}
	if n > MaxLimit {
		return 0, paramErrf("limit", "exceeds the maximum of %d", MaxLimit)
	}
	return n, nil
}

// parseParam parses raw per pgType, wrapping any failure as a ParamError naming
// param (a value that fails to parse per its source-column type -> 400 naming
// the param).
func parseParam(param, pgType, raw string) (any, error) {
	v, err := parseValue(pgType, raw)
	if err != nil {
		return nil, paramErrf(param, "%v", err)
	}
	return v, nil
}

// numericLiteralRe matches a Postgres numeric literal: an optionally signed
// decimal with optional fraction and optional scientific exponent. It validates a
// numeric param's shape without a float round-trip that would lose precision.
var numericLiteralRe = regexp.MustCompile(`^[+-]?(\d+(\.\d*)?|\.\d+)([eE][+-]?\d+)?$`)

// uuidRe matches the canonical 8-4-4-4-12 hexadecimal uuid form.
var uuidRe = regexp.MustCompile(`^[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}$`)

// baseType strips a Postgres type's parametrization and normalizes case, so
// varchar(255) parses as varchar and numeric(10,2) as numeric. Types without
// parentheses (double precision, timestamptz) pass through unchanged.
func baseType(pgType string) string {
	t := strings.ToLower(strings.TrimSpace(pgType))
	if i := strings.IndexByte(t, '('); i >= 0 {
		t = strings.TrimSpace(t[:i])
	}
	return t
}

// parseValue validates raw against the closed source-column type set that is
// the wire grammar (the declared type mapping is the wire grammar) and returns
// the parsed value. A value that does not parse returns an error naming the
// type; the caller wraps it as a ParamError naming the param. text and varchar
// accept any string; every other type is validated. The returned value is bound
// as a positional param by the read layer, never interpolated.
func parseValue(pgType, raw string) (any, error) {
	switch baseType(pgType) {
	case "smallint":
		return strconv.ParseInt(raw, 10, 16)
	case "int", "integer":
		return strconv.ParseInt(raw, 10, 32)
	case "bigint":
		return strconv.ParseInt(raw, 10, 64)
	case "double precision":
		return strconv.ParseFloat(raw, 64)
	case "numeric":
		if !numericLiteralRe.MatchString(raw) {
			return nil, fmt.Errorf("value %q is not a valid numeric", raw)
		}
		return raw, nil // kept as a string: no float round-trip loss.
	case "boolean":
		return strconv.ParseBool(raw)
	case "text", "varchar":
		return raw, nil
	case "uuid":
		if !uuidRe.MatchString(raw) {
			return nil, fmt.Errorf("value %q is not a valid uuid", raw)
		}
		return raw, nil
	case "timestamptz", "timestamp":
		return parseTimestamp(raw)
	case "date":
		return time.Parse("2006-01-02", raw)
	case "time":
		return parseTimeOfDay(raw)
	case "json", "jsonb":
		if !json.Valid([]byte(raw)) {
			return nil, fmt.Errorf("value is not valid json")
		}
		return json.RawMessage(raw), nil
	case "bytea":
		b, err := base64.StdEncoding.DecodeString(raw)
		if err != nil {
			return nil, fmt.Errorf("value is not valid base64: %w", err)
		}
		return b, nil
	default:
		return nil, fmt.Errorf("unsupported column type %q", pgType)
	}
}

// timestampLayouts are the RFC 3339 timestamp forms the wire accepts, offset
// and offset-free (timestamptz/timestamp are RFC 3339 strings). The offset-free
// forms cover a plain timestamp column.
var timestampLayouts = []string{
	time.RFC3339Nano,
	time.RFC3339,
	"2006-01-02T15:04:05.999999999",
	"2006-01-02T15:04:05",
}

// parseTimestamp parses an RFC 3339 timestamp, trying each accepted layout.
func parseTimestamp(raw string) (time.Time, error) {
	for _, layout := range timestampLayouts {
		if ts, err := time.Parse(layout, raw); err == nil {
			return ts, nil
		}
	}
	return time.Time{}, fmt.Errorf("value %q is not an RFC 3339 timestamp", raw)
}

// timeLayouts are the time-of-day forms the wire accepts (time is an RFC 3339
// string).
var timeLayouts = []string{
	"15:04:05.999999999",
	"15:04:05",
	"15:04",
}

// parseTimeOfDay parses a time-of-day value, trying each accepted layout.
func parseTimeOfDay(raw string) (time.Time, error) {
	for _, layout := range timeLayouts {
		if ts, err := time.Parse(layout, raw); err == nil {
			return ts, nil
		}
	}
	return time.Time{}, fmt.Errorf("value %q is not a valid time", raw)
}
